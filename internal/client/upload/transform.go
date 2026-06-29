package upload

import (
	"bytes"
	"crypto/sha256"
	"hash"
	"os"

	"github.com/jssblck/akari/internal/parser"
)

// transformedChunk is one boundary-aligned span of the transformed transcript,
// ready to upload, together with the bodies lifted out of it and the original
// byte span it was produced from. The server stores transformed bytes at
// transformed offsets but tracks the original cursor, so the client reports both:
// it appends Data at the transformed offset and advances the original cursor by
// OrigLen.
type transformedChunk struct {
	Data    []byte        // transformed bytes to append
	Bodies  []parser.Body // tool bodies lifted from these bytes, deduped within the chunk
	OrigLen int64         // original bytes this chunk was produced from
}

// transformResult is the outcome of one transform pass over a span of the
// original file: zero or more complete chunks, plus how far the original cursor
// advanced past them. A pass may produce no chunk (only an incomplete or withheld
// trailing message remains), in which case the caller waits for more bytes.
type transformResult struct {
	chunks  []transformedChunk
	origEnd int64 // original offset just past the consumed, chunked bytes
}

// transformTail reads the original file from origStart, transforms each tool body
// into a CAS sentinel, and groups the rewritten lines into boundary-aligned
// chunks. Boundary detection runs on the rewritten bytes, which is sound because
// the rewrite is line preserving: each line keeps its newline and a Codex
// turn-closing user line keeps its shape, so the transformed stream has the same
// line and turn boundaries as the original. The bodies are reported per chunk so
// the caller uploads them to the CAS before appending the chunk that references
// them.
//
// settled lets the final, never-closed turn of an idle Codex session be flushed
// whole (mirroring nextChunk). The per-line read is bounded by hardCap so one
// pathological line cannot force an unbounded buffer; the transformed turn that
// motivated this work is small (its bulk lifted to the CAS), so the giant-turn
// case produces a small transcript chunk plus many body uploads.
func transformTail(f *os.File, origStart, origSize int64, agent string, settled bool) (transformResult, error) {
	sc := newOrigLineScanner(f, origStart, origSize)
	asm := newChunkAssembler(agent, origStart)
	var res transformResult

	for {
		line, origOff, origLen, ok, err := sc.next()
		if err != nil {
			return transformResult{}, err
		}
		if !ok {
			break
		}
		rewritten, bodies := parser.RewriteLine(parser.Agent(agent), line)
		if int64(len(rewritten)) > hardCap {
			return transformResult{}, errMessageTooBig(agent)
		}
		asm.add(rewritten, bodies, origOff, origLen)
		flushed, err := asm.takeReady()
		if err != nil {
			return transformResult{}, err
		}
		res.chunks = append(res.chunks, flushed...)
	}

	// The scanner consumed only complete lines; whatever it left (a partial
	// trailing line) is not chunked. The assembler may still hold complete lines
	// that form no boundary yet: for Codex an open trailing turn, withheld unless
	// the file has settled, in which case it is flushed whole.
	trailing, err := asm.finish(settled, sc.completeEnd())
	if err != nil {
		return transformResult{}, err
	}
	res.chunks = append(res.chunks, trailing...)
	res.origEnd = asm.consumedOrigEnd
	return res, nil
}

