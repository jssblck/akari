package parser

import (
	"bytes"
	"io"
)

// BodyKind selects how the raw located value is canonicalized into the bytes the
// CAS stores. The three kinds mirror the branches of bodyContent: a value copied
// verbatim, a JSON string whose decoded contents are the body, and an array of
// blocks flattened to its text.
type BodyKind int

const (
	// BodyRaw copies the value's raw bytes verbatim. This is the canonical form
	// for genuine objects (a result whose body is a JSON object) and for the
	// claude/pi tool input, where the stored body is exactly input.Raw.
	BodyRaw BodyKind = iota
	// BodyJSONString treats the value as a JSON string and emits its decoded
	// contents, matching gjson .String(). This is the canonical form for a string
	// result body and for codex arguments (a JSON-encoded string).
	BodyJSONString
	// BodyArrayText treats the value as an array of typed blocks and emits the
	// blockText flattening: the decoded text of each contributing element joined
	// by a single newline.
	BodyArrayText
)

// CanonicalBodyReader returns an io.Reader that streams the canonical body bytes
// for a value located at span [Start,End) within a line that begins at lineOffset
// in f. The reader pulls from f lazily and holds only a bounded window, never the
// whole body, so a tool body of hundreds of MiB streams through O(window) memory
// rather than being buffered. The bytes it yields are byte-identical to what
// bodyContent / setToolInput would store inline today, which is what lets the hash
// match the server's.
//
// kind selects the canonicalization:
//   - BodyRaw: the raw span, copied verbatim.
//   - BodyJSONString: the section inside the quotes, JSON-string-decoded on the fly.
//   - BodyArrayText: the array's contributing blocks, decoded and newline-joined.
func CanonicalBodyReader(f io.ReaderAt, lineOffset int64, span ValueSpan, kind BodyKind) io.Reader {
	switch kind {
	case BodyJSONString:
		return newJSONStringReader(f, lineOffset+span.Start, lineOffset+span.End)
	case BodyArrayText:
		return newArrayTextReader(f, lineOffset, span)
	default: // BodyRaw
		// A raw value is its source bytes unchanged; a section reader streams them
		// straight from the file with no buffering of our own.
		return io.NewSectionReader(f, lineOffset+span.Start, span.End-span.Start)
	}
}

// ClassifyResultBody peeks the first byte of a result value's raw span to choose
// the canonicalization kind and media type, matching bodyContent's switch without
// reading the whole value. firstByte is line[span.Start], the first structural
// byte of the value (results are located at the value's own start, so there is no
// leading whitespace to skip).
//
// The mapping mirrors bodyContent exactly:
//   - '"' -> a JSON string: decoded contents, text/plain.
//   - '[' -> an array of blocks: blockText flattening, text/plain.
//   - '{' -> a JSON object: raw JSON, application/json.
//   - anything else (a bare scalar) -> raw, text/plain.
//
// The absent-value case (empty body, empty media) is the caller's concern: a
// classifier needs a byte to look at, so callers handle "no value located"
// before reaching here.
func ClassifyResultBody(firstByte byte) (BodyKind, string) {
	switch firstByte {
	case '"':
		return BodyJSONString, "text/plain"
	case '[':
		return BodyArrayText, "text/plain"
	case '{':
		return BodyRaw, "application/json"
	default:
		return BodyRaw, "text/plain"
	}
}

// jsonStringReader streams the decoded contents of a JSON string value, reading
// the bytes between the surrounding quotes from the file in bounded chunks and
// resolving escape sequences on the fly. It never holds the whole string: only a
// read window plus a few bytes of carried escape state cross a chunk boundary.
//
// The decoding matches gjson .String(): \" \\ \/ \b \f \n \r \t become their
// literal bytes and \uXXXX becomes the UTF-8 encoding of the code point (surrogate
// pairs combined). Any other escape passes the following byte through verbatim,
// which is gjson's lenient behavior for input assumed well-formed.
type jsonStringReader struct {
	f   io.ReaderAt
	pos int64 // next file offset to read from (within the quotes)
	end int64 // offset just past the last content byte (the closing quote)

	in  []byte // raw bytes pulled from the file, not yet decoded
	out []byte // decoded bytes ready to hand to Read, FIFO via outPos
	off int    // read cursor into out

	// Carried escape state across chunk boundaries. pendingEsc is set after a lone
	// backslash; pendingU collects the hex digits of a \uXXXX still being read;
	// highSurrogate holds a leading surrogate awaiting its trailing pair.
	pendingEsc    bool
	pendingU      []byte
	highSurrogate rune

	err error
}

// newJSONStringReader builds a reader over the string value whose raw span is
// [rawStart,rawEnd) in f, where rawStart points at the opening quote. The decoded
// contents live in (rawStart, rawEnd-1), i.e. between the quotes.
func newJSONStringReader(f io.ReaderAt, rawStart, rawEnd int64) *jsonStringReader {
	return &jsonStringReader{
		f:   f,
		pos: rawStart + 1, // skip the opening quote
		end: rawEnd - 1,   // stop before the closing quote
	}
}

const jsonStringChunk = 64 * 1024

