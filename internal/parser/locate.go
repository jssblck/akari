package parser

import (
	"context"
	"fmt"
	"io"
)

// readFull reads exactly len(buf) bytes at off from f, treating any short read as
// a hard error rather than silently zero-filling. A transcript line declares the
// byte ranges of its values; if the file holds fewer bytes than a span claims the
// file is truncated, and reporting that as io.ErrUnexpectedEOF (with the range)
// keeps a corrupted store from being mistaken for valid empty data. It mirrors the
// full-read-or-error discipline of internal/client/upload.readAt, adapted to the
// io.ReaderAt the parser is handed rather than an *os.File.
func readFull(f io.ReaderAt, buf []byte, off int64) error {
	n, err := f.ReadAt(buf, off)
	if n == len(buf) {
		return nil
	}
	if err == nil || err == io.EOF {
		return fmt.Errorf("short read at [%d,%d): got %d of %d bytes: %w", off, off+int64(len(buf)), n, len(buf), io.ErrUnexpectedEOF)
	}
	// A non-EOF read failure keeps the same [off,off+len) context so the caller can
	// tell which span could not be read, not just that a read failed somewhere.
	return fmt.Errorf("read at [%d,%d): %w", off, off+int64(len(buf)), err)
}

// BodyLocation is one tool body found in a transcript line by streaming, ready to
// be lifted to the CAS without ever buffering the body. Span is the raw byte range
// of the value within the line (relative to the line's first byte), the bytes the
// sentinel replaces. Kind and Media say how to canonicalize the raw value into the
// bytes the CAS stores (CanonicalBodyReader), so the streamed body is byte
// identical to what the server records inline today.
type BodyLocation struct {
	Span  ValueSpan
	Kind  BodyKind
	Media string
}

// LocateToolBodies enumerates the tool input and result bodies in one transcript
// line, in source order, by streaming the line from the file rather than parsing
// it whole. It is the streaming twin of toolBodyFields: the same agent knowledge
// (which paths are bodies, which media each gets), but expressed as byte spans and
// a canonicalization kind so a hundreds-of-MiB body is never resident.
//
// The line lives at [lineOff, lineOff+lineLen) in f. Enumeration reads only the
// small structural parts (block `type` discriminators), never a body. A line whose
// shape is unknown or carries no tool body yields nothing.
//
// Results stream through emit, called once per located body in source order,
// rather than being collected into a slice. This lets the client lift one body at
// a time (upload it, rewrite its span) without the parser holding a slice whose
// size grows with the block count, so peak memory stays bounded by the structural
// scan, not by how many bodies a line carries. If emit returns an error the walk
// aborts and that error is returned. ctx threads through the structural scans so a
// canceled lift stops promptly even mid-line.
func LocateToolBodies(ctx context.Context, agent Agent, f io.ReaderAt, lineOff, lineLen int64, emit func(BodyLocation) error) error {
	src := &lineSource{ctx: ctx, f: f, base: lineOff, size: lineLen}
	switch agent {
	case AgentClaude:
		return locateClaude(src, emit)
	case AgentCodex:
		return locateCodex(src, emit)
	case AgentPi:
		return locatePi(src, emit)
	default:
		return nil
	}
}

// lineSource streams a single line's bytes from a file span and reads small fixed
// spans within it. It exists so the enumerator can both run a streaming
// LocateValues pass (via reader) and pull a tiny value (a block `type`) by span
// without buffering the whole line. ctx carries the caller's cancellation into
// every streaming scan the source drives.
type lineSource struct {
	ctx  context.Context
	f    io.ReaderAt
	base int64 // file offset of the line's first byte
	size int64 // line length in bytes
}

// scanChunk bounds how much of the line the enumerator pulls per read while
// streaming it through LocateValues. It is small: enumeration only walks structure
// and never materializes a body.
const scanChunk = 64 << 10

// reader returns a next() that streams the whole line in bounded windows, for a
// LocateValues pass.
func (s *lineSource) reader() func() ([]byte, error) {
	pos := int64(0)
	return func() ([]byte, error) {
		if pos >= s.size {
			return nil, io.EOF
		}
		n := s.size - pos
		if n > scanChunk {
			n = scanChunk
		}
		buf := make([]byte, n)
		// The window lies wholly within the declared line, so a short read here means
		// the file is shorter than the line claims: a truncation, not a clean EOF.
		if err := readFull(s.f, buf, s.base+pos); err != nil {
			return nil, err
		}
		pos += n
		var perr error
		if pos >= s.size {
			perr = io.EOF
		}
		return buf, perr
	}
}

