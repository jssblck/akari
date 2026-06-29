package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"hash"
	"io"
	"os"

	"github.com/jssblck/akari/internal/parser"
)

// chunkSink consumes the transform's output as it is produced. The transform never
// holds more than one chunk's worth of transcript bytes in memory: as soon as a
// boundary-aligned, size-bounded chunk is ready it is handed here, uploaded, and
// dropped. emitBody streams one tool body to the CAS during the transform of a big
// line, so a hundreds-of-MiB body never lands in a chunk buffer; the chunk carries
// only the small literal regions and the sentinels that replace the bodies.
//
// emitBody is called for every body the transform lifts, before the chunk that
// references it is emitted, so the transcript can never land referencing a body the
// CAS does not yet hold. It returns the deduped body descriptor (sha, length,
// media) the sentinel is built from.
type chunkSink interface {
	// emitBody streams one located body to the CAS (skipping the upload when the
	// server already holds it) and returns its content hash, byte length, and media
	// type. content, when non-nil, is the already-buffered body bytes of a small
	// line; when nil the body is streamed from the file via the reader factory.
	emitBody(ctx context.Context, ref bodyRef) (parser.Body, error)
	// emitChunk uploads one boundary-aligned transformed chunk and reports how far
	// the original cursor advanced. It returns conflicted=true when the server's
	// cursor moved, which unwinds the whole transform.
	emitChunk(ctx context.Context, data []byte, origLen int64) (conflicted bool, err error)
}

// bodyRef points at one tool body to be lifted to the CAS, either as bytes already
// in hand (a small line buffered whole) or as a file span to be streamed (a big
// line). Exactly one of content / (file, span, kind) is set.
type bodyRef struct {
	media string
	kind  string // "input" | "result", for diagnostics

	// Small-line path: the canonical body bytes, already extracted by RewriteLine.
	content     []byte
	haveContent bool

	// Big-line path: where to stream the canonical body from, never buffered whole.
	file     *os.File
	lineOff  int64
	span     parser.ValueSpan
	bodyKind parser.BodyKind
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

	sc  *origLineScanner
	asm *chunkAssembler
}

// newTransformer builds a transformer reading original [origStart, size).
func newTransformer(f *os.File, origStart, size int64, agent string, sink chunkSink) *transformer {
	return &transformer{
		f:     f,
		agent: agent,
		size:  size,
		sink:  sink,
		sc:    newOrigLineScanner(f, origStart, size),
		asm:   newChunkAssembler(agent, origStart),
	}
}

