// Package casenc encodes tool bodies for the content-addressed store. The CAS key
// is the sha256 of the STORED bytes, not the raw body: a body large enough to be
// worth it is stored zstd-compressed (content type application/zstd) and keyed by
// the hash of that compressed form; a smaller body is stored verbatim
// (application/octet-stream) and keyed by the hash of its raw bytes.
//
// Compression lives entirely on the client. The server stores and serves these
// bytes opaquely, verifying only that they hash to the declared key; it never
// compresses or decompresses, so it stays off the CPU-bound path and links no
// compression code. The browser decompresses transparently via Content-Encoding.
//
// The client deliberately spends redundant CPU to keep the server that simple.
// Because the CAS key is the hash of the stored (compressed) bytes, the only way to
// learn a body's key is to encode the body: so a body that turns out to be absent is
// encoded a second time
// for the upload itself (HashStream then StreamAs), and cold-cache prefix
// verification re-encodes bodies to recompute their keys. Each redundant pass is a
// fixed constant factor over one body, never superlinear, and it buys a server that
// does no compression and a protocol with no server round-trip to hand the client a
// key. We accept wasting some client CPU here precisely so the server never pays it.
//
// Determinism is the contract that makes content addressing and resync dedup work:
// the same raw body must always produce the same stored bytes and the same key,
// whether it is encoded from memory (EncodeBody) or streamed from disk
// (HashStream / StreamAs). Two things guarantee that: a single fixed zstd
// configuration, and single-threaded encoding (klauspost's parallel encoder splits
// blocks nondeterministically, which would change the bytes and thus the key). Both
// are pinned in newZstd. The compression threshold is checked against the raw body
// length identically in both paths, so a body never gets two different keys just
// because its transcript line happened to be buffered in one session and streamed
// in another.
package casenc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/jssblck/akari/internal/parser"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/semaphore"
)

// Threshold is the raw-body size at or above which a body is zstd-compressed. Below
// it the zstd frame overhead is not worth paying (and can even expand a tiny body),
// so the body is stored verbatim. It must stay far below the client's big-line
// threshold (1 MiB) so a body's compression decision never depends on whether its
// transcript line was buffered or streamed: any body large enough to be streamed is
// far above this and is compressed either way. It is a var so tests can shrink it to
// exercise the compressed path on small inputs.
var Threshold = 1024

// Level is the zstd compression level. SpeedDefault (level 3) matches the courier
// CAS this is modeled on and balances ratio against the client CPU we deliberately
// spend here. It is a var so a test or operator can retune it; changing it changes
// the stored bytes (and so the key) of future uploads, which at worst re-stores some
// bodies under new keys, never corrupts anything.
var Level = zstd.SpeedDefault

// Encoder encodes bodies with one fixed configuration. It is safe for concurrent
// use; a Client holds a single shared instance, so many files lifting bodies at once
// share one Encoder.
//
// compSem, when set, bounds how many zstd compression passes run at once across every
// goroutine sharing the Encoder, so building CAS keys for many bodies in parallel (a
// fleet of files syncing, or batched uploads) cannot oversubscribe the CPUs. It guards
// only the compression branches that produce a key (EncodeBody and HashStream); the
// cheap raw-hash path needs no bound. The upload re-compression in StreamAs is left to
// the caller's upload-concurrency limiter instead, so a slow network upload never holds
// a CPU permit while it waits on the wire. A nil compSem means unbounded, the default
// for direct use and tests.
type Encoder struct {
	compSem *semaphore.Weighted
}

// New builds an Encoder with no compression-concurrency bound.
func New() *Encoder { return &Encoder{} }

// NewLimited builds an Encoder that lets at most maxConcurrency zstd compression
// passes run at once across all goroutines that share it. A non-positive bound is
// treated as unbounded, matching New.
func NewLimited(maxConcurrency int) *Encoder {
	if maxConcurrency <= 0 {
		return &Encoder{}
	}
	return &Encoder{compSem: semaphore.NewWeighted(int64(maxConcurrency))}
}

// acquireComp blocks until a compression permit is free (or never, when unbounded),
// returning a release func. ctx bounds the wait so a canceled sync stops waiting for a
// permit; a nil ctx (the no-ctx EncodeBody path) waits uninterruptibly, which is safe
// because the permitted work is a short CPU pass, not a network call.
func (e Encoder) acquireComp(ctx context.Context) (release func(), err error) {
	if e.compSem == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := e.compSem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	return func() { e.compSem.Release(1) }, nil
}

// copyChunk bounds how much is moved per read while streaming a body through the
// encoder, so a huge body is processed in fixed slices and cancellation is checked
// between them rather than after reading the whole thing.
const copyChunk = 256 << 10

// newZstd builds a single-threaded zstd writer over dst. Concurrency 1 is mandatory:
// the key is the hash of the writer's output, and the parallel encoder would split
// blocks across goroutines nondeterministically, producing different bytes (and so a
// different key) for the same input. The options here are static and valid, so
// NewWriter never actually errors; it is propagated only for completeness.
func newZstd(dst io.Writer) (*zstd.Encoder, error) {
	return zstd.NewWriter(dst,
		zstd.WithEncoderLevel(Level),
		zstd.WithEncoderConcurrency(1))
}

