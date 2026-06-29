package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"hash"
	"io"
	"log/slog"
	"os"

	"github.com/jssblck/akari/internal/casenc"
	"github.com/jssblck/akari/internal/parser"
)

// chunkSink consumes the transform's output as it is produced. The transform never
// holds more than one chunk's worth of transcript bytes in memory: as soon as a
// boundary-aligned, size-bounded chunk is ready it is handed here, uploaded, and
// dropped. A big body is never buffered into a chunk: it is referenced by a cheap file
// span the sink re-streams, so the chunk carries only the small literal regions and the
// sentinels that replace the bodies.
//
// emitBody is called for every body the transform lifts, before the chunk that
// references it is emitted, and returns the body descriptor (sha, length, media) the
// sentinel is built from. The sink need not upload inline: it may queue the body and
// ensure it is present when emitChunk flushes, but every body a chunk references must be
// in the CAS before that chunk lands, so the transcript can never reference a body the
// CAS lacks.
type chunkSink interface {
	// emitBody registers one located body for upload and returns the descriptor the
	// sentinel is built from (key, raw byte length, media type). A small-line ref
	// carries the stored bytes already encoded by RewriteLine; a big-line ref carries a
	// file span the sink streams through the encoder. The sink may defer the existence
	// check and upload to a batch, but must complete them before the referencing chunk.
	emitBody(ctx context.Context, ref bodyRef) (parser.Body, error)
	// emitChunk ensures every body the chunk references is in the CAS, then uploads one
	// boundary-aligned transformed chunk and reports how far the original cursor
	// advanced. It returns conflicted=true when the server's cursor moved, which unwinds
	// the whole transform.
	emitChunk(ctx context.Context, data []byte, origLen int64) (conflicted bool, err error)
}

// bodyRef points at one tool body to be lifted to the CAS, either as stored bytes
// already in hand (a small line buffered whole and encoded by RewriteLine) or as a
// file span to be streamed and encoded on the fly (a big line). Exactly one of the
// in-hand fields / (file, span, kind) group is set, distinguished by haveContent.
type bodyRef struct {
	media string
	kind  string // "input" | "result", for diagnostics

	// Small-line path: the encoded stored bytes and their key/encoding, already
	// produced by the encoder inside RewriteLine, plus the raw body length the
	// sentinel records.
	haveContent bool
	sha         string
	stored      []byte
	contentType string
	rawLen      int

	// Big-line path: where to stream the canonical (raw) body from, never buffered
	// whole. The key, encoding, and raw length are computed by streaming it through
	// the encoder.
	file     *os.File
	lineOff  int64
	span     parser.ValueSpan
	bodyKind parser.BodyKind
}

// canonicalReader streams the body's raw canonical bytes for the big-line path. Each
// call returns a fresh reader from the file, so the hash pass and the upload pass
// each get the body from its start.
func (r bodyRef) canonicalReader(ctx context.Context) io.Reader {
	return parser.CanonicalBodyReader(ctx, r.file, r.lineOff, r.span, r.bodyKind)
}

// bigLineThreshold is the original-line size past which the transform stops
// buffering the line and switches to the streaming path: it locates each body's
// span and streams the body straight to the CAS, building the rewritten line from
// only the small literal regions between bodies plus the sentinels. A line at or
// below this is buffered and rewritten in one shot (the common, cheap path), since
// the buffer is bounded by the threshold. The motivating 508 MiB turn is one such
// big line; under streaming it costs O(window), not O(line). It is a var so tests
// can shrink it to exercise the streaming path on small inputs.
var bigLineThreshold int64 = 1 << 20

// transformer turns the unsent original tail of a session file into
// boundary-aligned transformed chunks, lifting every tool body to the CAS, and
// streams them to a sink. It holds incremental state across ticks so a session that
// is synced repeatedly does work proportional to the newly appended bytes, not the
// whole file: the scanner keeps a cursor into its read buffer, and an open Codex
// turn whose close has not yet arrived is carried as a pending run rather than
// re-scanned from the file each tick.
type transformer struct {
	f     *os.File
	agent string
	size  int64
	sink  chunkSink
	enc   *casenc.Encoder

	sc  *origLineScanner
	asm *chunkAssembler
}

// pendingTurn carries the assembler's withheld lines, its boundary counters, and the
// scan offset across sync ticks. For Codex, an open trailing turn (no closing user
// line yet) is withheld and would otherwise be re-transformed from its start every
// tick, quadratic as the turn grows. Caching the rewritten (body-free, small) held
// lines plus the offset just past them lets the next tick resume scanning at the delta
// and re-seed the assembler, so each tick processes only the newly appended bytes. The
// bodies in the held lines were already uploaded the tick they were first seen, so
// re-seeding does not re-upload them.
//
// Ownership of the lines slice is transferred between the assembler and this struct
// rather than copied, and the boundary counters travel with it, so resuming an open
// turn is O(newly appended lines), not O(all withheld lines). It is purely a cache:
// dropping it costs a one-time re-transform of the open turn, never correctness.
type pendingTurn struct {
	lines         []pendingLine
	lastBoundary  int   // assembler's last-boundary index for these lines
	pendingBytes  int   // transformed bytes held
	boundaryBytes int   // releasable prefix bytes
	scanEnd       int64 // original offset just past the held lines (where scanning resumes)
}