// run scans the unsent tail, transforming each line and emitting chunks to the sink
// as boundaries are reached. It returns the original offset just past everything the
// server accepted and conflicted=true when a chunk upload hit an offset conflict
// (the caller re-announces). Memory stays bounded: at most one chunk's worth of
// transformed bytes plus one line's small head, with big bodies streamed straight
// through to the CAS.
func (t *transformer) run(ctx context.Context, settled bool) (origEnd int64, conflicted bool, err error) {
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
// each lifted body. The body bytes are already in hand, so emitBody streams from
// them without a second copy.
func (t *transformer) handleSmallLine(ctx context.Context, line []byte, origOff, origLen int64) error {
	rewritten, bodies := parser.RewriteLine(parser.Agent(t.agent), line)
	if int64(len(rewritten)) > hardCap {
		return errMessageTooBig(t.agent)
	}
	for _, b := range bodies {
		if _, err := t.sink.emitBody(ctx, bodyRef{
			media:       b.MediaType,
			kind:        b.Kind,
			content:     []byte(b.Content),
			haveContent: true,
		}); err != nil {
			return err
		}
	}
	t.asm.add(rewritten, origOff, origLen)
	return nil
}

// handleBigLine transforms a line too large to buffer. It locates each tool body's
// raw span by streaming the line, streams each body to the CAS (so the body itself
// is never resident), and assembles the rewritten line from the small literal
// regions between bodies plus the sentinels. The rewritten line is small (its bulk
// lifted to the CAS), so it is bounded by hardCap like any other.
func (t *transformer) handleBigLine(ctx context.Context, origOff, origLen int64) error {
	// The line spans [origOff, origOff+origLen) in the file, trailing newline
	// included. Locate bodies over the content without the newline so a span never
	// includes it.
	contentLen := origLen
	hasNL := false
	if contentLen > 0 {
		var last [1]byte
		if _, err := t.f.ReadAt(last[:], origOff+contentLen-1); err != nil && err != io.EOF {
			return err
		}
		if last[0] == '\n' {
			contentLen--
			hasNL = true
		}
	}

	locs, err := parser.LocateToolBodies(parser.Agent(t.agent), t.f, origOff, contentLen)
	if err != nil {
		return err
	}
	if len(locs) == 0 {
		// A big line with no tool body cannot be lifted; it must ride inline, but it
		// exceeds hardCap by definition of "big". Refuse it rather than buffer it.
		return errMessageTooBig(t.agent)
	}

	var rewritten []byte
	cursor := int64(0)
	for _, loc := range locs {
		if loc.Span.Start < cursor || loc.Span.End > contentLen {
			continue // a span out of order or past the line: skip defensively
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
		rewritten = appendFileSpan(rewritten, t.f, origOff+cursor, loc.Span.Start-cursor)
		rewritten = append(rewritten, sentinelFor(body)...)
		cursor = loc.Span.End
		if int64(len(rewritten)) > hardCap {
			return errMessageTooBig(t.agent)
		}
	}
	rewritten = appendFileSpan(rewritten, t.f, origOff+cursor, contentLen-cursor)
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

// appendFileSpan appends n bytes read from f at off to dst. It is used to copy the
// small literal regions of a big line (the JSON structure around the bodies) into
// the rewritten line; n is bounded by the gaps between bodies, which are small.
func appendFileSpan(dst []byte, f *os.File, off, n int64) []byte {
	if n <= 0 {
		return dst
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		// A read error here is surfaced by the surrounding emitChunk failing on a
		// short/garbled line; appending nothing keeps the slice valid meanwhile.
		return dst
	}
	return append(dst, buf...)
}

// sentinelFor renders the CAS reference that replaces a body, reusing the parser's
// canonical encoding so the bytes match what RewriteLine produces for a small line.
func sentinelFor(b parser.Body) []byte {
	return parser.SentinelBytes(b.SHA256, b.Bytes, b.MediaType)
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
func transformPrefixDigest(ctx context.Context, f *os.File, agent string, size, wantTransformed int64) (hash.Hash, int64, bool, error) {
	sc := newOrigLineScanner(f, 0, size)
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
		rewritten, err := rewriteForDigest(f, agent, line, origOff, origLen, isBig)
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
// by streaming, identical to handleBigLine, but the hashing is done locally rather
// than through the sink.
func rewriteForDigest(f *os.File, agent string, line []byte, origOff, origLen int64, isBig bool) ([]byte, error) {
	if !isBig {
		rewritten, _ := parser.RewriteLine(parser.Agent(agent), line)
		return rewritten, nil
	}
	contentLen := origLen
	hasNL := false
	if contentLen > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], origOff+contentLen-1); err != nil && err != io.EOF {
			return nil, err
		}
		if last[0] == '\n' {
			contentLen--
			hasNL = true
		}
	}
	locs, err := parser.LocateToolBodies(parser.Agent(agent), f, origOff, contentLen)
	if err != nil {
		return nil, err
	}
	var rewritten []byte
	cursor := int64(0)
	for _, loc := range locs {
		if loc.Span.Start < cursor || loc.Span.End > contentLen {
			continue
		}
		sha, n, err := hashBodySpan(f, origOff, loc.Span, loc.Kind)
		if err != nil {
			return nil, err
		}
		rewritten = appendFileSpan(rewritten, f, origOff+cursor, loc.Span.Start-cursor)
		rewritten = append(rewritten, parser.SentinelBytes(sha, n, loc.Media)...)
		cursor = loc.Span.End
	}
	rewritten = appendFileSpan(rewritten, f, origOff+cursor, contentLen-cursor)
	if hasNL {
		rewritten = append(rewritten, '\n')
	}
	return rewritten, nil
}

// hashBodySpan streams a located body through its canonical reader to compute its
// sha256 and byte length without buffering it. It is the shared primitive both the
// upload path and the digest path use, so the hash a sentinel carries is identical
// in both.
func hashBodySpan(f *os.File, lineOff int64, span parser.ValueSpan, kind parser.BodyKind) (string, int, error) {
	r := parser.CanonicalBodyReader(f, lineOff, span, kind)
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return parser.HexDigest(h.Sum(nil)), int(n), nil
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
	f       *os.File
	size    int64
	pos     int64  // next original byte to read
	buf     []byte // unconsumed bytes already read from the file
	bufBase int64  // original offset of buf[0]
	scanned int    // bytes of buf already scanned for a newline (incremental cursor)
	done    bool
}

func newOrigLineScanner(f *os.File, start, size int64) *origLineScanner {
	return &origLineScanner{f: f, size: size, pos: start, bufBase: start}
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
		if rel := bytes.IndexByte(s.buf[s.scanned:], '\n'); rel >= 0 {
			end := s.scanned + rel + 1
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
	// Search the bytes already buffered first, then the rest of the file in windows.
	searchFrom := s.bufBase + int64(s.scanned)
	if rel := bytes.IndexByte(s.buf[s.scanned:], '\n'); rel >= 0 {
		end := s.bufBase + int64(s.scanned+rel+1)
		s.advancePast(end)
		return nil, lineStart, end - lineStart, true, true, nil
	}
	scanPos := s.pos
	if searchFrom > scanPos {
		scanPos = searchFrom
	}
	for scanPos < s.size {
		end := scanPos + scanWindow
		if end > s.size {
			end = s.size
		}
		win := make([]byte, end-scanPos)
		if err := readAt(s.f, win, scanPos); err != nil {
			return nil, 0, 0, false, false, err
		}
		if rel := bytes.IndexByte(win, '\n'); rel >= 0 {
			lineEnd := scanPos + int64(rel) + 1
			s.advancePast(lineEnd)
			return nil, lineStart, lineEnd - lineStart, true, true, nil
		}
		scanPos = end
	}
	// Reached EOF without a newline: the big line is incomplete. Leave it unconsumed.
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
	lines []pendingLine

	lastBoundary  int   // index of the last boundary line in lines, or -1
	pendingBytes  int   // transformed bytes held in lines
	boundaryBytes int   // transformed bytes of lines [0, lastBoundary], the releasable prefix
	flushAll      bool  // finish() asked to flush every held line (settled / non-Codex tail)
	completeEnd   int64 // original offset of the start of any partial trailing line

	consumedOrigEnd int64 // original offset just past everything emitted so far
}

func newChunkAssembler(agent string, origStart int64) *chunkAssembler {
	return &chunkAssembler{agent: agent, lastBoundary: -1, consumedOrigEnd: origStart}
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
		return nil, 0, false // no boundary yet, and not flushing all
	}
	if !final && a.boundaryBytes < chunkTarget {
		// Steady state withholds a sub-target releasable prefix so chunks stay near
		// chunkTarget; a boundary that crosses the target releases below. The size
		// gate reads the running boundary-prefix byte count, so it is O(1).
		return nil, 0, false
	}

	return a.cutChunk(cut)
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