func (r *jsonStringReader) Read(p []byte) (int, error) {
	for len(r.out) == r.off {
		// No decoded bytes buffered; decode another chunk. Reset the buffer when it
		// has been fully drained so it does not grow without bound.
		r.out = r.out[:0]
		r.off = 0
		if r.err != nil {
			return 0, r.err
		}
		if r.pos >= r.end {
			// All content bytes consumed; the only thing left is to surface EOF.
			r.err = io.EOF
			return 0, io.EOF
		}
		if err := r.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.out[r.off:])
	r.off += n
	return n, nil
}

// fill pulls the next raw chunk from the file and decodes it into r.out, carrying
// any partial escape that straddles the chunk boundary.
func (r *jsonStringReader) fill() error {
	want := r.end - r.pos
	if want > jsonStringChunk {
		want = jsonStringChunk
	}
	if cap(r.in) < int(want) {
		r.in = make([]byte, want)
	}
	buf := r.in[:want]
	n, err := r.f.ReadAt(buf, r.pos)
	r.pos += int64(n)
	if n == 0 {
		if err == nil {
			err = io.EOF
		}
		r.err = err
		return err
	}
	r.decode(buf[:n])
	// A short read that also reported an error other than the natural end is
	// fatal; otherwise keep going and let the pos>=end check end the stream.
	if err != nil && err != io.EOF {
		r.err = err
	}
	return nil
}

// decode appends the JSON-decoded form of the raw chunk b to r.out, resuming from
// any escape state carried across the previous boundary.
func (r *jsonStringReader) decode(b []byte) {
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case r.pendingU != nil:
			// Mid \uXXXX: accumulate hex digits until four are collected.
			r.pendingU = append(r.pendingU, c)
			if len(r.pendingU) == 4 {
				r.emitU(decodeHex4(r.pendingU))
				r.pendingU = nil
			}
		case r.pendingEsc:
			r.pendingEsc = false
			switch c {
			case '"':
				r.flushSurrogate()
				r.out = append(r.out, '"')
			case '\\':
				r.flushSurrogate()
				r.out = append(r.out, '\\')
			case '/':
				r.flushSurrogate()
				r.out = append(r.out, '/')
			case 'b':
				r.flushSurrogate()
				r.out = append(r.out, '\b')
			case 'f':
				r.flushSurrogate()
				r.out = append(r.out, '\f')
			case 'n':
				r.flushSurrogate()
				r.out = append(r.out, '\n')
			case 'r':
				r.flushSurrogate()
				r.out = append(r.out, '\r')
			case 't':
				r.flushSurrogate()
				r.out = append(r.out, '\t')
			case 'u':
				r.pendingU = make([]byte, 0, 4)
			default:
				// Lenient passthrough for an unknown escape, matching gjson.
				r.flushSurrogate()
				r.out = append(r.out, c)
			}
		case c == '\\':
			r.pendingEsc = true
		default:
			r.flushSurrogate()
			r.out = append(r.out, c)
		}
	}
}

// emitU handles a decoded \uXXXX code point, combining a surrogate pair when one
// is pending so astral-plane characters round-trip as gjson encodes them.
func (r *jsonStringReader) emitU(cp rune) {
	if r.highSurrogate != 0 {
		if cp >= 0xDC00 && cp <= 0xDFFF {
			combined := 0x10000 + (r.highSurrogate-0xD800)<<10 + (cp - 0xDC00)
			r.highSurrogate = 0
			r.out = appendRune4(r.out, combined)
			return
		}
		// A high surrogate not followed by a low one: emit it as-is, then handle
		// the current code point afresh.
		r.out = appendRune4(r.out, r.highSurrogate)
		r.highSurrogate = 0
	}
	if cp >= 0xD800 && cp <= 0xDBFF {
		r.highSurrogate = cp
		return
	}
	r.out = appendRune4(r.out, cp)
}

// flushSurrogate emits a lone high surrogate that was awaiting a trailing pair
// when the next byte turns out not to be one. Called before appending any
// non-surrogate output so an unpaired surrogate keeps its position.
func (r *jsonStringReader) flushSurrogate() {
	if r.highSurrogate != 0 {
		r.out = appendRune4(r.out, r.highSurrogate)
		r.highSurrogate = 0
	}
}