// newTransformer builds a transformer reading original [origStart, size). When prev
// holds a cached open turn whose scanEnd is still consistent with origStart, the
// transformer resumes from scanEnd with the held lines restored, so only the appended
// delta is processed. partialSearch, when positive, is an offset a previous tick
// already searched the trailing partial line up to without finding a newline, so the
// scanner skips re-searching it. Otherwise it starts fresh at origStart.
func newTransformer(f *os.File, origStart, size int64, agent string, sink chunkSink, enc *casenc.Encoder, prev *pendingTurn, partialSearch int64) *transformer {
	scanStart := origStart
	asm := newChunkAssembler(agent, f.Name(), origStart)
	if prev != nil && prev.scanEnd >= origStart && prev.scanEnd <= size {
		// Resume past the already-processed held lines and adopt them so the open turn is
		// not re-transformed. Ownership transfers (no copy) and the counters come along,
		// so this is O(1) regardless of how many lines were withheld.
		scanStart = prev.scanEnd
		asm.adopt(prev, origStart)
	}
	sc := newOrigLineScanner(f, scanStart, size)
	if partialSearch > scanStart && partialSearch <= size {
		sc.withResumeSearch(partialSearch)
	}
	return &transformer{
		f:     f,
		agent: agent,
		size:  size,
		sink:  sink,
		enc:   enc,
		sc:    sc,
		asm:   asm,
	}
}

// partialSearchedTo returns how far the scanner searched an incomplete trailing line
// for a newline without finding one, or 0 when no partial line is pending. The caller
// caches it so the next tick resumes the search there.
func (t *transformer) partialSearchedTo() int64 {
	return t.sc.searchedTo
}

// snapshot hands the assembler's withheld lines and counters to a pendingTurn for
// caching across ticks, transferring slice ownership rather than copying. It returns
// nil when nothing is held (Claude and pi, or a settled Codex turn that fully
// flushed), so the cache is populated only for an open Codex turn. The assembler is
// not used again after snapshot in a given sync, so handing off its slice is safe.
func (t *transformer) snapshot() *pendingTurn {
	if len(t.asm.lines) == 0 {
		return nil
	}
	return &pendingTurn{
		lines:         t.asm.lines,
		lastBoundary:  t.asm.lastBoundary,
		pendingBytes:  t.asm.pendingBytes,
		boundaryBytes: t.asm.boundaryBytes,
		scanEnd:       t.sc.completeEnd(),
	}
}

// run scans the unsent tail, transforming each line and emitting chunks to the sink
// as boundaries are reached. It returns the original offset just past everything the
// server accepted and conflicted=true when a chunk upload hit an offset conflict
// (the caller re-announces). Memory stays bounded: at most one chunk's worth of
// transformed bytes plus one line's small head, with big bodies streamed straight
// through to the CAS.
func (t *transformer) run(ctx context.Context, settled bool) (origEnd int64, conflicted bool, err error) {
	t.sc.withContext(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		line, origOff, origLen, isBig, ok, err := t.sc.next()
		if err != nil {
			return 0, false, err
		}
		if !ok {
			break
		}
		if err := t.handleLine(ctx, line, origOff, origLen, isBig); err != nil {
			return 0, false, err
		}
		conflicted, err := t.drain(ctx, false)
		if err != nil || conflicted {
			return t.asm.consumedOrigEnd, conflicted, err
		}
	}
	// The scanner left only a partial trailing line (if any). Flush the assembler's
	// held lines: for Claude and pi every line is a boundary so all emit; for Codex
	// an open trailing turn is withheld unless the file has settled.
	if err := t.asm.finish(settled, t.sc.completeEnd()); err != nil {
		return 0, false, err
	}
	conflicted, err = t.drain(ctx, true)
	if err != nil {
		return 0, false, err
	}
	return t.asm.consumedOrigEnd, conflicted, nil
}

// handleLine transforms one original line into a rewritten line and adds it to the
// assembler. A line at or below bigLineThreshold is buffered and rewritten whole
// (the small path); a bigger line is streamed body by body so it never resides.
func (t *transformer) handleLine(ctx context.Context, line []byte, origOff, origLen int64, isBig bool) error {
	if !isBig {
		return t.handleSmallLine(ctx, line, origOff, origLen)
	}
	return t.handleBigLine(ctx, origOff, origLen)
}

