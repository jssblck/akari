// Package upload drives the ingest protocol from the client side, statelessly.
// The server is authoritative for how many bytes of each file it holds, so the
// client persists nothing: it announces, reconciles against the server's cursor
// and content hash, and streams the gap in newline-terminated chunks.
package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jssblck/akari/internal/casenc"
	"github.com/jssblck/akari/internal/parser"
)

// hardCap bounds the largest single transformed message (one rewritten JSONL
// line) the client will buffer. After the transform lifts tool bodies to the CAS
// a line is small, so this only refuses a truly pathological line; it matches the
// server's maxChunk, bounding the server's worst-case parse memory too. It is a
// var so tests can shrink it to exercise the refusal path.
var hardCap int64 = 128 << 20

// settleWindow is how long a session file must be idle before the client uploads
// its trailing in-progress turn. A Codex turn closes only at the next user line,
// so a session's final turn has no closer; once the file stops growing, the turn
// is done and is flushed. It is a var so tests can force an immediate flush.
var settleWindow = 60 * time.Second

// Action describes what SyncFile did for a file.
type Action string

const (
	ActionUpToDate Action = "uptodate" // server already had the whole file
	ActionUploaded Action = "uploaded" // appended new bytes
	ActionReset    Action = "reset"    // diverged, re-uploaded from zero
)

// Target is everything needed to upload one resolved session file. Kind is the
// session's classification ("remote", "standalone", or "orphaned"). ProjectKey
// is set only for a remote session; for standalone and orphaned sessions the
// server derives the project key from Machine and the local location. LocalRoot,
// set only for a standalone session in a live worktree, is the repo root shared
// by every worktree; the server keys on it so a local-only repo's worktrees
// collapse into one project. When it is empty the server falls back to Cwd.
type Target struct {
	Agent      string
	Path       string
	SourceID   string
	Kind       string
	ProjectKey string
	LocalRoot  string
	GitBranch  string
	Cwd        string
	Machine    string

	// Finalize forces the session's trailing turn to be treated as settled on this
	// sync regardless of how recently the file was written. A Codex session's final
	// turn has no closing user line, so it is normally withheld until the file has
	// been idle for settleWindow (see syncOnce); on an ephemeral host (CI, a cloud
	// sandbox) that idle window never elapses before the host is torn down, so the
	// final turn would never upload. Set by `akari sync --finalize`, an assertion by
	// the caller that every session being synced is terminal.
	Finalize bool
}

// Outcome reports the result of syncing one file.
type Outcome struct {
	Action        Action
	UploadedBytes int64
	StoredBytes   int64
	// SessionID is the server's id for the synced session, learned at announce. It
	// lets SyncFile address the session after the upload (the finalize refresh), and
	// is carried out to the caller for the same reason.
	SessionID int64
}

// Client talks to one akari server with one bearer token.
type Client struct {
	http    *http.Client
	baseURL string
	token   string

	// enc encodes each tool body into the bytes the CAS stores (small bodies
	// verbatim, large bodies zstd) and names its key. It is the single source of the
	// stored-byte encoding for both the upload and the cold-cache prefix digest, so
	// the sentinel a re-sync recomputes matches the one it first uploaded. It bounds
	// concurrent compression to the CPU count, so building keys for many bodies at once
	// (a fleet of files, or a batch of uploads) does not oversubscribe the machine.
	enc *casenc.Encoder

	// uploads bounds how many tool-body uploads run at once, adapting the live width to
	// observed round-trip latency. It is shared across every file this Client syncs, so
	// the cap is a whole-client budget, not per file.
	uploads uploadLimiter

	// files caches the transformed-to-original cursor mapping and open trailing
	// turns. It is guarded by mu; a given path is synced single-flight, so the
	// *fileSync it returns is only ever touched by one sync at a time. Idle entries
	// are evicted by least-recent use so a long-running watcher does not retain every
	// session path it has ever observed.
	mu      sync.Mutex
	files   map[string]*fileSync
	fileUse uint64
}

// fileStateCacheCap bounds the number of idle incremental states retained by a
// long-running Client. Active holders and lock waiters are never evicted, so the
// map may exceed the cap temporarily when more than this many paths are syncing at
// once. It is a variable so tests can exercise eviction with a small cache.
var fileStateCacheCap = 1024

