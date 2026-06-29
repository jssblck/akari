package parser

import (
	"errors"
	"io"
)

// ValueSpan is the byte range [Start,End) of one located JSON value within a
// single JSONL line, relative to the line's first byte (offset 0 = first byte of
// the line). It matches gjson's value Index and Index+len(Raw) exactly.
type ValueSpan struct {
	Start int64
	End   int64
}

// Step is one segment of a path into a JSON document: either an object Key or an
// array Idx. The marker method keeps the two step kinds in a single closed set
// so callers cannot smuggle in an arbitrary type.
type Step interface{ isStep() }

// Key selects an object member by name.
type Key string

// Idx selects an array element by its 0-based position.
type Idx int

func (Key) isStep() {}
func (Idx) isStep() {}

// LocatedSpan pairs a located value's span with the index of the path that
// produced it, so callers can correlate results back to their request even when
// some paths are absent and skipped.
type LocatedSpan struct {
	PathIndex int
	Span      ValueSpan
}

// LocateValues scans the JSONL line exactly once, streaming, and returns the
// byte span of every requested path that is present, in source order (the order
// the values appear in the line, which for distinct leaf paths is also request
// order for well-formed transcripts). Absent paths are skipped.
//
// The motivation is lifting very large tool-call bodies (a single JSON value,
// possibly hundreds of MiB) out of a transcript line without ever buffering the
// whole line or the whole value. The returned spans are byte-identical to
// gjson's value .Index (Start) and .Index+len(.Raw) (End), so the exact bytes
// they delimit can be sha256'd to match what the server stored.
//
// next supplies the line incrementally: each call returns the next chunk of
// bytes (any size greater than zero) until it reports io.EOF. The scanner is
// correct for any chunking, including a single byte per call, and it retains
// only O(path depth) state plus the constant overhead of one chunk at a time. It
// never accumulates the bytes of a located value.
func LocateValues(paths [][]Step, next func() ([]byte, error)) ([]LocatedSpan, error) {
	sc := newScanner(paths)
	for {
		chunk, err := next()
		for _, b := range chunk {
			sc.feed(b)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		// A nil/empty chunk with a nil error is permitted; just ask again.
	}
	sc.finish()
	return sc.results, nil
}

// LocateValue is the single-path convenience wrapper over LocateValues. ok is
// false when the path is absent from the line.
func LocateValue(path []Step, next func() ([]byte, error)) (ValueSpan, bool, error) {
	res, err := LocateValues([][]Step{path}, next)
	if err != nil {
		return ValueSpan{}, false, err
	}
	if len(res) == 0 {
		return ValueSpan{}, false, nil
	}
	return res[0].Span, true, nil
}

// frame is one level of the JSON container stack. It records what kind of
// container the scanner is inside and the structural position needed to match
// path steps: for objects, the key of the member whose value is being scanned;
// for arrays, the index of the element being scanned.
type frame struct {
	isObject bool
	// curKey is the most recently completed object key, valid while scanning the
	// value that follows it. Meaningful only when isObject.
	curKey string
	// arrIdx is the 0-based index of the array element currently being scanned.
	// It starts at -1 and increments as each element value begins. Meaningful
	// only when !isObject.
	arrIdx int
}

// pendingValue tracks a path whose value we are currently inside, so we can
// record its End once the value closes. depth is the container-stack depth at
// which the value lives (the depth of its enclosing container), used to detect
// when a container value has fully closed.
type pendingValue struct {
	pathIndex int
	depth     int
	// container is true when the located value is itself an object or array, so
	// the End is the matching close brace/bracket; false for strings and scalars
	// whose End is detected by their own terminator.
	container bool
}

type scanner struct {
	paths   [][]Step
	results []LocatedSpan

	// off is the absolute byte offset of the NEXT byte to be fed (equivalently,
	// the count of bytes consumed so far). It is the line-relative offset used
	// for spans.
	off int64

	stack []frame

	// String scanning state. When inString is true the scanner is consuming the
	// contents of a JSON string; escNext skips the byte after a backslash; uLeft
	// counts remaining hex digits of a \uXXXX escape.
	inString bool
	escNext  bool
	uLeft    int
	// stringIsKey is true when the current string is an object member key (so its
	// completion sets the enclosing frame's curKey) rather than a value.
	stringIsKey bool
	// keyBuf accumulates the bytes of an object key as it is read. Keys are
	// bounded by JSON structure and small in practice; only one is held at a time
	// per active key, never the value bytes.
	keyBuf []byte

	// Scalar scanning state. When inScalar is true the scanner is consuming a
	// bare token (number, true, false, null) whose end is the first byte that is
	// not part of the token. scalarPending, when set, is the pending value to
	// close at the scalar's end.
	inScalar      bool
	scalarPending *pendingValue

	// pendings holds values currently open (entered but not yet closed) whose End
	// must still be recorded.
	pendings []pendingValue

	// expectKey is true when, inside an object, the next string encountered is a
	// member key rather than a value.
	expectKey bool
}

func newScanner(paths [][]Step) *scanner {
	return &scanner{paths: paths}
}

// atPath reports whether the structural position about to receive a value
// matches the given path. The position is described by the container stack plus
// the key or index that the value sits under. A value at stack depth d matches a
// path of length d+1 when every ancestor step agrees and the final step agrees
// with the immediate key/index.
func (s *scanner) atPath(path []Step) bool {
	// A value living directly inside the stack's containers sits at depth
	// len(stack); its path must have exactly that many steps.
	if len(path) != len(s.stack) {
		return false
	}
	for i, fr := range s.stack {
		switch step := path[i].(type) {
		case Key:
			if !fr.isObject || fr.curKey != string(step) {
				return false
			}
		case Idx:
			if fr.isObject || fr.arrIdx != int(step) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// beginValue is called at the first byte of any JSON value (object, array,
// string, number, literal). startOff is that byte's absolute offset. It records
// Start for any path matching the current structural position and registers a
// pending close so End can be captured later.
func (s *scanner) beginValue(startOff int64, container bool) {
	for pi, path := range s.paths {
		if s.alreadyLocated(pi) {
			continue
		}
		if s.atPath(path) {
			s.pendings = append(s.pendings, pendingValue{
				pathIndex: pi,
				depth:     len(s.stack),
				container: container,
			})
			// Stash Start immediately by writing a placeholder result; End is
			// filled when the value closes. Using a map-free approach: append now,
			// patch End later via pendingValue.pathIndex lookup.
			s.results = append(s.results, LocatedSpan{
				PathIndex: pi,
				Span:      ValueSpan{Start: startOff, End: -1},
			})
		}
	}
}

// alreadyLocated reports whether a path already has a recorded span, so the same
// path is not matched twice (the first occurrence wins, matching a single
// gjson lookup).
func (s *scanner) alreadyLocated(pi int) bool {
	for _, r := range s.results {
		if r.PathIndex == pi {
			return true
		}
	}
	return false
}

// closeScalarPending records the End of a pending scalar value at the current
// offset.
func (s *scanner) setEnd(pi int, end int64) {
	for i := range s.results {
		if s.results[i].PathIndex == pi && s.results[i].Span.End == -1 {
			s.results[i].Span.End = end
			return
		}
	}
}

// feed advances the scanner by a single byte b. The byte's absolute offset is
// the current s.off; afterward s.off is incremented.
func (s *scanner) feed(b byte) {
	off := s.off
	s.off = off + 1

	// String contents: consume until the closing quote, honoring escapes. Brace
	// and bracket bytes inside a string are literal and must not affect the
	// container stack, which is the whole point of tracking string state.
	if s.inString {
		// Accumulate bytes only for object keys, which are small and must be
		// decoded for path matching. Value strings (which can be hundreds of MiB)
		// are skipped without retaining any bytes, keeping memory O(depth).
		if s.stringIsKey {
			s.keyBuf = append(s.keyBuf, b)
		}
		switch {
		case s.escNext:
			s.escNext = false
			if b == 'u' {
				s.uLeft = 4
			}
		case s.uLeft > 0:
			s.uLeft--
		case b == '\\':
			s.escNext = true
		case b == '"':
			s.endString(off)
		}
		return
	}

	// Bare scalar (number / true / false / null): ends at the first byte that is
	// not part of the token. That terminator byte is NOT consumed here; it is
	// re-dispatched below as ordinary structure.
	if s.inScalar {
		if isScalarByte(b) {
			return
		}
		s.endScalar(off)
		// fall through to handle b as structure
	}

	switch b {
	case ' ', '\t', '\n', '\r':
		return
	case '"':
		s.startString(off)
	case '{':
		s.openContainer(off, true)
	case '[':
		s.openContainer(off, false)
	case '}', ']':
		s.closeContainer(off)
	case ':':
		// Separates a key from its value inside an object: the next value is a
		// member value, not a key.
		return
	case ',':
		// Separates members/elements: inside an object the next string is a key;
		// inside an array the element index advances when its value begins.
		if top := s.top(); top != nil && top.isObject {
			s.expectKey = true
		}
		return
	default:
		// Start of a bare scalar.
		s.startScalar(off, b)
	}
}

// top returns the innermost container frame, or nil at the document root.
func (s *scanner) top() *frame {
	if len(s.stack) == 0 {
		return nil
	}
	return &s.stack[len(s.stack)-1]
}

// preValue updates structural bookkeeping that must happen at the first byte of
// a value: advancing the array element index, and clearing the object
// expect-key flag. It returns after the stack frame reflects the position of the
// value about to begin, so atPath sees the correct key/index.
func (s *scanner) preValue() {
	top := s.top()
	if top == nil {
		return
	}
	if !top.isObject {
		top.arrIdx++
	}
}

func (s *scanner) startString(off int64) {
	top := s.top()
	if top != nil && top.isObject && s.expectKey {
		// This string is a member key, not a value.
		s.inString = true
		s.stringIsKey = true
		s.expectKey = false
		s.keyBuf = s.keyBuf[:0]
		s.keyBuf = append(s.keyBuf, '"')
		return
	}
	// A string value.
	s.preValue()
	s.beginValue(off, false)
	s.inString = true
	s.stringIsKey = false
	s.keyBuf = s.keyBuf[:0]
}

func (s *scanner) endString(off int64) {
	s.inString = false
	s.escNext = false
	s.uLeft = 0
	if s.stringIsKey {
		// keyBuf holds the raw quoted key including both quotes; decode it to the
		// member name and stash on the enclosing frame.
		if top := s.top(); top != nil {
			top.curKey = decodeKey(s.keyBuf)
		}
		s.stringIsKey = false
		s.keyBuf = s.keyBuf[:0]
		return
	}
	// End of a string value is the byte just past the closing quote.
	s.closePendingsAt(off+1, false)
	s.keyBuf = s.keyBuf[:0]
}

func (s *scanner) startScalar(off int64, b byte) {
	s.preValue()
	s.beginValue(off, false)
	s.inScalar = true
}

func (s *scanner) endScalar(off int64) {
	s.inScalar = false
	// End of a scalar is the offset of the first non-token byte, which is exactly
	// off (the byte currently being dispatched as structure).
	s.closePendingsAt(off, false)
}

func (s *scanner) openContainer(off int64, isObject bool) {
	s.preValue()
	s.beginValue(off, true)
	fr := frame{isObject: isObject, arrIdx: -1}
	s.stack = append(s.stack, fr)
	if isObject {
		s.expectKey = true
	}
}

func (s *scanner) closeContainer(off int64) {
	// Pop the frame; the closed container's values lived at depth len(stack)-1,
	// so any pending value entered at that depth as a container closes here, with
	// End just past this brace/bracket.
	if len(s.stack) == 0 {
		return
	}
	closedDepth := len(s.stack) - 1
	s.stack = s.stack[:len(s.stack)-1]
	s.closeContainerPendingsAt(off+1, closedDepth)
	s.expectKey = false
}

// closePendingsAt records End for non-container pending values (strings,
// scalars) that are open. Strings and scalars cannot nest, so the most recently
// opened non-container pending is the one ending here.
func (s *scanner) closePendingsAt(end int64, container bool) {
	for i := len(s.pendings) - 1; i >= 0; i-- {
		p := s.pendings[i]
		if p.container != container {
			continue
		}
		s.setEnd(p.pathIndex, end)
		s.pendings = append(s.pendings[:i], s.pendings[i+1:]...)
		// Only the innermost matching one closes per terminator.
		return
	}
}

// closeContainerPendingsAt records End for container pending values that were
// entered at closedDepth (their immediate container was the one just popped:
// their own depth equals closedDepth).
func (s *scanner) closeContainerPendingsAt(end int64, closedDepth int) {
	for i := len(s.pendings) - 1; i >= 0; i-- {
		p := s.pendings[i]
		if !p.container || p.depth != closedDepth {
			continue
		}
		s.setEnd(p.pathIndex, end)
		s.pendings = append(s.pendings[:i], s.pendings[i+1:]...)
	}
}

// finish flushes any value still open at EOF. A scalar at the very end of the
// line (no trailing structure byte to terminate it) closes at the final offset.
func (s *scanner) finish() {
	if s.inScalar {
		s.inScalar = false
		s.closePendingsAt(s.off, false)
	}
	// Drop any results that never received an End (malformed/truncated input):
	// they would otherwise carry the sentinel -1.
	filtered := s.results[:0]
	for _, r := range s.results {
		if r.Span.End != -1 {
			filtered = append(filtered, r)
		}
	}
	s.results = filtered
}

// isScalarByte reports whether b can be part of a bare JSON scalar token
// (number, true, false, null). The set is deliberately permissive: the input is
// assumed well-formed, so this only needs to distinguish token bytes from the
// structural bytes and whitespace that terminate a token.
func isScalarByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', ',', '}', ']', ':':
		return false
	default:
		return true
	}
}