// handleSmallLine rewrites a buffered line in one shot via RewriteLine, then uploads
// each lifted body. RewriteLine already ran the encoder, so the stored bytes, key,
// and encoding are in hand and emitBody uploads them without re-encoding.
func (t *transformer) handleSmallLine(ctx context.Context, line []byte, origOff, origLen int64) error {
	rewritten, bodies := parser.RewriteLine(parser.Agent(t.agent), line, t.enc)
	if int64(len(rewritten)) > hardCap {
		return errMessageTooBig(t.agent)
	}
	for _, b := range bodies {
		if _, err := t.sink.emitBody(ctx, bodyRef{
			media:       b.MediaType,
			kind:        b.Kind,
			haveContent: true,
			sha:         b.SHA256,
			stored:      b.Stored,
			contentType: b.ContentType,
			rawLen:      b.Bytes,
		}); err != nil {
			return err
		}
	}
	t.asm.add(rewritten, origOff, origLen)
	return nil
}

// handleBigLine transforms a line past bigLineThreshold. It locates each tool body's
// raw span by streaming the line, streams each body to the CAS (so the body itself
// is never resident), and assembles the rewritten line from the literal regions
// between bodies plus the sentinels. When the line carries liftable bodies its
// rewritten form is small (its bulk lifted to the CAS). When it carries none (or
// keeps non-body bulk between bodies) the remainder rides inline, copied through a
// bounded window and capped at hardCap: a large-but-bodyless line is buffered and
// sent like any other message rather than refused, and only a line that truly
// exceeds hardCap errors.
func (t *transformer) handleBigLine(ctx context.Context, origOff, origLen int64) error {
	// The line spans [origOff, origOff+origLen) in the file, trailing newline
	// included. Locate bodies over the content without the newline so a span never
	// includes it.
	contentLen, hasNL, err := lineContentLen(t.f, origOff, origLen)
	if err != nil {
		return err
	}

	// Build the rewritten line incrementally as each body is located, so the parser
	// never holds a slice of all body locations and the bodies upload one at a time.
	// The emit callback runs once per body in source order: copy the literal gap up to
	// the body, upload the body, append its sentinel, and advance the cursor.
	var rewritten []byte
	cursor := int64(0)
	emit := func(loc parser.BodyLocation) error {
		// Honor cancellation between bodies: a big line can hold many bodies, each a
		// streamed upload, so a shutdown must not have to wait for the whole line.
		if err := ctx.Err(); err != nil {
			return err
		}
		if loc.Span.Start < cursor || loc.Span.End > contentLen {
			return nil // a span out of order or past the line: skip defensively
		}
		body, err := t.sink.emitBody(ctx, bodyRef{
			media:    loc.Media,
			kind:     "", // big-line kind is not surfaced; diagnostics use the small path
			file:     t.f,
			lineOff:  origOff,
			span:     loc.Span,
			bodyKind: loc.Kind,
		})
		if err != nil {
			return err
		}
		rewritten, err = appendFileSpan(rewritten, t.f, t.agent, origOff+cursor, loc.Span.Start-cursor)
		if err != nil {
			return err
		}
		rewritten = append(rewritten, sentinelFor(body)...)
		cursor = loc.Span.End
		if int64(len(rewritten)) > hardCap {
			return errMessageTooBig(t.agent)
		}
		return nil
	}
	if err := parser.LocateToolBodies(ctx, parser.Agent(t.agent), t.f, origOff, contentLen, emit); err != nil {
		return err
	}

	// A big line with no liftable body (or one whose tail follows the last lifted
	// body) rides inline: appendFileSpan copies the remaining content into the
	// rewritten line through a bounded window, enforcing hardCap as it goes. Only a
	// line that genuinely exceeds hardCap is refused; a merely-large bodyless line
	// (an image-progress event, a compacted history, anything past bigLineThreshold
	// but under the cap) is buffered and sent like any other message. This keeps the
	// memory-bounded streaming for liftable bodies while never rejecting a line the
	// server could store.
	rewritten, err = appendFileSpan(rewritten, t.f, t.agent, origOff+cursor, contentLen-cursor)
	if err != nil {
		return err
	}
	if hasNL {
		rewritten = append(rewritten, '\n')
	}
	if int64(len(rewritten)) > hardCap {
		return errMessageTooBig(t.agent)
	}
	t.asm.add(rewritten, origOff, origLen)
	return nil
}

// drain emits every chunk the assembler can now produce, uploading each through the
// sink and advancing the cursor. With final=false it emits only chunks ending on a
// boundary at or past the size target (steady state); with final=true it flushes
// whatever finish() readied. A conflict from any chunk upload stops the drain.
func (t *transformer) drain(ctx context.Context, final bool) (bool, error) {
	for {
		chunk, origLen, ok := t.asm.takeReady(final)
		if !ok {
			return false, nil
		}
		conflicted, err := t.sink.emitChunk(ctx, chunk, origLen)
		if err != nil {
			return false, err
		}
		if conflicted {
			return true, nil
		}
		t.asm.commit(origLen)
	}
}