// transformPrefixDigest re-transforms the original file from byte zero, hashing
// the transformed output, and stops when the transformed length reaches
// wantTransformed. It returns the digest of the transformed prefix, the original
// offset that produced it, and ok=false when the transform cannot land exactly on
// wantTransformed (the boundaries diverged, so the local file no longer matches
// the server's stored bytes). It is the cold-cache verification path: deterministic
// because the transform is, so the recomputed prefix is byte identical to what was
// uploaded.
func transformPrefixDigest(f *os.File, agent string, size, wantTransformed int64) (hash.Hash, int64, bool, error) {
	sc := newOrigLineScanner(f, 0, size)
	h := sha256.New()
	var transformed, orig int64
	for transformed < wantTransformed {
		line, _, origLen, ok, err := sc.next()
		if err != nil {
			return nil, 0, false, err
		}
		if !ok {
			return nil, 0, false, nil // ran out of file before reaching the cursor
		}
		rewritten, _ := parser.RewriteLine(parser.Agent(agent), line)
		transformed += int64(len(rewritten))
		orig += origLen
		h.Write(rewritten)
		if transformed > wantTransformed {
			return nil, 0, false, nil // a line straddles the cursor: boundaries diverged
		}
	}
	return h, orig, true, nil
}

// origLineScanner yields complete JSONL lines from the original file starting at a
// byte offset, tracking each line's original offset and length so the assembler
// can map transformed chunks back to original spans. It reads in bounded windows
// and never returns a partial trailing line.
type origLineScanner struct {
	f       *os.File
	size    int64
	pos     int64  // next original byte to read
	buf     []byte // unconsumed bytes already read from the file
	bufBase int64  // original offset of buf[0]
	done    bool
}

func newOrigLineScanner(f *os.File, start, size int64) *origLineScanner {
	return &origLineScanner{f: f, size: size, pos: start, bufBase: start}
}

// scanWindow is how many bytes the scanner pulls from the file at a time while
// looking for the next newline. A line longer than this grows the read until the
// line completes or hardCap is hit.
const scanWindow = 1 << 20

// next returns the next complete line (including its trailing newline), its
// original offset, and its length. ok is false when no complete line remains.
//
// A single original line can be large (it holds a tool body the transform is
// about to lift to the CAS, the motivating big-body case), so the scanner does not
// cap the original line: it must read the whole line to extract its body. The cap
// that bounds memory is applied by transformTail to the REWRITTEN line, after the
// body has been lifted out, since that is the bytes that travel in the transcript.
func (s *origLineScanner) next() (line []byte, origOff, origLen int64, ok bool, err error) {
	for {
		if nl := bytes.IndexByte(s.buf, '\n'); nl >= 0 {
			end := nl + 1
			line = s.buf[:end]
			origOff = s.bufBase
			origLen = int64(end)
			s.buf = s.buf[end:]
			s.bufBase += int64(end)
			return line, origOff, origLen, true, nil
		}
		if s.done {
			return nil, 0, 0, false, nil
		}
		if err := s.fill(); err != nil {
			return nil, 0, 0, false, err
		}
	}
}

// fill reads the next window of file bytes onto the buffer tail, marking done at
// EOF.
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
// has produced (the start of any partial trailing line). The assembler reports it
// as the consumed original end when it flushes its held lines.
func (s *origLineScanner) completeEnd() int64 { return s.bufBase }

// chunkTarget is the preferred transformed-chunk size. A pass emits a chunk once
// the pending bytes reach it at a message boundary, so a long first sync moves in
// bounded pieces rather than one giant POST. It stays well under the server's
// 128 MiB maxChunk; a single transformed line larger than this (a body the
// transform somehow left inline) still rides alone, capped by hardCap.
const chunkTarget = 1 << 20

// pendingLine is one rewritten transcript line held by the assembler: its
// transformed bytes, the bodies lifted from it, the original byte length it came
// from, and whether it closes a chunk boundary (every line for Claude and pi; a
// turn-closing user line for Codex).
type pendingLine struct {
	data       []byte
	bodies     []parser.Body
	origLen    int64
	isBoundary bool
}

// chunkAssembler groups rewritten lines into boundary-aligned, size-bounded
// transformed chunks. It keeps the lines (not a flat buffer) so it can cut at any
// boundary, partitioning the bodies and the original span exactly. For Claude and
// pi every line is a boundary; for Codex a chunk closes only after a turn-ending
// user line, so lines accumulate until one arrives (or the trailing turn is
// flushed on settle).
type chunkAssembler struct {
	agent string
	lines []pendingLine

	consumedOrigEnd int64 // original offset just past everything emitted so far
}