// EncodeBody encodes a body whose raw bytes are already in hand (the buffered
// small-line path). It returns the CAS key (sha256 of the stored bytes), the stored
// bytes, and their storage content type. It satisfies parser.BodyEncoder.
//
// A body shorter than Threshold is stored verbatim and keyed by its raw hash. The
// returned Stored slice for that case aliases raw (no copy): RewriteLine has already
// copied the line out of the scan buffer, so the bytes are stable for the upload.
func (e Encoder) EncodeBody(raw []byte) (sha string, stored []byte, contentType string) {
	if len(raw) < Threshold {
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:]), raw, parser.ContentRaw
	}
	// Bound concurrent compression to keep many bodies hashing at once off all CPUs.
	// EncodeBody has no context (it satisfies parser.BodyEncoder), so the wait is
	// uninterruptible; the work it gates is a short, fully in-memory pass.
	release, _ := e.acquireComp(nil)
	defer release()
	var buf bytes.Buffer
	// A compressed body is at least somewhat smaller than its input; preallocating a
	// fraction avoids a few grow-copies without overcommitting.
	buf.Grow(len(raw)/2 + 64)
	zw, _ := newZstd(&buf)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	out := buf.Bytes()
	sum := sha256.Sum256(out)
	return hex.EncodeToString(sum[:]), out, parser.ContentZstd
}

// HashStream encodes a body streamed from r (the big-line path and the cold-cache
// digest path) without buffering it, returning the CAS key, the storage content
// type, and the RAW (uncompressed) body length the sentinel records. It peeks the
// first Threshold bytes to make the very same size decision EncodeBody makes, so a
// streamed body and an in-hand body of identical content produce identical keys.
//
// It hashes the STORED bytes: for a small body that is the raw bytes; for a large
// one it is the zstd output, hashed as it is produced so the compressed form is
// never resident. The caller then calls StreamAs to (re)produce the same stored
// bytes for the actual upload.
func (e Encoder) HashStream(ctx context.Context, r io.Reader) (sha, contentType string, rawLen int, err error) {
	head, more, err := readPeek(ctx, r, Threshold)
	if err != nil {
		return "", "", 0, err
	}
	h := sha256.New()
	if !more {
		// The whole body fits under Threshold: store it verbatim, key over the raw
		// bytes, exactly as EncodeBody would.
		h.Write(head)
		return hex.EncodeToString(h.Sum(nil)), parser.ContentRaw, len(head), nil
	}

	// Larger than Threshold: compress head + the rest through one writer, hashing the
	// compressed output and counting the raw bytes consumed. Bound concurrent
	// compression so a batch of big bodies hashing at once does not oversubscribe the
	// CPUs; the wait honors ctx so a canceled sync stops waiting for a permit.
	release, err := e.acquireComp(ctx)
	if err != nil {
		return "", "", 0, err
	}
	defer release()
	zw, _ := newZstd(h)
	if _, err := zw.Write(head); err != nil {
		_ = zw.Close()
		return "", "", 0, fmt.Errorf("compress body head: %w", err)
	}
	rest, err := copyCtx(ctx, zw, r)
	if err != nil {
		_ = zw.Close()
		return "", "", 0, err
	}
	if err := zw.Close(); err != nil {
		return "", "", 0, fmt.Errorf("finalize body compression: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), parser.ContentZstd, len(head) + int(rest), nil
}

// StreamAs returns a reader of the stored bytes for a body streamed from r, given
// the content type HashStream already decided. For a raw body it is r unchanged; for
// a zstd body it streams the compression through a pipe so the upload moves in
// O(window) memory and the compressed form is never fully resident. The bytes it
// yields are byte-identical to what HashStream hashed, so the server's verification
// against the key holds.
func (e Encoder) StreamAs(ctx context.Context, r io.Reader, contentType string) io.Reader {
	if contentType != parser.ContentZstd {
		return r
	}
	pr, pw := io.Pipe()
	go func() {
		zw, _ := newZstd(pw)
		_, cErr := copyCtx(ctx, zw, r)
		clErr := zw.Close()
		err := cErr
		if err == nil {
			err = clErr
		}
		// CloseWithError(nil) is a clean EOF for the reader; a non-nil err surfaces on
		// the consumer's next Read so a failed compression aborts the upload.
		_ = pw.CloseWithError(err)
	}()
	return pr
}

// readPeek reads up to n bytes from r. more reports whether r held at least n bytes
// (so there may be more to come); when more is false, head is the entire remaining
// input and is shorter than n. A real read error (not a short final read) is
// returned. It is the streamed mirror of EncodeBody's len(raw) < Threshold check.
func readPeek(ctx context.Context, r io.Reader, n int) (head []byte, more bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	switch err {
	case nil:
		return buf[:got], true, nil
	case io.EOF, io.ErrUnexpectedEOF:
		// Fewer than n bytes exist: this is the whole body.
		return buf[:got], false, nil
	default:
		return nil, false, fmt.Errorf("read body head: %w", err)
	}
}

// copyCtx copies r into w in bounded slices, checking ctx between them, and returns
// the number of bytes copied. It replaces io.Copy on the body path so a canceled
// sync stops moving a hundreds-of-MiB body instead of running to its end.
func copyCtx(ctx context.Context, w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, copyChunk)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}