// fileSync maps the server's transformed cursor back to the original file and
// caches an open trailing turn across syncs. It is a cache only: dropping it costs
// a cold prefix reconstruction, never correctness.
//
// Under the client-CAS protocol the bytes the server stores are the TRANSFORMED
// transcript (tool bodies lifted to the CAS, replaced by sentinels), so the
// verified-prefix cache is kept over the transformed stream: base is the count of
// transformed bytes the server holds, prefixHasher is the sha256 of those
// transformed bytes, and origBase is the count of original on-disk bytes that
// transform to them. The client still resumes by the original file (it is what it
// can recompute statelessly), so origBase is where the next transform pass reads
// from.
type fileSync struct {
	// lock serializes the whole sync of one path. The fields below and the hasher
	// are not safe for concurrent use, so SyncFile holds it for its entire run; two
	// goroutines syncing the same path proceed one at a time, while different paths
	// (different fileSync) run in parallel. It is a one-slot semaphore rather than a
	// sync.Mutex so the wait can be abandoned when the caller's context is canceled:
	// the holder may be parked on a slow HTTP call, and a mutex wait would ignore a
	// shutdown. A nil channel (a fileSync built directly in a test) means no locking.
	lock chan struct{}

	// refs and lastUse are protected by Client.mu. refs includes both the current
	// lock holder and callers waiting for the lock; an entry is eligible for cache
	// eviction only at zero.
	refs    int
	lastUse uint64

	// The incremental projection belongs to one logical session in one filesystem
	// object. A path can be reused for a different session or atomically replaced;
	// either change invalidates every cached cursor and pending turn below.
	agent    string
	sourceID string
	fileInfo os.FileInfo

	// Verified prefix, over the transformed stream. The server holds transformed
	// bytes [0, base); prefixHasher has consumed exactly those bytes, so the next
	// verification re-derives the prefix from disk and replaces the hasher before an
	// append. origBase is the original-file offset that produced base transformed
	// bytes, so the next transform pass reads original [origBase, size). prefixSize
	// is the original file size observed at the last verification.
	base         int64
	origBase     int64
	prefixHasher hash.Hash
	prefixSize   int64

	// pending caches an open Codex trailing turn (its withheld rewritten lines and
	// the scan offset just past them) so a repeated sync of a session whose final
	// turn is still open re-transforms only the appended delta, not the whole turn.
	// It is nil for Claude and pi (every line is a boundary, nothing is withheld) and
	// after a turn closes or the file settles. Like the rest of fileSync it is a
	// cache: dropping it costs a one-time re-transform, never correctness.
	pending *pendingTurn

	// partialSearch caches how far the scanner searched an incomplete trailing line
	// for a newline on the last tick, so the next tick resumes the newline search there
	// instead of rescanning the whole partial line. It keeps a line written over many
	// appends from costing quadratic newline-search work. Zero when no partial line is
	// pending. Also a cache: dropping it costs one redundant rescan.
	partialSearch int64

	// tailProof authenticates the original bytes represented by pending and
	// partialSearch. File identity and length cannot prove append-only growth: a
	// writer may truncate or rewrite the same inode, including to a larger size.
	// Reusing either cursor is safe only after the covered bytes are re-hashed.
	tailProofStart int64
	tailProofEnd   int64
	tailProofSHA   [sha256.Size]byte
	haveTailProof  bool
}

// New builds a Client. baseURL is the server root (trailing slash optional).
func New(httpClient *http.Client, baseURL, token string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		http:    httpClient,
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		enc:     casenc.NewLimited(runtime.NumCPU()),
		uploads: uploadLimiterOrFallback(newAdaptiveUploadLimiter),
		files:   map[string]*fileSync{},
	}
}

// uploadLimiterOrFallback returns the adaptive limiter built by make, or a fixed-width
// limiter when make fails. The adaptive limiter is built from static, valid parameters,
// so the fallback does not trigger in practice; it exists so a Client is always usable
// rather than left without upload concurrency control. It takes the constructor as a
// parameter so a test can drive the fallback branch.
func uploadLimiterOrFallback(make func() (uploadLimiter, error)) uploadLimiter {
	if lim, err := make(); err == nil {
		return lim
	}
	return newFixedUploadLimiter(uploadInitialConcurrency)
}

// retainFileState returns the cached sync state for a path and pins it against
// eviction. The caller must eventually call releaseFileStateRef.
func (c *Client) retainFileState(path string) *fileSync {
	c.mu.Lock()
	defer c.mu.Unlock()
	fs := c.files[path]
	if fs == nil {
		fs = &fileSync{lock: make(chan struct{}, 1)}
		c.files[path] = fs
	}
	fs.refs++
	c.fileUse++
	fs.lastUse = c.fileUse
	c.evictFileStatesLocked()
	return fs
}

// releaseFileStateRef unpins a state after a completed lock hold or an abandoned
// lock wait, then trims the idle cache back to its bound.
func (c *Client) releaseFileStateRef(fs *fileSync) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fs.refs--
	c.fileUse++
	fs.lastUse = c.fileUse
	c.evictFileStatesLocked()
}