func newChunkAssembler(agent string, origStart int64) *chunkAssembler {
	return &chunkAssembler{agent: agent, consumedOrigEnd: origStart}
}

// add records one rewritten line, classifying whether it closes a chunk boundary.
func (a *chunkAssembler) add(rewritten []byte, bodies []parser.Body, origOff, origLen int64) {
	a.lines = append(a.lines, pendingLine{
		data:       rewritten,
		bodies:     bodies,
		origLen:    origLen,
		isBoundary: boundaryWithin(rewritten, 0, a.agent) > 0,
	})
}

// takeReady emits chunks for the boundaries reached so far, each cut at a boundary
// line and held near chunkTarget. It always leaves the tail after the last
// boundary pending (an open Codex turn, or nothing for Claude/pi since their last
// line is always a boundary), so a partial turn is never sent.
func (a *chunkAssembler) takeReady() ([]transformedChunk, error) {
	lastBoundary := -1
	for i := len(a.lines) - 1; i >= 0; i-- {
		if a.lines[i].isBoundary {
			lastBoundary = i
			break
		}
	}
	if lastBoundary < 0 {
		return nil, nil // no complete boundary yet
	}
	return a.emitUpTo(lastBoundary), nil
}

// finish emits the remaining lines once no more are coming. An unclosed Codex
// trailing turn is withheld unless settled; for Claude and pi the last line is a
// boundary, so everything emits. completeEnd is the original offset of the start of
// any partial trailing line, recorded as the consumed end when nothing is held.
func (a *chunkAssembler) finish(settled bool, completeEnd int64) ([]transformedChunk, error) {
	if len(a.lines) == 0 {
		a.consumedOrigEnd = completeEnd
		return nil, nil
	}
	if a.agent != agentCodex || settled {
		// Flush every held line, boundary or not (the settled final turn).
		return a.emitUpTo(len(a.lines) - 1), nil
	}
	// Hold an open trailing turn: emit only through the last real boundary.
	chunks, err := a.takeReady()
	if err != nil {
		return nil, err
	}
	return chunks, nil
}

// emitUpTo produces chunks covering lines [0, last], starting a new chunk once the
// accumulated bytes reach chunkTarget at a boundary so no chunk grows without
// bound. The size check is applied BEFORE appending the next line, so a large run
// between boundaries (in the limit, one line near hardCap) always lands in its own
// chunk rather than being tacked onto a full one: a chunk is at most one
// boundary-to-boundary run, which keeps it under the server's maxChunk. Lines past
// last stay pending. Bodies and the original span are partitioned per chunk so each
// reports exactly what it carries.
func (a *chunkAssembler) emitUpTo(last int) []transformedChunk {
	var chunks []transformedChunk
	var cur transformedChunk
	var curBytes int
	curEndsBoundary := false

	flush := func() {
		if len(cur.Data) == 0 {
			return
		}
		chunks = append(chunks, cur)
		a.consumedOrigEnd += cur.OrigLen
		cur = transformedChunk{}
		curBytes = 0
		curEndsBoundary = false
	}

	for i := 0; i <= last; i++ {
		ln := a.lines[i]
		// Cut before adding the next line, but only where the accumulator already rests
		// on a boundary, so a turn is never split and a fresh large line starts clean.
		if curEndsBoundary && curBytes >= chunkTarget {
			flush()
		}
		cur.Data = append(cur.Data, ln.data...)
		cur.Bodies = append(cur.Bodies, ln.bodies...)
		cur.OrigLen += ln.origLen
		curBytes += len(ln.data)
		curEndsBoundary = ln.isBoundary
	}
	flush()

	a.lines = append(a.lines[:0], a.lines[last+1:]...)
	return chunks
}