// appendFileSpanBuf bounds how much of a literal gap is read at once when copying
// the non-body regions of a big line into the rewritten line. Copying through a
// fixed window keeps a pathological gap (a giant non-body string between bodies)
// from allocating its whole length in one read.
const appendFileSpanBuf = 256 << 10

// appendFileSpan appends n bytes read from f at off to dst, copying through a fixed
// bounded window rather than allocating the whole gap, and enforcing hardCap as it
// goes so the rewritten line cannot grow without bound. A short read is a hard error
// (the file was truncated mid-line): it must never be treated as success, which would
// splice zero-filled or partial bytes into the transcript. The gap between two bodies
// is normally tiny (JSON structure), but the bound makes the worst case a constant.
func appendFileSpan(dst []byte, f *os.File, agent string, off, n int64) ([]byte, error) {
	if n <= 0 {
		return dst, nil
	}
	buf := make([]byte, appendFileSpanBuf)
	remaining := n
	at := off
	for remaining > 0 {
		want := remaining
		if want > int64(len(buf)) {
			want = int64(len(buf))
		}
		if err := readAt(f, buf[:want], at); err != nil {
			return dst, err
		}
		dst = append(dst, buf[:want]...)
		if int64(len(dst)) > hardCap {
			return dst, errMessageTooBig(agent)
		}
		remaining -= want
		at += want
	}
	return dst, nil
}

// sentinelFor renders the CAS reference that replaces a body, reusing the parser's
// canonical encoding so the bytes match what RewriteLine produces for a small line.
func sentinelFor(b parser.Body) []byte {
	return parser.SentinelBytes(b.SHA256, b.Bytes, b.MediaType)
}

// lineContentLen returns the byte length of a line's content (its bytes minus a
// trailing newline) and whether it ended in a newline, reading only the line's last
// byte. The read is full-or-error: a short read means the file was truncated between
// the scan and now, which must abort the transform rather than misjudge the line
// shape. The byte lies inside a line the scanner already framed, so in practice it is
// always present.
func lineContentLen(f *os.File, origOff, origLen int64) (contentLen int64, hasNL bool, err error) {
	contentLen = origLen
	if contentLen <= 0 {
		return 0, false, nil
	}
	var last [1]byte
	if err := readAt(f, last[:], origOff+contentLen-1); err != nil {
		return 0, false, err
	}
	if last[0] == '\n' {
		return contentLen - 1, true, nil
	}
	return contentLen, false, nil
}

// transformPrefixDigest re-transforms the original file from byte zero, hashing the
// transformed output, and stops when the transformed length reaches wantTransformed.
// It returns the digest of the transformed prefix, the original offset that produced
// it, and ok=false when the transform cannot land exactly on wantTransformed (the
// boundaries diverged, so the local file no longer matches the server's stored
// bytes). It is the cold-cache verification path: deterministic because the
// transform is, so the recomputed prefix is byte identical to what was uploaded.
//
// The prefix verification only needs the transformed bytes, not their bodies, so it
// re-derives each rewritten line. A small line is rewritten whole; a big line is
// rewritten from its literal regions plus sentinels, streaming each body once only
// to recompute its hash, never buffering it.
//
// Recomputing those hashes re-compresses the bodies (the key is the hash of the
// compressed bytes, so there is no cheaper way to recover it). That is a deliberate
// tradeoff of the compressed-CAS design, not wasted work to optimize away: it spends
// client CPU on the cold-cache path so the server never compresses and stores no
// recovery state for the client. It runs only when the verified-prefix cache is cold
// (a fresh process or an evicted entry), and like the rest of the transform it
// streams in a fixed window, so the cost is bounded client CPU, never input-sized
// memory.
func transformPrefixDigest(ctx context.Context, f *os.File, agent string, size, wantTransformed int64, enc *casenc.Encoder) (hash.Hash, int64, bool, error) {
	sc := newOrigLineScanner(f, 0, size).withContext(ctx)
	h := sha256.New()
	var transformed, orig int64
	for transformed < wantTransformed {
		if err := ctx.Err(); err != nil {
			return nil, 0, false, err
		}
		line, origOff, origLen, isBig, ok, err := sc.next()
		if err != nil {
			return nil, 0, false, err
		}
		if !ok {
			return nil, 0, false, nil // ran out of file before reaching the cursor
		}
		rewritten, err := rewriteForDigest(ctx, f, agent, line, origOff, origLen, isBig, enc)
		if err != nil {
			return nil, 0, false, err
		}
		transformed += int64(len(rewritten))
		orig += origLen
		h.Write(rewritten)
		if transformed > wantTransformed {
			return nil, 0, false, nil // a line straddles the cursor: boundaries diverged
		}
	}
	return h, orig, true, nil
}

// rewriteForDigest produces the transformed bytes of one line for prefix
// verification, matching the bytes the transform uploaded. A big line is rewritten
// by streaming, identical to handleBigLine, but the body is only hashed (through the
// same encoder, so the key matches) rather than uploaded.
func rewriteForDigest(ctx context.Context, f *os.File, agent string, line []byte, origOff, origLen int64, isBig bool, enc *casenc.Encoder) ([]byte, error) {
	if !isBig {
		rewritten, _ := parser.RewriteLine(parser.Agent(agent), line, enc)
		return rewritten, nil
	}
	contentLen, hasNL, err := lineContentLen(f, origOff, origLen)
	if err != nil {
		return nil, err
	}
	var rewritten []byte
	cursor := int64(0)
	emit := func(loc parser.BodyLocation) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if loc.Span.Start < cursor || loc.Span.End > contentLen {
			return nil
		}
		reader := parser.CanonicalBodyReader(ctx, f, origOff, loc.Span, loc.Kind)
		sha, _, rawLen, err := enc.HashStream(ctx, reader)
		if err != nil {
			return err
		}
		rewritten, err = appendFileSpan(rewritten, f, agent, origOff+cursor, loc.Span.Start-cursor)
		if err != nil {
			return err
		}
		rewritten = append(rewritten, parser.SentinelBytes(sha, rawLen, loc.Media)...)
		cursor = loc.Span.End
		return nil
	}
	if err := parser.LocateToolBodies(ctx, parser.Agent(agent), f, origOff, contentLen, emit); err != nil {
		return nil, err
	}
	rewritten, err = appendFileSpan(rewritten, f, agent, origOff+cursor, contentLen-cursor)
	if err != nil {
		return nil, err
	}
	if hasNL {
		rewritten = append(rewritten, '\n')
	}
	return rewritten, nil
}

// origLineScanner yields complete JSONL lines from the original file starting at a
// byte offset, tracking each line's original offset and length so the assembler can
// map transformed chunks back to original spans. It reads in bounded windows and
// never returns a partial trailing line.
//
// It distinguishes small lines (returned with their bytes) from big lines (returned
// with isBig=true and a nil slice): a big line is one whose length exceeds
// bigLineThreshold, which the transform streams from the file rather than buffering.
// The scanner therefore never holds more than bigLineThreshold of a single line plus
// one read window, so one giant line cannot blow the memory budget.
type origLineScanner struct {
	ctx     context.Context
	f       *os.File
	size    int64
	pos     int64  // next original byte to read
	buf     []byte // unconsumed bytes already read from the file
	bufBase int64  // original offset of buf[0]
	scanned int    // bytes of buf already scanned for a newline (incremental cursor)
	done    bool

	// resumeSearch is an absolute file offset at or before which a previous tick
	// already searched the current (still-incomplete) trailing line for a newline and
	// found none. The newline search for the first line skips ahead to it, so a line
	// written over many appends is not re-searched from its start each tick. It is the
	// fix for the otherwise quadratic newline search of a growing partial line.
	resumeSearch int64
	// searchedTo records how far (absolute offset) the scanner has searched the
	// trailing line for a newline without finding one, so the next tick can resume
	// there. It is meaningful only when next() reported no complete line at EOF.
	searchedTo int64
}

func newOrigLineScanner(f *os.File, start, size int64) *origLineScanner {
	return &origLineScanner{ctx: context.Background(), f: f, size: size, pos: start, bufBase: start, resumeSearch: start}
}

// withResumeSearch tells the scanner a previous tick already searched the trailing
// line up to off without finding a newline, so the search can skip ahead to off.
func (s *origLineScanner) withResumeSearch(off int64) *origLineScanner {
	if off > s.resumeSearch {
		s.resumeSearch = off
	}
	return s
}

// withContext attaches a context so the scan-to-EOF loop over a huge line can honor
// cancellation. The transformer sets it from the sync's context.
func (s *origLineScanner) withContext(ctx context.Context) *origLineScanner {
	if ctx != nil {
		s.ctx = ctx
	}
	return s
}

// scanWindow is how many bytes the scanner pulls from the file at a time while
// looking for the next newline.
const scanWindow = 1 << 20

// next returns the next complete line. For a line at or below bigLineThreshold it
// returns the line bytes (including the trailing newline). For a bigger line it
// returns isBig=true with a nil slice: the caller streams it from the file by offset
// and length rather than receiving its bytes, so the scanner never buffers it whole.
// ok is false when no complete line remains.
//
// The newline search resumes from a cursor into the buffer rather than restarting at
// the buffer's head each call, so scanning a long run of lines is linear in the
// bytes read, not quadratic in the buffer length.
func (s *origLineScanner) next() (line []byte, origOff, origLen int64, isBig, ok bool, err error) {
	for {
		// Skip ahead to where a previous tick already searched this trailing line, so a
		// partial line continued across appends is not re-searched from its start.
		from := s.scanned
		if skip := int(s.resumeSearch - s.bufBase); skip > from {
			if skip > len(s.buf) {
				skip = len(s.buf)
			}
			from = skip
		}
		if rel := bytes.IndexByte(s.buf[from:], '\n'); rel >= 0 {
			end := from + rel + 1
			if int64(end) > bigLineThreshold {
				// The line is big even though it fits the buffer: hand it off by offset so
				// the caller streams it rather than receiving its bytes. This also covers a
				// small file read whole in one fill.
				lineStart := s.bufBase
				lineEnd := s.bufBase + int64(end)
				s.advancePast(lineEnd)
				return nil, lineStart, lineEnd - lineStart, true, true, nil
			}
			// Copy the line out so its lifetime is independent of the scan buffer: the
			// assembler retains the rewritten line (which, for a body-free line, is this
			// very slice) until its chunk flushes, while the scan buffer is compacted and
			// reused for later lines. The copy is one small line's worth (bounded by
			// bigLineThreshold), the same bytes we would hold anyway.
			line = append([]byte(nil), s.buf[:end]...)
			origOff = s.bufBase
			origLen = int64(end)
			s.consume(end)
			return line, origOff, origLen, false, true, nil
		}
		// No newline in the buffered bytes. If the buffer alone already exceeds the
		// big-line threshold, this line is big: find its end by streaming forward
		// from the file rather than growing the buffer.
		if int64(len(s.buf)) > bigLineThreshold {
			return s.takeBigLine()
		}
		if s.done {
			// EOF with an incomplete trailing line. Record how far it was searched so the
			// next tick resumes there rather than rescanning from the line start.
			s.searchedTo = s.bufBase + int64(len(s.buf))
			return nil, 0, 0, false, false, nil
		}
		s.scanned = len(s.buf) // everything buffered so far has been searched
		if err := s.fill(); err != nil {
			return nil, 0, 0, false, false, err
		}
	}
}

// takeBigLine handles a line whose length exceeds bigLineThreshold: it scans forward
// from the file to find the line's terminating newline (or EOF) without buffering
// the line, then drops the buffered head of the line and returns the line's offset
// and length for the caller to stream. A big line with no terminating newline is an
// incomplete trailing line: it is left unconsumed for a later tick.
func (s *origLineScanner) takeBigLine() (line []byte, origOff, origLen int64, isBig, ok bool, err error) {
	lineStart := s.bufBase
	// Search the buffered bytes first, skipping any prefix a previous tick already
	// searched, then the rest of the file in windows.
	bufFrom := s.scanned
	if skip := int(s.resumeSearch - s.bufBase); skip > bufFrom {
		if skip > len(s.buf) {
			skip = len(s.buf)
		}
		bufFrom = skip
	}
	searchFrom := s.bufBase + int64(bufFrom)
	if rel := bytes.IndexByte(s.buf[bufFrom:], '\n'); rel >= 0 {
		end := s.bufBase + int64(bufFrom+rel+1)
		s.advancePast(end)
		return nil, lineStart, end - lineStart, true, true, nil
	}
	scanPos := s.pos
	if searchFrom > scanPos {
		scanPos = searchFrom
	}
	if s.resumeSearch > scanPos {
		scanPos = s.resumeSearch
	}
	win := make([]byte, scanWindow)
	for scanPos < s.size {
		// A pathological line can span the whole file, so the scan-to-newline loop must
		// honor cancellation rather than read the entire file before noticing a shutdown.
		if err := s.ctx.Err(); err != nil {
			return nil, 0, 0, false, false, err
		}
		end := scanPos + scanWindow
		if end > s.size {
			end = s.size
		}
		w := win[:end-scanPos]
		if err := readAt(s.f, w, scanPos); err != nil {
			return nil, 0, 0, false, false, err
		}
		if rel := bytes.IndexByte(w, '\n'); rel >= 0 {
			lineEnd := scanPos + int64(rel) + 1
			s.advancePast(lineEnd)
			return nil, lineStart, lineEnd - lineStart, true, true, nil
		}
		scanPos = end
	}
	// Reached EOF without a newline: the big line is incomplete. Record how far it was
	// searched so the next tick resumes from there instead of rescanning from the line
	// start, which keeps the newline search over a growing big line linear, not
	// quadratic. Leave the line unconsumed.
	s.searchedTo = s.size
	return nil, 0, 0, false, false, nil
}

// advancePast resets the scanner to resume at file offset off, discarding any
// buffered bytes (they belonged to the big line just consumed). The next fill reads
// fresh from off.
func (s *origLineScanner) advancePast(off int64) {
	s.buf = s.buf[:0]
	s.bufBase = off
	s.pos = off
	s.scanned = 0
	s.done = false
}

// consume drops the first n bytes of the buffer (a returned small line), compacting
// the remaining tail to the front of the backing array so the buffer never drifts
// rightward and stays bounded by one read window plus a partial line regardless of
// file length. next() returns a copy of the line, so overwriting the backing array
// here is safe.
func (s *origLineScanner) consume(n int) {
	rest := s.buf[n:]
	s.buf = append(s.buf[:0], rest...)
	s.bufBase += int64(n)
	s.scanned = 0
}