// evictFileStatesLocked drops the least-recently-used idle entries until the
// cache is within its bound. It deliberately leaves the map oversized when every
// candidate is active; the next release trims it.
func (c *Client) evictFileStatesLocked() {
	for len(c.files) > fileStateCacheCap {
		var oldestPath string
		var oldest *fileSync
		for path, fs := range c.files {
			if fs.refs != 0 || (oldest != nil && fs.lastUse >= oldest.lastUse) {
				continue
			}
			oldestPath, oldest = path, fs
		}
		if oldest == nil {
			return
		}
		delete(c.files, oldestPath)
	}
}

// acquireFileState pins and locks one path's state. Pinning happens before the
// wait so an entry with queued callers cannot be evicted and replaced by a second
// lock for the same path.
func (c *Client) acquireFileState(ctx context.Context, path string) (*fileSync, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fs := c.retainFileState(path)
	select {
	case fs.lock <- struct{}{}:
		// A cancellation can race with the available slot. Hand the slot back rather
		// than starting filesystem or network work after cancellation.
		if err := ctx.Err(); err != nil {
			<-fs.lock
			c.releaseFileStateRef(fs)
			return nil, err
		}
		return fs, nil
	case <-ctx.Done():
		c.releaseFileStateRef(fs)
		return nil, ctx.Err()
	}
}

func (c *Client) releaseFileState(fs *fileSync) {
	<-fs.lock
	c.releaseFileStateRef(fs)
}

// bind invalidates derived state when a path now names another logical session
// or filesystem object. The cold state can still resume against a matching server
// prefix; it simply has to prove that prefix from disk again.
func (fs *fileSync) bind(t Target, info os.FileInfo) {
	if fs.fileInfo != nil && (fs.agent != t.Agent || fs.sourceID != t.SourceID || !os.SameFile(fs.fileInfo, info)) {
		fs.clearDerived()
	}
	fs.agent = t.Agent
	fs.sourceID = t.SourceID
	fs.fileInfo = info
}

func (fs *fileSync) clearDerived() {
	fs.base = 0
	fs.origBase = 0
	fs.prefixHasher = nil
	fs.prefixSize = 0
	fs.dropTailCache()
}

func (fs *fileSync) dropTailCache() {
	fs.pending = nil
	fs.partialSearch = 0
	fs.tailProofStart = 0
	fs.tailProofEnd = 0
	fs.tailProofSHA = [sha256.Size]byte{}
	fs.haveTailProof = false
}

// rewind sets the verified prefix back to empty: the server holds nothing, so the
// whole file will re-transform and re-upload from zero. Both the transformed
// cursor and the original cursor reset, since with nothing accepted there is no
// verified original prefix to resume from.
func (fs *fileSync) rewind(size int64) {
	fs.clearDerived()
	fs.prefixHasher = sha256.New()
	fs.prefixSize = size
}

// maxConflictRetries bounds how many times a single SyncFile re-announces after
// an offset conflict, so an unexpected server-state race cannot loop forever.
const maxConflictRetries = 5

// SyncFile announces a file, reconciles against the server's cursor, and uploads
// any new complete lines. It is safe to call repeatedly: an up-to-date file moves
// no bytes.
//
// A mid-stream offset conflict (HTTP 409) means the server's cursor moved out
// from under us, so the prefix verified at announce is stale. Rather than trust
// the conflict's reported cursor blindly (which could append onto a divergent
// prefix), SyncFile re-announces and re-verifies the prefix from scratch, up to
// maxConflictRetries times.
func (c *Client) SyncFile(ctx context.Context, t Target) (Outcome, error) {
	// Take the per-path lock before opening. A waiter must observe the path as it
	// exists after the previous holder finishes; opening first would pin an old inode
	// across an atomic replacement while the caller waited.
	fs, err := c.acquireFileState(ctx, t.Path)
	if err != nil {
		return Outcome{}, err
	}
	defer c.releaseFileState(fs)

	f, err := os.Open(t.Path)
	if err != nil {
		return Outcome{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return Outcome{}, err
	}
	fs.bind(t, info)

	var totalUploaded int64
	sawReset := false
	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		out, conflicted, err := c.syncOnce(ctx, t, fs, f)
		if err != nil {
			return Outcome{}, err
		}
		totalUploaded += out.UploadedBytes
		if out.Action == ActionReset {
			sawReset = true
		}
		if !conflicted {
			// Roll up work across all retries so the caller's summary is accurate.
			out.UploadedBytes = totalUploaded
			switch {
			case sawReset:
				out.Action = ActionReset
			case totalUploaded > 0:
				out.Action = ActionUploaded
			}
			// The whole transcript has landed. If this is a --finalize sync, ask the
			// server to grade the session now rather than on the next settle tick: on an
			// ephemeral host the grade has to land before teardown. The session was
			// announced terminal, so this refresh derives its signals with the idle checks
			// already satisfied. It runs once per file (each SyncFile is one session) and
			// only after the successful upload, so a mid-upload conflict retry never
			// finalizes a partial transcript.
			if t.Finalize && out.SessionID != 0 {
				if err := c.finalize(ctx, out.SessionID); err != nil {
					return Outcome{}, fmt.Errorf("finalize %s: %w", t.Path, err)
				}
			}
			return out, nil
		}
	}
	return Outcome{}, fmt.Errorf("sync %s: too many offset conflicts (%d attempts)", t.Path, maxConflictRetries)
}