// decodeKey turns a raw quoted JSON string (including both surrounding quotes)
// into its member-name value, resolving the escape sequences that can appear in
// object keys. Keys are compared against path steps, so they must be decoded to
// match what the caller wrote.
func decodeKey(raw []byte) string {
	if len(raw) < 2 {
		return ""
	}
	body := raw[1 : len(raw)-1]
	// Fast path: no escapes means the body is the name verbatim.
	hasEsc := false
	for _, c := range body {
		if c == '\\' {
			hasEsc = true
			break
		}
	}
	if !hasEsc {
		return string(body)
	}
	out := make([]byte, 0, len(body))
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		if i >= len(body) {
			break
		}
		switch body[i] {
		case '"':
			out = append(out, '"')
		case '\\':
			out = append(out, '\\')
		case '/':
			out = append(out, '/')
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r := decodeHex4(body[i+1:])
			i += 4
			out = appendRune(out, r)
		default:
			out = append(out, body[i])
		}
	}
	return string(out)
}

// decodeHex4 reads up to four hex digits and returns the code point. Malformed
// input (assumed not to occur for well-formed keys) yields the replacement
// character.
func decodeHex4(b []byte) rune {
	if len(b) < 4 {
		return '�'
	}
	var r rune
	for i := 0; i < 4; i++ {
		c := b[i]
		var v rune
		switch {
		case c >= '0' && c <= '9':
			v = rune(c - '0')
		case c >= 'a' && c <= 'f':
			v = rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = rune(c-'A') + 10
		default:
			return '�'
		}
		r = r<<4 | v
	}
	return r
}

// appendRune appends the UTF-8 encoding of r to out without importing
// unicode/utf8 for a single call site; it handles the basic-multilingual-plane
// range that JSON \u escapes for keys realistically use.
func appendRune(out []byte, r rune) []byte {
	switch {
	case r < 0x80:
		return append(out, byte(r))
	case r < 0x800:
		return append(out, byte(0xC0|r>>6), byte(0x80|r&0x3F))
	default:
		return append(out, byte(0xE0|r>>12), byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
	}
}