// fill reads the next window of file bytes onto the buffer tail, marking done at EOF.
func (s *origLineScanner) fill() error {
	if s.pos >= s.size {
		s.done = true
		return nil
	}
	end := s.pos + scanWindow
	if end > s.size {
		end = s.size
	}
	chunk := make([]byte, end-s.pos)
	if err := readAt(s.f, chunk, s.pos); err != nil {
		return err
	}
	s.buf = append(s.buf, chunk...)
	s.pos = end
	if s.pos >= s.size {
		s.done = true
	}
	return nil
}

// completeEnd is the original offset just past the last complete line the scanner
// has produced (the start of any partial trailing line).
func (s *origLineScanner) completeEnd() int64 { return s.bufBase }

// chunkTarget is the preferred transformed-chunk size. A chunk is emitted once the
// boundary-aligned pending bytes reach it, so a long first sync moves in bounded
// pieces rather than one giant POST. It stays well under the server's maxChunk. It is
// a var so tests can shrink it to exercise multi-chunk emission on small inputs.
var chunkTarget = 1 << 20

// pendingLine is one rewritten transcript line held by the assembler: its
// transformed bytes, the original byte length it came from, and whether it closes a
// chunk boundary (every line for Claude and pi; a turn-closing user line for Codex).
type pendingLine struct {
	data       []byte
	origLen    int64
	isBoundary bool
}

// chunkAssembler groups rewritten lines into boundary-aligned, size-bounded
// transformed chunks, streaming them out one at a time. It tracks the index of the
// last boundary line incrementally (updated as lines arrive) so deciding whether a
// chunk is ready is O(1), not a backward scan of the pending lines. For Claude and
// pi every line is a boundary; for Codex a chunk closes only after a turn-ending
// user line, so lines accumulate until one arrives (or the trailing turn is flushed
// on settle).
type chunkAssembler struct {
	agent string
	file  string // session file path, for the oversized-turn warning
	lines []pendingLine

	lastBoundary  int   // index of the last boundary line in lines, or -1
	pendingBytes  int   // transformed bytes held in lines
	boundaryBytes int   // transformed bytes of lines [0, lastBoundary], the releasable prefix
	flushAll      bool  // finish() asked to flush every held line (settled / non-Codex tail)
	completeEnd   int64 // original offset of the start of any partial trailing line

	consumedOrigEnd int64 // original offset just past everything emitted so far
}

func newChunkAssembler(agent, file string, origStart int64) *chunkAssembler {
	return &chunkAssembler{agent: agent, file: file, lastBoundary: -1, consumedOrigEnd: origStart}
}

// adopt re-seeds the assembler from a cached open Codex turn without copying or
// recounting: it takes ownership of the cached lines slice and the precomputed
// counters, so resuming an open turn is O(1) rather than O(withheld lines).
// consumedOrigEnd is the original offset the held lines begin at, so committing them
// advances the cursor exactly as if they had just been produced.
func (a *chunkAssembler) adopt(prev *pendingTurn, origStart int64) {
	a.lines = prev.lines
	a.lastBoundary = prev.lastBoundary
	a.pendingBytes = prev.pendingBytes
	a.boundaryBytes = prev.boundaryBytes
	a.consumedOrigEnd = origStart
}

// add records one rewritten line, classifying whether it closes a chunk boundary and
// updating the last-boundary index in O(1).
func (a *chunkAssembler) add(rewritten []byte, origOff, origLen int64) {
	isBoundary := boundaryWithin(rewritten, 0, a.agent) > 0
	a.lines = append(a.lines, pendingLine{
		data:       rewritten,
		origLen:    origLen,
		isBoundary: isBoundary,
	})
	a.pendingBytes += len(rewritten)
	if isBoundary {
		a.lastBoundary = len(a.lines) - 1
		// The releasable prefix now extends through this boundary, so it is exactly
		// every byte held: lines after a boundary do not exist yet.
		a.boundaryBytes = a.pendingBytes
	}
}

// finish marks that no more lines are coming for this pass. An unclosed Codex
// trailing turn is withheld unless settled; for Claude and pi the last line is a
// boundary, so everything is already releasable. completeEnd is the original offset
// of the start of any partial trailing line, recorded as the consumed end when
// nothing remains held.
func (a *chunkAssembler) finish(settled bool, completeEnd int64) error {
	a.completeEnd = completeEnd
	if a.agent != agentCodex || settled {
		a.flushAll = true
	}
	return nil
}

