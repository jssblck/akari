package parser

import (
	"fmt"
	"io"
)

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
func LocateToolBodies(agent Agent, f io.ReaderAt, lineOff, lineLen int64) ([]BodyLocation, error) {
	src := &lineSource{f: f, base: lineOff, size: lineLen}
	switch agent {
	case AgentClaude:
		return locateClaude(src)
	case AgentCodex:
		return locateCodex(src)
	case AgentPi:
		return locatePi(src)
	default:
		return nil, nil
	}
}

// lineSource streams a single line's bytes from a file span and reads small fixed
// spans within it. It exists so the enumerator can both run a streaming
// LocateValues pass (via reader) and pull a tiny value (a block `type`) by span
// without buffering the whole line.
type lineSource struct {
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
		if _, err := s.f.ReadAt(buf, s.base+pos); err != nil && err != io.EOF {
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
	if _, err := s.f.ReadAt(buf, s.base+sp.Start); err != nil && err != io.EOF {
		return "", err
	}
	return string(buf), nil
}

// locate runs one streaming LocateValues pass for the given paths and returns the
// spans keyed by their path index.
func (s *lineSource) locate(paths [][]Step) (map[int]ValueSpan, error) {
	spans, err := LocateValues(paths, s.reader())
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

// blockBatch is how many array indices the enumerator probes per LocateValues
// pass. A transcript line has only a handful of content blocks, so one batch
// almost always covers them; a longer line just runs another pass.
const blockBatch = 64

// locateClaude finds claude tool inputs (on an assistant line) and tool results
// (on a user line) by probing content-block indices in batches.
func locateClaude(s *lineSource) ([]BodyLocation, error) {
	typ, err := s.topType(Key("type"))
	if err != nil {
		return nil, err
	}
	switch typ {
	case "assistant":
		return s.locateBlocks(
			[]Step{Key("message"), Key("content")},
			"tool_use", Key("input"), BodyRaw, "application/json", false)
	case "user":
		return s.locateResultBlocks(
			[]Step{Key("message"), Key("content")},
			"tool_result", Key("content"))
	}
	return nil, nil
}

// locatePi finds pi tool inputs (assistant) and tool results (toolResult message).
func locatePi(s *lineSource) ([]BodyLocation, error) {
	typ, err := s.topType(Key("type"))
	if err != nil {
		return nil, err
	}
	if typ != "message" {
		return nil, nil
	}
	role, err := s.unquotedAt([]Step{Key("message"), Key("role")})
	if err != nil {
		return nil, err
	}
	switch role {
	case "assistant":
		return s.locateBlocks(
			[]Step{Key("message"), Key("content")},
			"toolCall", Key("arguments"), BodyRaw, "application/json", false)
	case "toolResult":
		return s.locateSingleResult([]Step{Key("message"), Key("content")})
	}
	return nil, nil
}

// locateCodex finds the codex function_call argument body and function_call_output
// result body. The argument body is a JSON-encoded string whose decoded contents
// are what the CAS stores, so its kind is BodyJSONString.
func locateCodex(s *lineSource) ([]BodyLocation, error) {
	typ, err := s.topType(Key("type"))
	if err != nil {
		return nil, err
	}
	if typ != "response_item" {
		return nil, nil
	}
	ptype, err := s.unquotedAt([]Step{Key("payload"), Key("type")})
	if err != nil {
		return nil, err
	}
	switch ptype {
	case "function_call":
		return s.locateSingle([]Step{Key("payload"), Key("arguments")}, BodyJSONString, "application/json")
	case "function_call_output":
		return s.locateSingleResult([]Step{Key("payload"), Key("output")})
	}
	return nil, nil
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

// locateSingle returns the body at a single fixed path with a known kind/media.
func (s *lineSource) locateSingle(path []Step, kind BodyKind, media string) ([]BodyLocation, error) {
	spans, err := s.locate([][]Step{path})
	if err != nil {
		return nil, err
	}
	sp, ok := spans[0]
	if !ok || sp.End <= sp.Start {
		return nil, nil
	}
	return []BodyLocation{{Span: sp, Kind: kind, Media: media}}, nil
}

// locateSingleResult returns a single result body at a fixed path, classifying its
// kind and media from its first byte (string, array, or object).
func (s *lineSource) locateSingleResult(path []Step) ([]BodyLocation, error) {
	spans, err := s.locate([][]Step{path})
	if err != nil {
		return nil, err
	}
	sp, ok := spans[0]
	if !ok || sp.End <= sp.Start {
		return nil, nil
	}
	loc, ok, err := s.classifyResult(sp)
	if err != nil || !ok {
		return nil, err
	}
	return []BodyLocation{loc}, nil
}

// locateBlocks walks an array of content blocks, returning the body at bodyKey for
// each block whose `type` matches wantType. inputs use a fixed kind/media.
func (s *lineSource) locateBlocks(arr []Step, wantType string, bodyKey Step, kind BodyKind, media string, _ bool) ([]BodyLocation, error) {
	var out []BodyLocation
	for start := 0; ; start += blockBatch {
		paths, meta := blockPaths(arr, start, bodyKey)
		spans, err := s.locate(paths)
		if err != nil {
			return nil, err
		}
		seen := false
		for i := 0; i < blockBatch; i++ {
			typeSpan, hasType := spans[meta.typeIdx(i)]
			if !hasType {
				continue // no such block
			}
			seen = true
			bt, err := s.unquoted(typeSpan)
			if err != nil {
				return nil, err
			}
			if bt != wantType {
				continue
			}
			if bodySpan, ok := spans[meta.bodyIdx(i)]; ok && bodySpan.End > bodySpan.Start {
				out = append(out, BodyLocation{Span: bodySpan, Kind: kind, Media: media})
			}
		}
		if !seen {
			break // the batch found no block: the array is exhausted
		}
	}
	return out, nil
}

// locateResultBlocks walks claude tool_result blocks, classifying each result body
// from its first byte.
func (s *lineSource) locateResultBlocks(arr []Step, wantType string, bodyKey Step) ([]BodyLocation, error) {
	var out []BodyLocation
	for start := 0; ; start += blockBatch {
		paths, meta := blockPaths(arr, start, bodyKey)
		spans, err := s.locate(paths)
		if err != nil {
			return nil, err
		}
		seen := false
		for i := 0; i < blockBatch; i++ {
			typeSpan, hasType := spans[meta.typeIdx(i)]
			if !hasType {
				continue
			}
			seen = true
			bt, err := s.unquoted(typeSpan)
			if err != nil {
				return nil, err
			}
			if bt != wantType {
				continue
			}
			bodySpan, ok := spans[meta.bodyIdx(i)]
			if !ok || bodySpan.End <= bodySpan.Start {
				continue
			}
			loc, ok, err := s.classifyResult(bodySpan)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, loc)
			}
		}
		if !seen {
			break
		}
	}
	return out, nil
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
	if _, err := s.f.ReadAt(b[:], s.base+sp.Start); err != nil && err != io.EOF {
		return 0, err
	}
	return b[0], nil
}

// batchMeta maps a block index within a batch to its path indices in the flat
// paths slice passed to LocateValues (two paths per block: its type and its body).
type batchMeta struct{ start int }

func (m batchMeta) typeIdx(i int) int { return i * 2 }
func (m batchMeta) bodyIdx(i int) int { return i*2 + 1 }

// blockPaths builds the type+body path pair for blockBatch consecutive array
// indices starting at start, plus the meta to read results back.
func blockPaths(arr []Step, start int, bodyKey Step) ([][]Step, batchMeta) {
	paths := make([][]Step, 0, blockBatch*2)
	for i := 0; i < blockBatch; i++ {
		idx := Idx(start + i)
		typePath := append(append([]Step{}, arr...), idx, Key("type"))
		bodyPath := append(append([]Step{}, arr...), idx, bodyKey)
		paths = append(paths, typePath, bodyPath)
	}
	return paths, batchMeta{start: start}
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