// readSpan pulls a small value's bytes (a block `type` discriminator) by its span.
// It refuses spans larger than a tiny cap so a malformed line can never trick the
// enumerator into buffering a body here; a body's own bytes are only ever streamed
// through CanonicalBodyReader.
func (s *lineSource) readSpan(sp ValueSpan) (string, error) {
	const cap = 4 << 10
	n := sp.End - sp.Start
	if n <= 0 || n > cap {
		return "", nil
	}
	buf := make([]byte, n)
	// The span sits within the line, so fewer bytes than the span length means a
	// truncated file rather than a legitimately short value.
	if err := readFull(s.f, buf, s.base+sp.Start); err != nil {
		return "", err
	}
	return string(buf), nil
}

// locate runs one streaming LocateValues pass for the given paths and returns the
// spans keyed by their path index.
func (s *lineSource) locate(paths [][]Step) (map[int]ValueSpan, error) {
	spans, err := LocateValues(s.ctx, paths, s.reader())
	if err != nil {
		return nil, fmt.Errorf("locate tool body spans: %w", err)
	}
	out := make(map[int]ValueSpan, len(spans))
	for _, ls := range spans {
		out[ls.PathIndex] = ls.Span
	}
	return out, nil
}

// unquoted returns the decoded contents of a small JSON string value at span, used
// to read a block `type`. The value is tiny, so a one-shot decode is fine.
func (s *lineSource) unquoted(sp ValueSpan) (string, error) {
	raw, err := s.readSpan(sp)
	if err != nil {
		return "", err
	}
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return jsonUnquote(raw), nil
	}
	return raw, nil
}

// locateClaude finds claude tool inputs (on an assistant line) and tool results
// (on a user line) by walking the content array once, emitting each body as it is
// found.
func locateClaude(s *lineSource, emit func(BodyLocation) error) error {
	typ, err := s.topType(Key("type"))
	if err != nil {
		return err
	}
	switch typ {
	case "assistant":
		return s.locateBlocks(
			[]Step{Key("message"), Key("content")},
			"tool_use", Key("input"), BodyRaw, "application/json", emit)
	case "user":
		return s.locateResultBlocks(
			[]Step{Key("message"), Key("content")},
			"tool_result", Key("content"), emit)
	}
	return nil
}

// locatePi finds pi tool inputs (assistant) and tool results (toolResult message).
func locatePi(s *lineSource, emit func(BodyLocation) error) error {
	typ, err := s.topType(Key("type"))
	if err != nil {
		return err
	}
	if typ != "message" {
		return nil
	}
	role, err := s.unquotedAt([]Step{Key("message"), Key("role")})
	if err != nil {
		return err
	}
	switch role {
	case "assistant":
		return s.locateBlocks(
			[]Step{Key("message"), Key("content")},
			"toolCall", Key("arguments"), BodyRaw, "application/json", emit)
	case "toolResult":
		return s.locateSingleResult([]Step{Key("message"), Key("content")}, emit)
	}
	return nil
}

// locateCodex finds every liftable Codex body in source order: tool inputs
// (function_call arguments, custom_tool_call input), tool results
// (function_call_output and custom_tool_call_output), and the base64 images Codex
// inlines (image_generation results, data-URI image_url blocks in a user turn, and the
// images array of a user_message event). It is the streaming twin of codexBodyFields,
// so the two agree on which bytes are bodies and how each canonicalizes.
func locateCodex(s *lineSource, emit func(BodyLocation) error) error {
	typ, err := s.topType(Key("type"))
	if err != nil {
		return err
	}
	ptype, err := s.unquotedAt([]Step{Key("payload"), Key("type")})
	if err != nil {
		return err
	}
	payload := func(k string) []Step { return []Step{Key("payload"), Key(k)} }
	switch typ {
	case "response_item":
		// The discriminators mirror reduceCodex (and codexBodyFields): a tool item is
		// keyed by payload.type, a user turn by payload.role, since a Codex message has
		// no payload.type. Reading role only when no tool type matched keeps the common
		// path to one extra structural lookup.
		switch {
		case ptype == "function_call":
			return s.locateSingle(payload("arguments"), BodyJSONString, "application/json", emit)
		case ptype == "custom_tool_call":
			return s.locateSingle(payload("input"), BodyJSONString, "text/plain", emit)
		case ptype == "function_call_output", ptype == "custom_tool_call_output":
			return s.locateSingleResult(payload("output"), emit)
		case ptype == "image_generation_call":
			return s.locateImage(payload("result"), emit)
		default:
			role, err := s.unquotedAt([]Step{Key("payload"), Key("role")})
			if err != nil {
				return err
			}
			if role == "user" {
				return s.locateImageBlocks(payload("content"), emit)
			}
		}
	case "event_msg":
		switch ptype {
		case "image_generation_end":
			return s.locateImage(payload("result"), emit)
		case "user_message":
			return s.locateImageArray(payload("images"), emit)
		}
	}
	return nil
}

// locateImage emits the base64 image at a single fixed path as a BodyBase64 body,
// classifying its media from the value's head. A value that is absent, empty, or not a
// recognizable base64 image yields nothing (it stays inline).
func (s *lineSource) locateImage(path []Step, emit func(BodyLocation) error) error {
	spans, err := s.locate([][]Step{path})
	if err != nil {
		return err
	}
	sp, ok := spans[0]
	if !ok {
		return nil
	}
	return s.emitImage(sp, emit)
}

// locateImageBlocks walks a content array once, emitting each block's base64 image_url
// as a BodyBase64 body. It keys off the presence of a base64 image_url (not the block
// type) so it covers any image block kind, matching codexImageBlocks.
func (s *lineSource) locateImageBlocks(arr []Step, emit func(BodyLocation) error) error {
	return WalkArrayElements(s.ctx, arr, []Step{Key("image_url")}, s.reader(),
		func(_ int, _ ValueSpan, subs map[Step]ValueSpan) error {
			sp, ok := subs[Key("image_url")]
			if !ok {
				return nil
			}
			return s.emitImage(sp, emit)
		})
}

// locateImageArray walks a flat array of image strings once (the images field of a
// user_message event), emitting each base64 element as a BodyBase64 body.
func (s *lineSource) locateImageArray(arr []Step, emit func(BodyLocation) error) error {
	return WalkArrayElements(s.ctx, arr, nil, s.reader(),
		func(_ int, elem ValueSpan, _ map[Step]ValueSpan) error {
			return s.emitImage(elem, emit)
		})
}

// emitImage classifies a string value's head and emits it as a BodyBase64 body when it
// is a recognizable base64 image, choosing its media type the same way the buffered
// imageField does. A non-image or sub-quote-length span is skipped, so a non-image
// element of a walked array is passed over rather than lifted.
func (s *lineSource) emitImage(sp ValueSpan, emit func(BodyLocation) error) error {
	if sp.End-sp.Start < 2 {
		return nil // too short to be a quoted string value
	}
	head, err := s.imageHead(sp)
	if err != nil {
		return err
	}
	if !looksLikeBase64Image(head) {
		return nil
	}
	return emit(BodyLocation{Span: sp, Kind: BodyBase64, Media: imageMediaType(head)})
}

// imageHead reads the leading content bytes of a string value (inside the quotes) to
// classify it as an image and pick its media type. Base64/data-URI content carries no
// JSON escapes, so the raw bytes are the literal content, matching the buffered
// imageHead over the decoded string. The read is bounded, so a huge image is never
// buffered just to classify it.
func (s *lineSource) imageHead(sp ValueSpan) (string, error) {
	start := sp.Start + 1 // skip the opening quote
	end := sp.End - 1     // stop before the closing quote
	if end <= start {
		return "", nil
	}
	n := end - start
	if n > int64(imageHeadLen) {
		n = int64(imageHeadLen)
	}
	buf := make([]byte, n)
	if err := readFull(s.f, buf, s.base+start); err != nil {
		return "", err
	}
	return string(buf), nil
}

// topType reads a top-level discriminator string (the line `type`).
func (s *lineSource) topType(key Step) (string, error) {
	return s.unquotedAt([]Step{key})
}

// unquotedAt locates a small string value at a path and returns its decoded
// contents, or "" when absent.
func (s *lineSource) unquotedAt(path []Step) (string, error) {
	spans, err := s.locate([][]Step{path})
	if err != nil {
		return "", err
	}
	sp, ok := spans[0]
	if !ok {
		return "", nil
	}
	return s.unquoted(sp)
}

// locateSingle emits the body at a single fixed path with a known kind/media. The
// single-body cases call emit at most once.
func (s *lineSource) locateSingle(path []Step, kind BodyKind, media string, emit func(BodyLocation) error) error {
	spans, err := s.locate([][]Step{path})
	if err != nil {
		return err
	}
	sp, ok := spans[0]
	if !ok || sp.End <= sp.Start {
		return nil
	}
	return emit(BodyLocation{Span: sp, Kind: kind, Media: media})
}

// locateSingleResult emits a single result body at a fixed path, classifying its
// kind and media from its first byte (string, array, or object).
func (s *lineSource) locateSingleResult(path []Step, emit func(BodyLocation) error) error {
	spans, err := s.locate([][]Step{path})
	if err != nil {
		return err
	}
	sp, ok := spans[0]
	if !ok || sp.End <= sp.Start {
		return nil
	}
	loc, ok, err := s.classifyResult(sp)
	if err != nil || !ok {
		return err
	}
	return emit(loc)
}

// locateBlocks walks an array of content blocks in a single streaming pass,
// emitting the body at bodyKey for each block whose `type` matches wantType.
// Inputs use a fixed kind/media. Walking the array once (rather than re-streaming
// the whole line per batch of indices) keeps enumeration O(line); the walker hands
// back only the tiny type and body spans per element, never the body bytes. Each
// matching body is streamed to emit the instant its block is visited, so peak
// memory does not scale with the block count.
func (s *lineSource) locateBlocks(arr []Step, wantType string, bodyKey Step, kind BodyKind, media string, emit func(BodyLocation) error) error {
	return s.walkBlocks(arr, bodyKey, func(typeSpan, bodySpan ValueSpan, hasBody bool) error {
		bt, err := s.unquoted(typeSpan)
		if err != nil {
			return err
		}
		if bt != wantType {
			return nil
		}
		if hasBody && bodySpan.End > bodySpan.Start {
			return emit(BodyLocation{Span: bodySpan, Kind: kind, Media: media})
		}
		return nil
	})
}

// locateResultBlocks walks claude tool_result blocks in a single streaming pass,
// classifying each result body from its first byte and emitting it as it is found.
// Like locateBlocks it relies on WalkArrayElements so the line is streamed once
// regardless of block count and no per-block slice accumulates.
func (s *lineSource) locateResultBlocks(arr []Step, wantType string, bodyKey Step, emit func(BodyLocation) error) error {
	return s.walkBlocks(arr, bodyKey, func(typeSpan, bodySpan ValueSpan, hasBody bool) error {
		bt, err := s.unquoted(typeSpan)
		if err != nil {
			return err
		}
		if bt != wantType {
			return nil
		}
		if !hasBody || bodySpan.End <= bodySpan.Start {
			return nil
		}
		loc, ok, err := s.classifyResult(bodySpan)
		if err != nil {
			return err
		}
		if ok {
			return emit(loc)
		}
		return nil
	})
}

// walkBlocks runs one WalkArrayElements pass over the content array, invoking
// onBlock for each element that carries a `type` discriminator. It is the shared
// single-pass spine of locateBlocks and locateResultBlocks: both need each block's
// type span (to decide whether it is the wanted kind) and its body span (the value
// at bodyKey), and both must preserve source order, which the walker guarantees.
// An element without a `type` (a bare string element of a result array) is skipped
// here because both callers key off the discriminator.
func (s *lineSource) walkBlocks(arr []Step, bodyKey Step, onBlock func(typeSpan, bodySpan ValueSpan, hasBody bool) error) error {
	subKeys := []Step{Key("type"), bodyKey}
	return WalkArrayElements(s.ctx, arr, subKeys, s.reader(), func(_ int, _ ValueSpan, subs map[Step]ValueSpan) error {
		typeSpan, hasType := subs[Key("type")]
		if !hasType {
			return nil
		}
		bodySpan, hasBody := subs[bodyKey]
		return onBlock(typeSpan, bodySpan, hasBody)
	})
}

// classifyResult reads a result value's first byte to choose its canonicalization
// kind and media type, matching bodyContent's switch.
func (s *lineSource) classifyResult(sp ValueSpan) (BodyLocation, bool, error) {
	head, err := s.readHead(sp)
	if err != nil || head == 0 {
		return BodyLocation{}, false, err
	}
	kind, media := ClassifyResultBody(head)
	return BodyLocation{Span: sp, Kind: kind, Media: media}, true, nil
}

// readHead returns the first byte of a value span, the discriminator ClassifyResultBody needs.
func (s *lineSource) readHead(sp ValueSpan) (byte, error) {
	if sp.End <= sp.Start {
		return 0, nil
	}
	var b [1]byte
	// The span is non-empty (checked above), so the first byte must exist; a short
	// read here is a truncated file, not an absent value.
	if err := readFull(s.f, b[:], s.base+sp.Start); err != nil {
		return 0, err
	}
	return b[0], nil
}

// jsonUnquote decodes a small JSON string literal (a block `type`), resolving the
// simple escapes a discriminator could contain. Bodies never go through here; they
// stream through CanonicalBodyReader.
func jsonUnquote(raw string) string {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return raw
	}
	inner := raw[1 : len(raw)-1]
	if !hasByte(inner, '\\') {
		return inner
	}
	out := make([]byte, 0, len(inner))
	for i := 0; i < len(inner); i++ {
		if inner[i] != '\\' || i+1 >= len(inner) {
			out = append(out, inner[i])
			continue
		}
		i++
		switch inner[i] {
		case 'n':
			out = append(out, '\n')
		case 't':
			out = append(out, '\t')
		case 'r':
			out = append(out, '\r')
		case '"', '\\', '/':
			out = append(out, inner[i])
		default:
			out = append(out, '\\', inner[i])
		}
	}
	return string(out)
}

func hasByte(s string, b byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return true
		}
	}
	return false
}