// syncOnce performs one announce, reconcile, and upload pass. It returns
// conflicted=true if an offset conflict interrupted the upload, signalling the
// caller to re-announce and try again.
func (c *Client) syncOnce(ctx context.Context, t Target, fs *fileSync, f *os.File) (Outcome, bool, error) {
	ann, err := c.announce(ctx, t)
	if err != nil {
		return Outcome{}, false, err
	}
	info, err := f.Stat()
	if err != nil {
		return Outcome{}, false, err
	}
	size := info.Size()
	// A session's final turn has no closing user line, so it is withheld until the
	// file goes idle. Treat a file untouched for settleWindow as settled, which
	// also keeps a turn that merely paused mid-write from being flushed early.
	// --finalize (t.Finalize) forces settled: on an ephemeral host the idle window
	// never elapses before teardown, so the caller asserts the session is terminal
	// and the trailing turn flushes now rather than being lost.
	settled := t.Finalize || time.Since(info.ModTime()) > settleWindow

	action := ActionUploaded
	if ann.StoredBytes == 0 {
		// The server holds nothing: there is no prefix to verify. A new Codex session
		// whose first turn is still open keeps the server at zero every tick (the turn
		// is withheld), so a plain rewind here would discard the cached open turn and
		// re-transform from offset zero each tick, quadratic over the growing turn.
		// Preserve a pending turn or partial-line search only after proving that every
		// original byte it represents is unchanged. Size and inode checks alone miss a
		// truncate-and-rewrite of the same path.
		tailOK, err := fs.tailCacheMatches(ctx, f, 0, size)
		if err != nil {
			return Outcome{}, false, err
		}
		if tailOK {
			fs.base = 0
			fs.origBase = 0
			fs.prefixHasher = sha256.New()
			fs.prefixSize = size
		} else {
			fs.rewind(size)
		}
	} else {
		ok, err := c.verifyPrefix(ctx, f, fs, t.Agent, ann.StoredBytes, size, ann.PrefixSHA256)
		if err != nil {
			return Outcome{}, false, err
		}
		if !ok {
			// The local file was rotated, truncated, or rewritten, or the server
			// diverged: drop the server copy and re-upload from zero.
			if err := c.reset(ctx, ann.SessionID); err != nil {
				return Outcome{}, false, err
			}
			fs.rewind(size)
			action = ActionReset
		} else {
			tailOK, err := fs.tailCacheMatches(ctx, f, fs.origBase, size)
			if err != nil {
				return Outcome{}, false, err
			}
			if !tailOK {
				fs.dropTailCache()
			}
		}
	}

	out := Outcome{StoredBytes: ann.StoredBytes, SessionID: ann.SessionID}

	// Transform the unsent original tail into boundary-aligned transformed chunks,
	// streaming each chunk (and the bodies it references) to the CAS as it becomes
	// ready, so no more than one chunk's worth of transcript is ever buffered. Each
	// pass reads original [origBase, size); origBase advances only past bytes the
	// server accepts, so steady-state work is proportional to the newly appended
	// bytes. A withheld trailing turn leaves origBase where it is and is re-examined
	// next tick (the only repeated work, and only for an open final turn).
	sink := &syncSink{c: c, fs: fs, sessionID: ann.SessionID, size: size, out: &out, present: newSeenCache(), pendingShas: map[string]struct{}{}, enc: c.enc}
	tr := newTransformer(f, fs.origBase, size, t.Agent, sink, c.enc, fs.pending, fs.partialSearch)
	_, conflicted, err := tr.run(ctx, settled)
	if err != nil {
		return out, false, err
	}
	if conflicted {
		// The server cursor moved; the cached open turn may no longer line up, so drop
		// it and let the re-announce rebuild from the verified prefix.
		fs.dropTailCache()
		return out, true, nil
	}
	// Upload any tool bodies lifted from a withheld trailing turn whose transcript chunk
	// was not emitted this pass. The held lines are cached and never re-transformed, so
	// this is the only tick those bodies are seen: ensuring them now (pinned on the
	// server) means the chunk that references them, when it finally lands, finds the CAS
	// already holding them.
	if err := sink.flushPending(ctx); err != nil {
		return out, false, err
	}
	// Carry an open Codex trailing turn to the next tick so it is not re-transformed.
	// For Claude, pi, or a settled/closed turn this is nil and the cache clears.
	fs.pending = tr.snapshot()
	// Carry how far an incomplete trailing line was searched, so the next tick resumes
	// the newline search at the appended tail rather than from the line start.
	fs.partialSearch = tr.partialSearchedTo()
	if err := fs.rememberTailProof(ctx, f, size); err != nil {
		fs.dropTailCache()
		return out, false, err
	}

	if out.UploadedBytes == 0 && action != ActionReset {
		action = ActionUpToDate
	}
	out.Action = action
	return out, false, nil
}