// appendRune4 appends the UTF-8 encoding of r, covering the full range including
// astral-plane code points (four-byte sequences) that combined surrogate pairs
// produce. The three-byte helper in jsonspan.go only needs the BMP for keys; this
// one is used for arbitrary string content.
func appendRune4(out []byte, r rune) []byte {
	switch {
	case r < 0x80:
		return append(out, byte(r))
	case r < 0x800:
		return append(out, byte(0xC0|r>>6), byte(0x80|r&0x3F))
	case r < 0x10000:
		return append(out, byte(0xE0|r>>12), byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
	default:
		return append(out,
			byte(0xF0|r>>18),
			byte(0x80|(r>>12)&0x3F),
			byte(0x80|(r>>6)&0x3F),
			byte(0x80|r&0x3F))
	}
}

// arrayPiece names one element's contribution to blockText: a span (relative to
// the line) whose decoded string contents are the piece. Only contributing
// elements (a bare string element, or an object block whose type is text-like)
// produce a piece; everything else is skipped, exactly like blockText.
type arrayPiece struct {
	span ValueSpan
}

// newArrayTextReader builds a reader that streams blockText over the array whose
// raw span is span within the line beginning at lineOffset in f. It enumerates the
// contributing element spans up front (a streaming pass per index, reading only
// the tiny type strings into memory), then chains a decoding reader per piece with
// a literal newline between pieces, so the big text bodies still stream one at a
// time and are never all resident together.
func newArrayTextReader(f io.ReaderAt, lineOffset int64, span ValueSpan) io.Reader {
	pieces, err := enumerateArrayPieces(f, lineOffset, span)
	if err != nil {
		return &errReader{err: err}
	}
	readers := make([]io.Reader, 0, len(pieces)*2)
	for i, pc := range pieces {
		if i > 0 {
			// strings.Join(parts, "\n"): a separator between pieces, never leading
			// or trailing.
			readers = append(readers, bytes.NewReader([]byte("\n")))
		}
		readers = append(readers, newJSONStringReader(f, lineOffset+pc.span.Start, lineOffset+pc.span.End))
	}
	return io.MultiReader(readers...)
}

// enumerateArrayPieces walks the array element by element and records the span of
// each contributing piece. For each index i it locates, in one streaming pass over
// the array region, the element (Idx i), its type (Idx i then Key "type"), and its
// text (Idx i then Key "text"). A bare string element contributes its own span; an
// object element contributes its text span only when its type is one of the
// text-like kinds. The loop stops at the first index whose element is absent, so
// the array length need not be known ahead of time.
//
// Spans returned by LocateValues are relative to the array region's first byte, so
// they are rebased by span.Start to become line-relative like the caller expects.
func enumerateArrayPieces(f io.ReaderAt, lineOffset int64, span ValueSpan) ([]arrayPiece, error) {
	var pieces []arrayPiece
	for i := 0; ; i++ {
		paths := [][]Step{
			{Idx(i)},              // 0: the element value
			{Idx(i), Key("type")}, // 1: its type, if an object
			{Idx(i), Key("text")}, // 2: its text, if an object block
		}
		located, err := LocateValues(paths, sectionNext(f, lineOffset+span.Start, span.End-span.Start))
		if err != nil {
			return nil, err
		}
		var elem, typ, text *ValueSpan
		for _, ls := range located {
			s := ls.Span
			switch ls.PathIndex {
			case 0:
				v := s
				elem = &v
			case 1:
				v := s
				typ = &v
			case 2:
				v := s
				text = &v
			}
		}
		if elem == nil {
			break // no element at this index: end of the array
		}
		// A bare string element (no nested type) is itself the text, matching
		// blockText's b.Type == String branch.
		if typ == nil {
			if isStringSpan(f, lineOffset+span.Start+elem.Start) {
				pieces = append(pieces, arrayPiece{span: rebase(*elem, span.Start)})
			}
			continue
		}
		// An object block contributes its text only for a text-like type.
		kind, err := readSpanString(f, lineOffset+span.Start, *typ)
		if err != nil {
			return nil, err
		}
		switch kind {
		case "text", "output_text", "input_text":
			if text != nil {
				pieces = append(pieces, arrayPiece{span: rebase(*text, span.Start)})
			}
		}
	}
	return pieces, nil
}

// rebase shifts an array-region-relative span to be line-relative by adding the
// array's own start offset.
func rebase(s ValueSpan, base int64) ValueSpan {
	return ValueSpan{Start: s.Start + base, End: s.End + base}
}

// isStringSpan reports whether the value at fileOff begins with a quote, i.e. it
// is a JSON string. A bare string array element is detected this way so it can be
// decoded as a piece.
func isStringSpan(f io.ReaderAt, fileOff int64) bool {
	var one [1]byte
	if _, err := f.ReadAt(one[:], fileOff); err != nil && err != io.EOF {
		return false
	}
	return one[0] == '"'
}

// readSpanString decodes a small JSON string value (a block's type) fully into
// memory. Only structural strings flow through here; the large text bodies stream
// via newJSONStringReader instead, so this bounded read does not break the memory
// budget.
func readSpanString(f io.ReaderAt, base int64, rel ValueSpan) (string, error) {
	rd := newJSONStringReader(f, base+rel.Start, base+rel.End)
	data, err := io.ReadAll(rd)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// sectionNext adapts a file section into the chunked next() that LocateValues
// consumes, reading in bounded windows so the locator stays O(window) over the
// array region rather than buffering it.
func sectionNext(f io.ReaderAt, start, length int64) func() ([]byte, error) {
	sr := io.NewSectionReader(f, start, length)
	buf := make([]byte, jsonStringChunk)
	return func() ([]byte, error) {
		n, err := sr.Read(buf)
		if n > 0 {
			return buf[:n], nil
		}
		return nil, err
	}
}

// errReader yields a fixed error on every Read, so a failure during piece
// enumeration surfaces through the normal io.Reader contract rather than panicking
// or silently truncating.
type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }
