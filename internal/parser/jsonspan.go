package parser

import (
	"context"
	"errors"
	"io"
)

// maxKeyLen caps how many bytes of an object key the span scanners buffer. The
// scanners only buffer keys to compare them against path steps, and a real path
// step (a field name like "content" or "arguments") is short. A raw tool body
// that is a JSON object with an enormous key name would otherwise grow a
// body-sized allocation just to locate a span, so once a key passes this cap it
// can no longer equal any path step and the rest of its bytes are dropped: the
// key is treated as a guaranteed non-match. The cap is generous relative to any
// real field name so normal matching is untouched.
const maxKeyLen = 4096

// ctxCheckBytes bounds how many bytes the streaming scanners feed between
// context cancellation checks. A single transcript line can be hundreds of MiB,
// so a per-chunk check alone is not enough when one next() chunk is itself huge;
// checking every ctxCheckBytes keeps cancellation responsive without paying an
// atomic load per byte.
const ctxCheckBytes = 64 << 10

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
//
// ctx lets a caller cancel a scan of a huge line promptly: it is checked once
// per chunk returned by next() and again every ctxCheckBytes within a chunk, so
// a canceled hash or upload aborts instead of streaming the whole value.
func LocateValues(ctx context.Context, paths [][]Step, next func() ([]byte, error)) ([]LocatedSpan, error) {
	sc := newScanner(paths)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk, err := next()
		for i, b := range chunk {
			if i%ctxCheckBytes == 0 && i > 0 {
				if cerr := ctx.Err(); cerr != nil {
					return nil, cerr
				}
			}
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
func LocateValue(ctx context.Context, path []Step, next func() ([]byte, error)) (ValueSpan, bool, error) {
	res, err := LocateValues(ctx, [][]Step{path}, next)
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
		// are skipped without retaining any bytes, keeping memory O(depth). A key
		// is buffered only up to maxKeyLen: past that it cannot equal any path
		// step, so the surplus bytes are dropped to bound the allocation while the
		// scan still tracks the string to its closing quote.
		if s.stringIsKey && len(s.keyBuf) < maxKeyLen {
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

// WalkArrayElements scans the JSONL line exactly once, streaming, and invokes
// visit for each direct element of the array located at arrPath, in source order.
// For every element it reports the element's own byte span plus, for an object
// element, the byte spans of any requested subKeys that are present as direct
// members. Elements that are not objects (a bare string, a number) simply carry
// an empty subSpans map.
//
// This is the single-pass primitive behind the block walkers: a transcript line's
// content array can hold many blocks, and probing each block index with its own
// LocateValues pass restreams the whole line per element (O(line * elements)).
// Walking the array once is O(line) total while keeping memory at O(path depth):
// element bodies (which can be hundreds of MiB) are never buffered, only the tiny
// element span and the small subKey spans are retained, and each is handed to
// visit as soon as the element closes so nothing accumulates across elements.
//
// next supplies the line incrementally exactly as LocateValues consumes it: each
// call returns the next chunk of bytes until it reports io.EOF, and the walker is
// correct for any chunking. visit is called with the element index (0-based), the
// element span, and a map from the matched subKey Step to its span. Returning a
// non-nil error from visit aborts the walk and is propagated. The reported spans
// are byte-identical to gjson (value .Index for Start, .Index+len(.Raw) for End).
//
// Only direct members of an element object are matched for subKeys: a subKey is a
// single Step (for example Key("type")), not a nested path, because block
// discriminators and bodies live one level under the element.
//
// ctx lets a caller abort a walk over a huge array promptly: it is checked once
// per chunk returned by next() and again every ctxCheckBytes within a chunk, so
// a canceled hash or upload of a large array result stops mid-enumeration rather
// than draining the whole region.
func WalkArrayElements(ctx context.Context, arrPath []Step, subKeys []Step, next func() ([]byte, error), visit func(idx int, elemSpan ValueSpan, subSpans map[Step]ValueSpan) error) error {
	w := newArrayWalker(arrPath, subKeys, visit)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := next()
		for i, b := range chunk {
			if i%ctxCheckBytes == 0 && i > 0 {
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
			}
			if ferr := w.feed(b); ferr != nil {
				return ferr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}
	return w.finish()
}

// arrayWalker is the single-pass scanner backing WalkArrayElements. It tracks the
// container stack to recognize when it enters the target array, then captures one
// element's span (and its requested subKey spans) at a time, flushing each to the
// visit callback the instant the element closes so element bodies are never
// retained.
type arrayWalker struct {
	arrPath []Step
	subKeys []Step
	visit   func(idx int, elemSpan ValueSpan, subSpans map[Step]ValueSpan) error

	off   int64
	stack []frame

	// inString / escNext / uLeft / stringIsKey / keyBuf mirror the LocateValues
	// scanner's string state: structural braces and brackets inside a string are
	// literal and must not move the container stack, and object keys are decoded
	// for subKey matching while value bytes are skipped.
	inString    bool
	escNext     bool
	uLeft       int
	stringIsKey bool
	keyBuf      []byte

	inScalar  bool
	expectKey bool

	// arrDepth is the stack depth of the target array's frame once entered, or -1
	// when the walker is not inside the target array. Elements are the values that
	// live directly in that array, at stack depth arrDepth+1.
	arrDepth int

	// elem is the span of the array element currently being scanned, valid while
	// inElem is true. elemSubs collects the matched subKey spans for that element.
	// elemContainer marks an element that is itself an object or array, whose End is
	// its matching close bracket rather than a scalar/string terminator.
	inElem        bool
	elemContainer bool
	elem          ValueSpan
	elemSubs      map[Step]ValueSpan
	elemIdx       int

	// sub tracks a subKey value currently open inside the element object so its End
	// can be recorded when it closes. subActive is the matched Step; subContainer
	// distinguishes a container value's close-bracket terminator.
	subActive    Step
	subOpen      bool
	subContainer bool
	subStart     int64
	subDepth     int
}

func newArrayWalker(arrPath, subKeys []Step, visit func(idx int, elemSpan ValueSpan, subSpans map[Step]ValueSpan) error) *arrayWalker {
	return &arrayWalker{
		arrPath:  arrPath,
		subKeys:  subKeys,
		visit:    visit,
		arrDepth: -1,
	}
}

// arrayMatchesPath reports whether the array value about to begin (its opening
// bracket is being processed) sits exactly at arrPath. It mirrors the LocateValues
// atPath convention: a value living directly inside the current containers sits at
// depth len(stack), so its path has exactly len(stack) steps, and each enclosing
// frame's current key/index must agree with the corresponding step. The innermost
// frame's curKey/arrIdx already describes the position of the value being scanned,
// so there is no separate parent-vs-final split.
func (w *arrayWalker) arrayMatchesPath() bool {
	if len(w.arrPath) != len(w.stack) {
		return false
	}
	for i, fr := range w.stack {
		switch step := w.arrPath[i].(type) {
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

func (w *arrayWalker) top() *frame {
	if len(w.stack) == 0 {
		return nil
	}
	return &w.stack[len(w.stack)-1]
}

// inTargetArray reports whether the value about to begin is a direct element of
// the target array (the innermost frame is that array).
func (w *arrayWalker) inTargetArray() bool {
	return w.arrDepth >= 0 && len(w.stack) == w.arrDepth+1
}

// inElementObject reports whether the value about to begin is a direct member of
// the current element object (one level below the array).
func (w *arrayWalker) inElementObject() bool {
	return w.inElem && w.arrDepth >= 0 && len(w.stack) == w.arrDepth+2 && w.top() != nil && w.top().isObject
}

func (w *arrayWalker) feed(b byte) error {
	off := w.off
	w.off = off + 1

	if w.inString {
		// Buffer an object key only up to maxKeyLen, mirroring the LocateValues
		// scanner: a key longer than the cap cannot equal any short subKey, so its
		// surplus bytes are dropped to keep a giant key from growing a body-sized
		// allocation while the walk still tracks the string to its closing quote.
		if w.stringIsKey && len(w.keyBuf) < maxKeyLen {
			w.keyBuf = append(w.keyBuf, b)
		}
		switch {
		case w.escNext:
			w.escNext = false
			if b == 'u' {
				w.uLeft = 4
			}
		case w.uLeft > 0:
			w.uLeft--
		case b == '\\':
			w.escNext = true
		case b == '"':
			return w.endString(off)
		}
		return nil
	}

	if w.inScalar {
		if isScalarByte(b) {
			return nil
		}
		w.inScalar = false
		if err := w.closeNonContainer(off); err != nil {
			return err
		}
		// fall through to handle b as structure
	}

	switch b {
	case ' ', '\t', '\n', '\r':
		return nil
	case '"':
		return w.startString(off)
	case '{':
		return w.openContainer(off, true)
	case '[':
		return w.openContainer(off, false)
	case '}', ']':
		return w.closeContainer(off)
	case ':':
		return nil
	case ',':
		if top := w.top(); top != nil && top.isObject {
			w.expectKey = true
		}
		return nil
	default:
		return w.startScalar(off)
	}
}

// preValue advances the array element index of the innermost array frame, matching
// the LocateValues scanner so atPath-style checks see the right index.
func (w *arrayWalker) preValue() {
	top := w.top()
	if top != nil && !top.isObject {
		top.arrIdx++
	}
}

// noteValueStart records the start of an element or subKey value when the position
// matches. container says the value is an object or array whose End is its close
// bracket.
func (w *arrayWalker) noteValueStart(startOff int64, container bool) {
	if w.inTargetArray() {
		w.inElem = true
		w.elemContainer = container
		w.elem = ValueSpan{Start: startOff, End: -1}
		w.elemSubs = nil
		w.elemIdx = w.top().arrIdx
		return
	}
	if w.inElementObject() && !w.subOpen {
		key := w.top().curKey
		for _, sk := range w.subKeys {
			if k, ok := sk.(Key); ok && string(k) == key {
				w.subActive = sk
				w.subOpen = true
				w.subContainer = container
				w.subStart = startOff
				w.subDepth = len(w.stack)
				break
			}
		}
	}
}

func (w *arrayWalker) startString(off int64) error {
	top := w.top()
	if top != nil && top.isObject && w.expectKey {
		w.inString = true
		w.stringIsKey = true
		w.expectKey = false
		w.keyBuf = w.keyBuf[:0]
		w.keyBuf = append(w.keyBuf, '"')
		return nil
	}
	w.preValue()
	w.noteValueStart(off, false)
	w.inString = true
	w.stringIsKey = false
	w.keyBuf = w.keyBuf[:0]
	return nil
}

func (w *arrayWalker) endString(off int64) error {
	w.inString = false
	w.escNext = false
	w.uLeft = 0
	if w.stringIsKey {
		if top := w.top(); top != nil {
			top.curKey = decodeKey(w.keyBuf)
		}
		w.stringIsKey = false
		w.keyBuf = w.keyBuf[:0]
		return nil
	}
	// A string value ends just past its closing quote.
	w.keyBuf = w.keyBuf[:0]
	return w.closeNonContainer(off + 1)
}

func (w *arrayWalker) startScalar(off int64) error {
	w.preValue()
	w.noteValueStart(off, false)
	w.inScalar = true
	return nil
}

func (w *arrayWalker) openContainer(off int64, isObject bool) error {
	w.preValue()
	w.noteValueStart(off, true)
	// Recognize entry into the target array before pushing its frame, while the
	// stack still holds the parent containers that arrayMatchesPath inspects.
	enterTarget := !isObject && w.arrDepth < 0 && w.arrayMatchesPath()
	w.stack = append(w.stack, frame{isObject: isObject, arrIdx: -1})
	if isObject {
		w.expectKey = true
	}
	if enterTarget {
		w.arrDepth = len(w.stack) - 1
	}
	return nil
}

func (w *arrayWalker) closeContainer(off int64) error {
	if len(w.stack) == 0 {
		return nil
	}
	closedDepth := len(w.stack) - 1
	w.stack = w.stack[:len(w.stack)-1]
	end := off + 1

	// An open subKey container closes when its own depth is the depth just popped.
	if w.subOpen && w.subContainer && w.subDepth == closedDepth {
		w.recordSub(end)
	}
	// An element container closes when the array frame is now the innermost frame
	// again (the element lived one level deeper than the array).
	if w.inElem && w.elemContainer && w.arrDepth >= 0 && len(w.stack) == w.arrDepth+1 {
		w.elem.End = end
		if err := w.flushElem(); err != nil {
			return err
		}
	}
	// Leaving the target array entirely: the popped frame was the array frame.
	if w.arrDepth >= 0 && closedDepth == w.arrDepth {
		w.arrDepth = -1
	}
	w.expectKey = false
	return nil
}

// closeNonContainer records the End of an open string/scalar element or subKey at
// end, flushing a completed element to visit.
func (w *arrayWalker) closeNonContainer(end int64) error {
	// A subKey string/scalar value closes first (it is deeper than the element).
	if w.subOpen && !w.subContainer && w.subDepth == len(w.stack) {
		w.recordSub(end)
		return nil
	}
	if w.inElem && !w.elemContainer {
		w.elem.End = end
		return w.flushElem()
	}
	return nil
}

// recordSub stores a matched subKey span on the current element.
func (w *arrayWalker) recordSub(end int64) {
	if w.elemSubs == nil {
		w.elemSubs = make(map[Step]ValueSpan, len(w.subKeys))
	}
	w.elemSubs[w.subActive] = ValueSpan{Start: w.subStart, End: end}
	w.subOpen = false
	w.subActive = nil
}

// flushElem hands the completed element to visit and clears element state so the
// next element starts fresh and nothing accumulates across elements.
func (w *arrayWalker) flushElem() error {
	subs := w.elemSubs
	if subs == nil {
		subs = map[Step]ValueSpan{}
	}
	idx := w.elemIdx
	elem := w.elem
	w.inElem = false
	w.elemSubs = nil
	w.elem = ValueSpan{}
	w.subOpen = false
	w.subActive = nil
	return w.visit(idx, elem, subs)
}

// finish flushes a scalar element still open at EOF (a bare scalar at the very end
// of the array region has no trailing structure byte to terminate it).
func (w *arrayWalker) finish() error {
	if w.inScalar {
		w.inScalar = false
		if err := w.closeNonContainer(w.off); err != nil {
			return err
		}
	}
	return nil
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