// tailCacheMatches reports whether pending and partialSearch still describe the
// current file at original offset origBase. It hashes only the unaccepted range;
// verifyPrefix separately authenticates the accepted transformed prefix.
func (fs *fileSync) tailCacheMatches(ctx context.Context, f *os.File, origBase, size int64) (bool, error) {
	if fs.pending == nil && fs.partialSearch == 0 {
		return false, nil
	}
	end := fs.tailCacheEnd()
	if !fs.haveTailProof || fs.tailProofStart != origBase || fs.tailProofEnd != end || end > size {
		return false, nil
	}
	got, err := hashFileRange(ctx, f, fs.tailProofStart, fs.tailProofEnd)
	if err != nil {
		return false, err
	}
	return got == fs.tailProofSHA, nil
}

func (fs *fileSync) tailCacheEnd() int64 {
	end := fs.partialSearch
	if fs.pending != nil && fs.pending.scanEnd > end {
		end = fs.pending.scanEnd
	}
	return end
}

// rememberTailProof records the current original bytes covered by the incremental
// tail caches. A later sync must reproduce this digest before it can skip those
// bytes. This preserves append-only incremental work without trusting mutable file
// metadata as a content guarantee.
func (fs *fileSync) rememberTailProof(ctx context.Context, f *os.File, size int64) error {
	end := fs.tailCacheEnd()
	if end <= fs.origBase {
		fs.tailProofStart = 0
		fs.tailProofEnd = 0
		fs.tailProofSHA = [sha256.Size]byte{}
		fs.haveTailProof = false
		return nil
	}
	if end > size {
		return fmt.Errorf("cache session tail [%d,%d): file size is %d", fs.origBase, end, size)
	}
	sum, err := hashFileRange(ctx, f, fs.origBase, end)
	if err != nil {
		return err
	}
	fs.tailProofStart = fs.origBase
	fs.tailProofEnd = end
	fs.tailProofSHA = sum
	fs.haveTailProof = true
	return nil
}