// takeReady returns the next chunk ready to upload, cut at a boundary and held near
// chunkTarget, or ok=false when nothing is ready. With final=false it emits only
// once a boundary run reaches the size target; with final=true (after finish) it
// emits the remaining boundary-aligned lines, and the withheld open Codex turn if
// finish allowed flushing it.
//
// Deciding readiness is O(1): the chunk covers lines [0, cut], where cut is the last
// boundary index, and the size gate uses the running pendingBytes.
func (a *chunkAssembler) takeReady(final bool) (data []byte, origLen int64, ok bool) {
	if len(a.lines) == 0 {
		if final {
			// Nothing held: the consumed end is the start of the partial trailing line.
			a.consumedOrigEnd = a.completeEnd
		}
		return nil, 0, false
	}

	cut := a.lastBoundary
	if final && a.flushAll {
		// Flush every held line, boundary or not (the settled final Codex turn, or
		// Claude/pi where the last line is always a boundary).
		cut = len(a.lines) - 1
	}
	if cut < 0 {
		// No turn boundary yet. A Codex turn folds across lines, so normally the run is
		// withheld until its closing user line. The accepted bound: rewritten turns are
		// body-free and tiny (the 508 MiB image turn becomes a few MB of ref lines), so
		// truly bounding an arbitrarily long turn would require the server reducer to
		// fold a turn across regions, which it deliberately does not do (a chunk is
		// whole turns, so a region is always whole turns). Rather than rewrite the
		// reducer, cap the accumulated run by a constant: if a single open turn's
		// rewritten size somehow exceeds maxTurnBytes, emit a line-aligned partial chunk
		// as a hard backstop so worst-case memory is bounded by maxTurnBytes, not by
		// turn length. This sacrifices turn-alignment for that one pathological chunk; it
		// is not expected to fire in practice.
		if a.pendingBytes >= maxTurnBytes {
			return a.forcePartialFlush()
		}
		return nil, 0, false
	}
	if !final && a.boundaryBytes < chunkTarget {
		// Steady state withholds a sub-target releasable prefix so chunks stay near
		// chunkTarget; a boundary that crosses the target releases below. The size
		// gate reads the running boundary-prefix byte count, so it is O(1).
		return nil, 0, false
	}

	return a.cutChunk(cut)
}

// maxTurnBytes is the hard backstop on the rewritten bytes a single open Codex turn
// may accumulate before the assembler force-flushes it line-aligned. It converts the
// otherwise turn-length-bounded memory into a constant bound. It is generous (a
// rewritten turn is body-free) so it only fires for genuinely pathological input, and
// is a var so a test can shrink it to exercise the backstop.
var maxTurnBytes = 16 << 20

// forcePartialFlush emits the whole held run as one line-aligned chunk when an open
// turn has grown past maxTurnBytes without closing. It logs a warning naming the file
// because the emitted chunk is not turn-aligned, which the server reducer does not
// normally see; the warning makes the rare event visible rather than silent.
func (a *chunkAssembler) forcePartialFlush() (data []byte, origLen int64, ok bool) {
	last := len(a.lines) - 1
	slog.Warn("akari: forcing a non-turn-aligned partial chunk for an oversized open turn",
		"file", a.file, "rewritten_bytes", a.pendingBytes, "cap", maxTurnBytes)
	return a.cutChunk(last)
}

// cutChunk builds the chunk covering lines [0, cut], leaving the rest pending. It
// concatenates only the boundary-aligned prefix, so the returned slice is one
// chunk's worth of transformed bytes (at most one boundary-to-boundary run past the
// target), never the whole pending set.
func (a *chunkAssembler) cutChunk(cut int) (data []byte, origLen int64, ok bool) {
	total := 0
	for i := 0; i <= cut; i++ {
		total += len(a.lines[i].data)
	}
	data = make([]byte, 0, total)
	for i := 0; i <= cut; i++ {
		data = append(data, a.lines[i].data...)
		origLen += a.lines[i].origLen
	}
	return data, origLen, true
}

// commit drops the lines just emitted (those covered by the chunk takeReady cut),
// advancing the consumed original cursor and recomputing the last-boundary index for
// the lines that remain. It is called only after the sink accepts the chunk, so a
// conflicted upload leaves the assembler intact for the re-announce.
func (a *chunkAssembler) commit(origLen int64) {
	// Find how many lines the emitted origLen covered. The chunk was [0, cut]; recompute
	// cut by consuming origLen worth of lines from the head.
	consumed := int64(0)
	n := 0
	for n < len(a.lines) && consumed < origLen {
		consumed += a.lines[n].origLen
		n++
	}
	a.lines = append(a.lines[:0], a.lines[n:]...)
	a.consumedOrigEnd += origLen
	a.recountBoundary()
}

// recountBoundary recomputes the last-boundary index and pending byte count after a
// commit. This runs once per emitted chunk over the (small) remaining tail, not per
// line, so it does not reintroduce a per-line backward scan.
func (a *chunkAssembler) recountBoundary() {
	a.lastBoundary = -1
	a.pendingBytes = 0
	a.boundaryBytes = 0
	for i := range a.lines {
		a.pendingBytes += len(a.lines[i].data)
		if a.lines[i].isBoundary {
			a.lastBoundary = i
			a.boundaryBytes = a.pendingBytes
		}
	}
}
