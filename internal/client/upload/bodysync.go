package upload

import (
	"context"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/jssblck/akari/internal/parser"
)

// blobCheckBatch is the most hashes one existence-check request carries. It bounds the
// server's per-request work so a sync with thousands of bodies still hits the check
// endpoint in fixed-size, parallel requests rather than one unbounded query.
const blobCheckBatch = 100

// blobCheckConcurrency caps how many existence-check requests run at once. Checks are
// cheap (a single indexed lookup per hash on the server), so a modest fan-out drains a
// large backlog quickly without flooding the server with connections.
const blobCheckConcurrency = 8

// maxPendingBodyBytes bounds the in-hand small-body bytes the sink holds before forcing
// a flush. A chunk can reference many sizable small bodies, whose stored bytes are kept
// until their existence check decides whether to upload them; this budget caps that held
// memory. A streamed big body contributes nothing here (it is a cheap re-readable file
// span, never resident), so the budget tracks only buffered content. It is a var so a
// test can shrink it to exercise the early-flush path. Reaching it flushes early; the
// bodies upload now and are pinned, harmless even though their transcript chunk lands
// later.
var maxPendingBodyBytes int64 = 32 << 20

// pendingBody is one tool body the transform has lifted and is waiting to ensure is in
// the CAS: its key, its storage content type, the bytes of in-hand stored content it is
// holding (0 for a streamed body), and the ref the uploader re-reads it from when the
// existence check says it is missing.
type pendingBody struct {
	sha         string
	contentType string
	storedLen   int
	ref         bodyRef
}

// registerBody records a lifted body for the next batched existence check instead of
// checking and uploading it inline. The descriptor the sentinel needs (key, length,
// media) is already in hand at the call site, so deferring the round-trip lets the
// transform keep producing chunks while many bodies are checked and uploaded in
// parallel. A body already confirmed present this pass, or already queued, is dropped:
// its first occurrence will ensure it. Hitting the held-bytes budget triggers an early
// flush so memory stays bounded.
func (s *syncSink) registerBody(ctx context.Context, sha, contentType string, ref bodyRef) error {
	if s.present.has(sha) {
		return nil
	}
	if _, ok := s.pendingShas[sha]; ok {
		return nil
	}
	stored := 0
	if ref.haveContent {
		stored = len(ref.stored)
	}
	s.pending = append(s.pending, pendingBody{sha: sha, contentType: contentType, storedLen: stored, ref: ref})
	s.pendingShas[sha] = struct{}{}
	s.pendingBytes += int64(stored)
	if s.pendingBytes >= maxPendingBodyBytes {
		return s.flushPending(ctx)
	}
	return nil
}

// flushPending ensures every queued body is in the CAS: it batches the existence checks
// (up to blobCheckBatch hashes per request, requests in parallel), then uploads the
// missing bodies in parallel under the client's upload limiter. It is called before any
// chunk uploads (so the transcript never references a body the CAS lacks) and once more
// at the end of a pass (so a body lifted from a withheld trailing turn is uploaded the
// tick it is first seen, since the held lines are cached and never re-transformed).
// After it returns, every flushed hash is recorded present so a later occurrence skips
// the round-trip.
func (s *syncSink) flushPending(ctx context.Context) error {
	if len(s.pending) == 0 {
		return nil
	}
	pend := s.pending
	s.pending = nil
	s.pendingBytes = 0
	s.pendingShas = make(map[string]struct{})

	// Collapse duplicate hashes to one ref each, preserving first-seen order so the
	// batches are deterministic.
	order := make([]string, 0, len(pend))
	byID := make(map[string]pendingBody, len(pend))
	for _, p := range pend {
		if _, ok := byID[p.sha]; ok {
			continue
		}
		byID[p.sha] = p
		order = append(order, p.sha)
	}

	missing, err := s.checkMissing(ctx, order)
	if err != nil {
		return err
	}
	if err := s.uploadMissing(ctx, order, byID, missing); err != nil {
		return err
	}

	for _, sha := range order {
		s.present.add(sha)
	}
	return nil
}

// checkMissing reports which of the hashes the server lacks, querying in parallel
// batches of at most blobCheckBatch. The server pins every hash it finds present, so a
// body confirmed present here is held against the sweep until its transcript chunk
// commits, even though this check may run well before that chunk uploads.
func (s *syncSink) checkMissing(ctx context.Context, shas []string) (map[string]bool, error) {
	missing := make(map[string]bool, len(shas))
	if len(shas) == 0 {
		return missing, nil
	}

	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(blobCheckConcurrency)
	for start := 0; start < len(shas); start += blobCheckBatch {
		end := start + blobCheckBatch
		if end > len(shas) {
			end = len(shas)
		}
		batch := shas[start:end]
		g.Go(func() error {
			res, err := s.c.checkBlobs(gctx, batch)
			if err != nil {
				return err
			}
			mu.Lock()
			for sha, isMissing := range res {
				if isMissing {
					missing[sha] = true
				}
			}
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return missing, nil
}

// uploadMissing uploads every missing body in parallel, each upload gated on the
// client's adaptive upload limiter so the live concurrency tracks observed performance.
// The errgroup limit caps the worker goroutines at the limiter's ceiling; the limiter
// itself decides how many of them actually run at any moment and samples each upload's
// latency to retune. The first upload error cancels the rest and fails the flush, which
// fails the sync (as an inline upload error would have).
func (s *syncSink) uploadMissing(ctx context.Context, order []string, byID map[string]pendingBody, missing map[string]bool) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(uploadMaxConcurrency)
	for _, sha := range order {
		if !missing[sha] {
			continue
		}
		p := byID[sha]
		g.Go(func() error {
			slot, err := s.c.uploads.acquire(gctx)
			if err != nil {
				return err
			}
			err = s.c.putBody(gctx, s.enc, p.sha, p.contentType, p.ref)
			slot.release(err)
			return err
		})
	}
	return g.Wait()
}

// bodyDescriptor builds the sentinel descriptor for a lifted body without touching the
// network. It is the synchronous half of emitBody: compute the key (in hand for a small
// line, by streaming the encoder for a big one) so the transform can assemble the
// sentinel immediately, while the existence check and upload are deferred to a batch.
func (s *syncSink) bodyDescriptor(ctx context.Context, ref bodyRef) (parser.Body, string, error) {
	sha := ref.sha
	contentType := ref.contentType
	rawLen := ref.rawLen
	if !ref.haveContent {
		var err error
		sha, contentType, rawLen, err = s.enc.HashStream(ctx, ref.canonicalReader(ctx))
		if err != nil {
			return parser.Body{}, "", err
		}
	}
	return parser.Body{SHA256: sha, Bytes: rawLen, MediaType: ref.media, Kind: ref.kind}, contentType, nil
}