// hashFileRange hashes a fixed file range through a bounded buffer and checks
// cancellation between reads. The caller supplies offsets from a prior bounded
// scan; a concurrent truncation is therefore a hard error, not a partial proof.
func hashFileRange(ctx context.Context, f *os.File, start, end int64) ([sha256.Size]byte, error) {
	if start < 0 || end < start {
		return [sha256.Size]byte{}, fmt.Errorf("hash session file range [%d,%d): invalid range", start, end)
	}
	h := sha256.New()
	buf := make([]byte, 256<<10)
	for off := start; off < end; {
		if err := ctx.Err(); err != nil {
			return [sha256.Size]byte{}, err
		}
		n := end - off
		if n > int64(len(buf)) {
			n = int64(len(buf))
		}
		if err := readAt(f, buf[:n], off); err != nil {
			return [sha256.Size]byte{}, err
		}
		_, _ = h.Write(buf[:n])
		off += n
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// syncSink wires the streaming transform to the client's CAS and chunk endpoints
// for one sync pass. Bodies the transform lifts are registered (not uploaded inline)
// and ensured present in batches: every pending body is checked and uploaded before
// the chunk that references it lands, so the transform stays memory bounded (it never
// accumulates a chunk) while existence checks and uploads run batched and in parallel.
// It advances the verified-prefix cache as the server accepts chunks.
type syncSink struct {
	c         *Client
	fs        *fileSync
	sessionID int64
	size      int64
	out       *Outcome
	enc       *casenc.Encoder

	// pending holds bodies lifted since the last flush, awaiting a batched existence
	// check and parallel upload. pendingShas dedups them and pendingBytes tracks the
	// in-hand stored bytes held, bounding memory before a flush. present records hashes
	// already confirmed in the CAS this pass so a repeat skips the round-trip; it is a
	// bounded cache, so a re-check after eviction costs one extra request, never
	// correctness.
	pending      []pendingBody
	pendingShas  map[string]struct{}
	pendingBytes int64
	present      *seenCache
}

// emitBody resolves a located body to its CAS key (the sha256 of the bytes the CAS
// stores, which are the body's raw bytes or its zstd-compressed form), uploads those
// stored bytes when the server lacks them, and returns the descriptor the sentinel is
// built from. The sentinel records the RAW body length, not the stored length, so the
// transcript's size metadata is independent of compression.
//
// A small line already holds the encoded stored bytes (RewriteLine ran the encoder);
// a big line is encoded by streaming it from the file. Encoding it here is the only
// way to learn its key, since the key is the hash of the compressed bytes, so a body
// that turns out to be missing is deliberately encoded a second time for the upload
// (HashStream here to hash, then StreamAs in putBody to upload). That second
// compression pass is an accepted tradeoff, not an inefficiency to remove: it keeps
// the server from ever compressing, which is the whole point of the compressed-CAS
// design.
//
// emitBody does not check or upload inline: it computes the key, registers the body for
// the next batched existence check, and returns the descriptor so the transform can
// assemble the sentinel immediately. flushPending (called before each chunk and at the
// end of the pass) then checks the queued hashes in parallel batches and uploads the
// missing bodies concurrently. Dedup is the server's job: MissingBlobs reports a body
// already in the CAS (from any session) as not-missing and pins it, so a present body is
// never re-sent. The client also keeps a bounded cache of hashes already confirmed
// present this pass, to skip the round-trip for a body that recurs (the same tool result
// echoed across adjacent turns); the cache is capped, so its memory does not grow with
// the number of distinct bodies in a session.
func (s *syncSink) emitBody(ctx context.Context, ref bodyRef) (parser.Body, error) {
	body, contentType, err := s.bodyDescriptor(ctx, ref)
	if err != nil {
		return parser.Body{}, err
	}
	if err := s.registerBody(ctx, body.SHA256, contentType, ref); err != nil {
		return parser.Body{}, err
	}
	return body, nil
}

// seenCacheCap bounds the recently-handled-body cache. It is a small constant: the
// cache only exists to collapse back-to-back duplicate uploads, not to track every
// body, so its memory is fixed regardless of how many distinct bodies a session has.
const seenCacheCap = 1024

// seenCache is a bounded set of recently handled body hashes with simple wholesale
// eviction: when it fills, it is cleared. It never grows past seenCacheCap entries, so
// it cannot leak with unique body count. A miss after eviction costs one extra server
// check, never a correctness problem (the server is authoritative for presence).
type seenCache struct {
	m map[string]struct{}
}

func newSeenCache() *seenCache { return &seenCache{m: make(map[string]struct{}, seenCacheCap)} }

func (c *seenCache) has(sha string) bool {
	_, ok := c.m[sha]
	return ok
}

func (c *seenCache) add(sha string) {
	if len(c.m) >= seenCacheCap {
		// Drop the whole set rather than track recency: cheap, and a re-check after a
		// flush is harmless. This keeps the cache strictly bounded.
		c.m = make(map[string]struct{}, seenCacheCap)
	}
	c.m[sha] = struct{}{}
}

// emitChunk uploads one transformed chunk and folds the result into the verified
// prefix and the outcome. A 409 conflict is reported to the transform, which unwinds
// so the caller re-announces.
//
// Every body the chunk's sentinels reference was registered while its lines were being
// transformed, before this chunk was cut, so flushing the pending set here guarantees
// the CAS holds them all before the transcript that points at them lands. flushPending
// also covers bodies for lines beyond this chunk (a held open turn): uploading them
// early is harmless, since the server pins them until their own chunk commits.
func (s *syncSink) emitChunk(ctx context.Context, data []byte, origLen int64) (bool, error) {
	if err := s.flushPending(ctx); err != nil {
		return false, err
	}
	r, err := s.c.chunk(ctx, s.sessionID, s.fs.base, data)
	if err != nil {
		return false, err
	}
	if r.conflict {
		return true, nil
	}
	// The server accepted these transformed bytes, so extend the verified
	// transformed-prefix digest over them and advance the original cursor.
	s.fs.prefixHasher.Write(data)
	s.fs.base = r.storedBytes
	s.fs.origBase += origLen
	s.fs.prefixSize = s.size
	s.out.UploadedBytes += int64(len(data))
	s.out.StoredBytes = r.storedBytes
	return false, nil
}

// boundaryWithin returns the absolute file position just past the last message
// boundary in buf (which begins at file position bufStart and ends on a newline),
// or 0 when buf holds none. For Claude and pi every newline is a boundary, so it
// is the buffer's end; for Codex it is the last turn close (a user line).
func boundaryWithin(buf []byte, bufStart int64, agent string) int64 {
	if agent != agentCodex {
		return bufStart + int64(len(buf))
	}
	if rel := lastCodexTurnEnd(buf); rel > 0 {
		return bufStart + int64(rel)
	}
	return 0
}

// errMessageTooBig reports that one transformed message (a single rewritten JSONL
// line, after its tool bodies were lifted to the CAS) is larger than hardCap and so
// cannot be sent as one chunk. It fires only for a genuinely pathological line:
// after lifting, an ordinary line is small, and a large bodyless line rides inline
// up to this cap rather than being refused. The old wording ("without a boundary")
// described the symptom the assembler once saw, not the cause, and misled debugging
// of image-heavy Codex sessions, so it names the hard cap directly.
func errMessageTooBig(agent string) error {
	return fmt.Errorf("session %s: a single transformed message exceeds the %d-byte hard cap even after lifting tool bodies", agent, hardCap)
}

// readAt fills buf entirely from offset off. A short read means the file was
// truncated or rotated between Stat and now, so the missing bytes would otherwise
// be read as zero-filled session content; readAt treats any short read as an error
// (including the io.EOF ReadAt returns for one) and wraps it with the range. A full
// read returns nil even at the exact end of file, where ReadAt may report io.EOF.
func readAt(f *os.File, buf []byte, off int64) error {
	n, err := f.ReadAt(buf, off)
	if n == len(buf) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("read session file [%d,%d): short read (%d of %d bytes): %w",
		off, off+int64(len(buf)), n, len(buf), err)
}

// agentCodex is the agent string whose sessions fold a turn across lines, so a
// chunk boundary is a turn close (a user line) rather than any newline.
const agentCodex = "codex"

// codexLine is the minimal shape needed to spot a turn boundary: a Codex turn
// closes at the next user entry, so a response_item carrying role "user" ends the
// preceding turn.
type codexLine struct {
	Type    string `json:"type"`
	Payload struct {
		Role string `json:"role"`
	} `json:"payload"`
}

// lastCodexTurnEnd returns the offset just past the last line that closes a turn
// (a response_item with role "user"), or 0 when the buffer holds no such line.
// Cutting there keeps every folded turn whole within one chunk: the user entry
// that closes a turn travels with it, and the next turn begins in the next chunk.
func lastCodexTurnEnd(buf []byte) int {
	last := 0
	start := 0
	for i := 0; i < len(buf); i++ {
		if buf[i] != '\n' {
			continue
		}
		var cl codexLine
		if json.Unmarshal(bytes.TrimSpace(buf[start:i]), &cl) == nil &&
			cl.Type == "response_item" && cl.Payload.Role == "user" {
			last = i + 1
		}
		start = i + 1
	}
	return last
}

// verifyPrefix reports whether the TRANSFORMED prefix of the local file matches
// the server's stored transformed bytes, and advances fs to record that
// verification. The server holds transformed bytes (tool bodies lifted to the
// CAS), so the comparison is against the transformed prefix, not the raw file.
//
// Prefix verification always re-derives the transformed bytes from the current
// file. A cached digest proves only what a previous read saw; trusting it would
// miss an in-place rewrite whose size did not shrink and append new bytes to a
// stale server prefix. The pass streams from disk without a prefix-sized
// allocation.
func (c *Client) verifyPrefix(ctx context.Context, f *os.File, fs *fileSync, agent string, serverBytes, size int64, want string) (bool, error) {
	h, origBase, ok, err := transformPrefixDigest(ctx, f, agent, size, serverBytes, c.enc)
	if err != nil {
		return false, err
	}
	if !ok || hex.EncodeToString(h.Sum(nil)) != want {
		return false, nil
	}
	fs.prefixHasher = h
	fs.base = serverBytes
	fs.origBase = origBase
	fs.prefixSize = size
	return true, nil
}

type announceResp struct {
	SessionID    int64  `json:"session_id"`
	StoredBytes  int64  `json:"stored_bytes"`
	PrefixSHA256 string `json:"prefix_sha256"`
}

func (c *Client) announce(ctx context.Context, t Target) (announceResp, error) {
	body := map[string]any{
		"agent":             t.Agent,
		"source_session_id": t.SourceID,
		"kind":              t.Kind,
		"project_remote":    t.ProjectKey,
		"local_root":        t.LocalRoot,
		"git_branch":        t.GitBranch,
		"cwd":               t.Cwd,
		"machine":           t.Machine,
		// A --finalize sync announces the session terminal so the server can grade it
		// immediately (see Target.Finalize); an ordinary sync sends false.
		"terminal": t.Finalize,
	}
	var out announceResp
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/ingest/session", body, &out); err != nil {
		return announceResp{}, err
	}
	return out, nil
}

func (c *Client) reset(ctx context.Context, sessionID int64) error {
	path := fmt.Sprintf("/api/v1/ingest/session/%d/reset", sessionID)
	return c.doJSON(ctx, http.MethodPost, path, nil, nil)
}

// finalize asks the server to grade a terminal session now that its whole transcript
// has landed, so an ephemeral host sees the grade before it is torn down instead of
// waiting for the settle pass. It carries no body: the session was announced terminal
// and the grade derives from the projection the chunks already built.
func (c *Client) finalize(ctx context.Context, sessionID int64) error {
	path := fmt.Sprintf("/api/v1/ingest/session/%d/finalize", sessionID)
	return c.doJSON(ctx, http.MethodPost, path, nil, nil)
}

type chunkResult struct {
	storedBytes int64
	conflict    bool
}

func (c *Client) chunk(ctx context.Context, sessionID, offset int64, data []byte) (chunkResult, error) {
	url := fmt.Sprintf("%s/api/v1/ingest/session/%d/chunk?offset=%d", c.baseURL, sessionID, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return chunkResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return chunkResult{}, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		var r struct {
			StoredBytes int64 `json:"stored_bytes"`
		}
		if err := json.Unmarshal(payload, &r); err != nil {
			return chunkResult{}, fmt.Errorf("decode chunk response: %w", err)
		}
		return chunkResult{storedBytes: r.StoredBytes}, nil
	case http.StatusConflict:
		var r struct {
			StoredBytes int64 `json:"stored_bytes"`
		}
		if err := json.Unmarshal(payload, &r); err != nil {
			return chunkResult{}, fmt.Errorf("decode conflict response: %w", err)
		}
		return chunkResult{storedBytes: r.StoredBytes, conflict: true}, nil
	default:
		return chunkResult{}, fmt.Errorf("chunk: server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
}

// checkBlobs returns the set of hashes the server does not yet hold.
func (c *Client) checkBlobs(ctx context.Context, shas []string) (map[string]bool, error) {
	var resp struct {
		Missing []string `json:"missing"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/ingest/blobs/check",
		map[string]any{"sha256": shas}, &resp); err != nil {
		return nil, err
	}
	missing := make(map[string]bool, len(resp.Missing))
	for _, sha := range resp.Missing {
		missing[sha] = true
	}
	return missing, nil
}

// putBody streams one tool body's STORED bytes to the CAS under its key, declaring
// both the body's semantic media type and its storage content type (raw or zstd). For
// a small line the stored bytes are already in hand; for a big line they are produced
// by streaming the body's canonical bytes through the encoder, so a hundreds-of-MiB
// body uploads in O(window) memory and is never resident. The server verifies the
// uploaded bytes hash to sha and pins the body against the sweep, so a corrupt upload
// is rejected and a present-but-unreferenced body survives until the transcript lands;
// it stores the bytes opaquely and never decompresses them.
func (c *Client) putBody(ctx context.Context, enc *casenc.Encoder, sha, contentType string, ref bodyRef) error {
	endpoint := fmt.Sprintf("%s/api/v1/ingest/blob/%s?media_type=%s&content_type=%s",
		c.baseURL, sha, url.QueryEscape(ref.media), url.QueryEscape(contentType))

	var bodyReader io.Reader
	if ref.haveContent {
		bodyReader = bytes.NewReader(ref.stored)
	} else {
		bodyReader = enc.StreamAs(ctx, ref.canonicalReader(ctx), contentType)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	if ref.haveContent {
		req.ContentLength = int64(len(ref.stored))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// Return a typed status error so the upload limiter can tell a server shedding
		// load (429/5xx) from a client-side fault and retune accordingly.
		return &httpStatusError{op: fmt.Sprintf("upload blob %s", sha), code: resp.StatusCode, body: strings.TrimSpace(string(payload))}
	}
	return nil
}

// httpStatusError is a non-2xx server response carrying the status code, so callers
// (notably the upload limiter's load-shed detection) can branch on the code rather than
// parse a formatted string.
type httpStatusError struct {
	op   string
	code int
	body string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s: server returned %d: %s", e.op, e.code, e.body)
}

// doJSON performs a JSON request, optionally decoding a JSON response into out.
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: server returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out != nil {
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}
