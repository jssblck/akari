package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/tidwall/gjson"
)

// HashString returns the lowercase hex sha256 of content. It hashes in place (the
// digest consumes the string in fixed blocks) so a large body is never copied into a
// byte slice just to be hashed. It hashes raw bytes; the CAS key is the hash of the
// STORED bytes, which differ from the raw body when the encoder compresses it, so
// this is the key only for an uncompressed (raw-stored) body. The server's inline
// fallback write path computes the identical hash over the identical raw bytes.
func HashString(content string) string {
	h := sha256.New()
	_, _ = io.WriteString(h, content)
	return hex.EncodeToString(h.Sum(nil))
}

// sentinelKey marks a JSON object that stands in for a tool body that has been
// lifted out of the transcript and into the content-addressed store. The client
// extracts every tool input and result body, uploads it to the CAS, and rewrites
// the transcript line so the body is replaced by this compact reference. The
// server reducer, on seeing the sentinel, records the reference (sha256, byte
// length, media type) without re-storing the body.
//
// The key is deliberately namespaced so it cannot collide with a real tool body:
// no agent emits a tool input or result whose top-level shape is a JSON object
// carrying this field. A body that happened to look like one would still round
// trip, because the sentinel also carries the exact byte length, but the prefix
// keeps the space ours by construction.
const sentinelKey = "__akari_cas__"

// Storage content types name how the bytes the CAS holds are encoded, which is a
// separate axis from a body's own MediaType (its semantic type, e.g.
// application/json). A small body is stored verbatim; a body large enough to be
// worth it is stored zstd-compressed. Compression is deliberately a client concern:
// the server stores and serves these bytes opaquely and never (de)compresses, and
// the browser decompresses transparently via Content-Encoding. These constants live
// here, in the dependency-free parser, so the server can name them without linking
// any compression code.
const (
	ContentRaw  = "application/octet-stream" // stored verbatim, key over the raw bytes
	ContentZstd = "application/zstd"         // stored zstd-compressed, key over the compressed bytes
)

// BodyEncoder turns a tool body's canonical raw bytes into the bytes the CAS
// actually stores, deciding per body whether compression is worth it. The CAS key
// is the sha256 of the STORED bytes, so the same raw body must always encode to the
// same stored bytes and key; the client supplies a deterministic implementation.
// The parser depends only on this interface, never on a compression library, which
// is what keeps the server (which links the parser but never compresses) free of
// one. The streaming big-line path lives in the client and drives the same encoder
// directly, so a body encodes identically whether buffered or streamed.
type BodyEncoder interface {
	// EncodeBody returns the CAS key (sha256 of the stored bytes), the stored bytes,
	// and their storage content type (ContentRaw or ContentZstd) for a body's raw
	// canonical bytes.
	EncodeBody(raw []byte) (sha string, stored []byte, contentType string)
}

// Body kinds label what a lifted body is, for diagnostics and the test oracle. The
// server records a body by its position in the transcript, not by this label, so the
// label never drives storage; it only distinguishes a tool input/result from a binary
// attachment (a lifted image) when a test compares the client's lifted set against the
// server's recorded set.
const (
	bodyKindInput      = "input"
	bodyKindResult     = "result"
	bodyKindAttachment = "attachment"
)

// Body is one tool body the client lifts out of the transcript. Stored holds the
// exact bytes the CAS keeps, which are the raw canonical bytes for a small body and
// the zstd-compressed form for a large one; ContentType says which. SHA256 is the
// key, the sha256 of Stored. Bytes is the RAW (uncompressed) canonical length, the
// size the transcript sentinel and the tool_calls row record (what a reader thinks
// of as the body's size), independent of how the bytes are stored. Kind is "input"
// or "result", for diagnostics and tests.
type Body struct {
	SHA256      string
	Bytes       int
	MediaType   string
	Stored      []byte
	ContentType string
	Kind        string
}

// casRef is the parsed sentinel: the reference the server records in place of a
// body it no longer has to store.
type casRef struct {
	SHA256    string
	Bytes     int
	MediaType string
}

// SentinelBytes renders the CAS reference that replaces a tool body, for callers
// outside the package (the client's streaming big-line path) that build a rewritten
// line from located spans rather than from a parsed line. It is the same encoding
// RewriteLine uses, so a body lifted by streaming produces a byte-identical sentinel
// to one lifted by the buffered path.
func SentinelBytes(sha string, n int, media string) []byte {
	return sentinelBytes(sha, n, media)
}

// sentinelBytes renders the compact reference that replaces a body in the
// transcript. It is a single-line JSON object so the rewritten transcript stays
// valid JSONL and a Codex line keeps its turn-boundary shape: the client's chunk
// boundary detection and the server's line parser both see the same line count
// and the same newline positions as the original.
func sentinelBytes(sha string, n int, media string) []byte {
	// Hand-build the object so the field order and escaping are fixed and
	// independent of map iteration: the rewritten transcript must be byte stable
	// across runs so a re-sync of an unchanged file produces identical bytes and
	// uploads nothing.
	b, _ := json.Marshal(media)
	return []byte(fmt.Sprintf(`{"%s":1,"sha256":%q,"bytes":%d,"media_type":%s}`,
		sentinelKey, sha, n, string(b)))
}

// asCASRef reports whether a parsed tool body is a CAS sentinel, returning the
// reference it carries. Both the reducer (to record the reference) and tests
// build on this, so the sentinel has exactly one reader.
func asCASRef(v gjson.Result) (casRef, bool) {
	if v.Type != gjson.JSON || !v.IsObject() {
		return casRef{}, false
	}
	marker := v.Get(sentinelKey)
	if !marker.Exists() {
		return casRef{}, false
	}
	return casRef{
		SHA256:    v.Get("sha256").String(),
		Bytes:     int(v.Get("bytes").Int()),
		MediaType: v.Get("media_type").String(),
	}, true
}

// bodyField is one tool body located within a single transcript line: the exact
// source byte span occupied by the body's raw JSON (so a rewrite can swap the
// span for a sentinel without disturbing the rest of the line) and the canonical
// content/media the CAS stores for it. content is exactly what the server reducer
// would write to the CAS today, so its sha and length are byte-faithful.
type bodyField struct {
	start   int // byte offset of the body's raw span within the line
	end     int // byte offset just past the raw span
	content string
	media   string
	kind    string // "input" | "result"
}

// toolBodyFields enumerates the tool input and result bodies in one parsed line,
// in left-to-right source order. It is the single definition of "which bytes are
// tool bodies" that both the client extractor and the round-trip tests rely on;
// the canonical content it returns mirrors exactly what the reducer feeds the CAS
// (b.input.Raw for an input, bodyContent for a result), so the extracted set can
// never drift from what the server stores inline today. A field already rewritten
// to a sentinel is skipped: re-extraction of an already-transformed line is a
// no-op, which keeps a re-sync idempotent.
//
// The span comes from gjson's value Index plus len(Raw); gjson reports the value
// offset reliably for a freshly parsed line (no modifiers, no path escaping), and
// the extractor verifies the span equals Raw before trusting it.
func toolBodyFields(agent Agent, line []byte) []bodyField {
	e := gjson.ParseBytes(line)
	switch agent {
	case AgentClaude:
		return claudeBodyFields(e)
	case AgentCodex:
		return codexBodyFields(e)
	case AgentPi:
		return piBodyFields(e)
	default:
		return nil
	}
}

func claudeBodyFields(e gjson.Result) []bodyField {
	var fields []bodyField
	switch e.Get("type").String() {
	case "assistant":
		for _, b := range e.Get("message.content").Array() {
			if b.Get("type").String() != "tool_use" {
				continue
			}
			input := b.Get("input")
			if f, ok := rawField(input, input.Raw, "application/json", "input"); ok {
				fields = append(fields, f)
			}
		}
	case "user":
		content := e.Get("message.content")
		if !content.IsArray() {
			return nil
		}
		for _, b := range content.Array() {
			if b.Get("type").String() != "tool_result" {
				continue
			}
			body := b.Get("content")
			c, media := bodyContent(body)
			if f, ok := rawField(body, c, media, "result"); ok {
				fields = append(fields, f)
			}
		}
	}
	return fields
}

func codexBodyFields(e gjson.Result) []bodyField {
	p := e.Get("payload")
	switch e.Get("type").String() {
	case "response_item":
		// The cases mirror reduceCodex's switch exactly, including its discriminators: a
		// tool item is keyed by payload.type, a conversational turn by payload.role (a
		// Codex message carries no payload.type, so keying it on role is what keeps the
		// extractor and the reducer agreeing on which user-pasted images to lift).
		switch {
		case p.Get("type").String() == "function_call":
			args := p.Get("arguments")
			// Codex stores arguments as a JSON-encoded string; the body the reducer
			// records is the unquoted string value, so the canonical content is
			// args.String() while the rewritten span is the quoted raw value.
			if f, ok := rawField(args, args.String(), "application/json", "input"); ok {
				return []bodyField{f}
			}
		case p.Get("type").String() == "custom_tool_call":
			// A custom tool call (for example apply_patch) carries its input as a plain
			// string, which can be a large patch; lift it like any other tool input.
			in := p.Get("input")
			if f, ok := rawField(in, in.String(), "text/plain", "input"); ok {
				return []bodyField{f}
			}
		case p.Get("type").String() == "function_call_output",
			p.Get("type").String() == "custom_tool_call_output":
			out := p.Get("output")
			c, media := bodyContent(out)
			if f, ok := rawField(out, c, media, "result"); ok {
				return []bodyField{f}
			}
		case p.Get("type").String() == "image_generation_call":
			// The generated image rides inline as a base64 result; lift it as a binary
			// attachment so the transcript stays small and the image is stored decoded.
			if f, ok := imageField(p.Get("result")); ok {
				return []bodyField{f}
			}
		case p.Get("role").String() == "user":
			// A user turn can paste images as input_image blocks; lift each, matching the
			// reducer's role=="user" branch. Non-image blocks are left inline.
			return codexImageBlocks(p.Get("content"))
		}
	case "event_msg":
		switch p.Get("type").String() {
		case "image_generation_end":
			if f, ok := imageField(p.Get("result")); ok {
				return []bodyField{f}
			}
		case "user_message":
			return codexImageArray(p.Get("images"))
		}
	}
	return nil
}

// codexImageBlocks lifts every base64 image carried by an input_image-style block in a
// content array (a Codex user message can paste several images). It keys off the
// presence of a base64 image_url rather than the block's declared type, so a new image
// block kind is covered without a code change; non-image blocks are left inline.
func codexImageBlocks(content gjson.Result) []bodyField {
	if !content.IsArray() {
		return nil
	}
	var fields []bodyField
	for _, b := range content.Array() {
		if f, ok := imageField(b.Get("image_url")); ok {
			fields = append(fields, f)
		}
	}
	return fields
}

// codexImageArray lifts every base64 image in a flat array of image strings (the
// `images` field of a user_message event), each a data URI.
func codexImageArray(images gjson.Result) []bodyField {
	if !images.IsArray() {
		return nil
	}
	var fields []bodyField
	for _, img := range images.Array() {
		if f, ok := imageField(img); ok {
			fields = append(fields, f)
		}
	}
	return fields
}

// imageField turns a base64/data-URI image string value into an attachment bodyField,
// decoding it to the binary bytes the CAS stores. It declines when the value is absent,
// already a sentinel, not a string, not a recognizable base64 image, or not decodable,
// so non-image content (and an already-rewritten line) is left untouched. The kind is
// "attachment", which the reducer records on the attachments table rather than a tool
// call.
func imageField(v gjson.Result) (bodyField, bool) {
	if !v.Exists() || v.Index <= 0 || len(v.Raw) == 0 || v.Type != gjson.String {
		return bodyField{}, false
	}
	if _, ok := asCASRef(v); ok {
		return bodyField{}, false
	}
	s := v.String()
	if !looksLikeBase64Image(imageHead(s)) {
		return bodyField{}, false
	}
	decoded, ok := decodeBase64Body(s)
	if !ok {
		return bodyField{}, false
	}
	return bodyField{
		start:   v.Index,
		end:     v.Index + len(v.Raw),
		content: string(decoded),
		media:   imageMediaType(imageHead(s)),
		kind:    bodyKindAttachment,
	}, true
}

func piBodyFields(e gjson.Result) []bodyField {
	if e.Get("type").String() != "message" {
		return nil
	}
	msg := e.Get("message")
	switch msg.Get("role").String() {
	case "assistant":
		var fields []bodyField
		for _, b := range msg.Get("content").Array() {
			if b.Get("type").String() != "toolCall" {
				continue
			}
			args := b.Get("arguments")
			if f, ok := rawField(args, args.Raw, "application/json", "input"); ok {
				fields = append(fields, f)
			}
		}
		return fields
	case "toolResult":
		body := msg.Get("content")
		c, media := bodyContent(body)
		if f, ok := rawField(body, c, media, "result"); ok {
			return []bodyField{f}
		}
	}
	return nil
}

// rawField turns a parsed body value into a bodyField, locating its raw span via
// gjson's Index. It declines (ok=false) when the value is absent, is already a
// sentinel (so re-extraction is idempotent), or its reported span does not equal
// its Raw (a defensive guard: a body whose offset gjson could not pin down is
// left inline rather than rewritten at a wrong span).
func rawField(v gjson.Result, content, media, kind string) (bodyField, bool) {
	if !v.Exists() || v.Index <= 0 || len(v.Raw) == 0 {
		return bodyField{}, false
	}
	if _, ok := asCASRef(v); ok {
		return bodyField{}, false
	}
	return bodyField{
		start:   v.Index,
		end:     v.Index + len(v.Raw),
		content: content,
		media:   media,
		kind:    kind,
	}, true
}

// ExtractBodies lifts every tool input and result body out of a transcript region
// of complete lines, returning the rewritten region (each body replaced by a CAS
// sentinel) and the bodies that were lifted, deduped by sha256 within the call so
// a body that recurs is uploaded once. The region must be line aligned (the
// ingest protocol guarantees it); each line is rewritten independently, so the
// line count, the newline positions, and any non-body bytes are preserved exactly,
// which keeps the rewritten stream resumable and turn aligned.
//
// A line that is not valid JSON, or carries no tool body, passes through
// unchanged. Re-running ExtractBodies over already-rewritten output is a no-op
// (the sentinels are skipped), which is what makes a re-sync of an unchanged file
// upload zero bodies and zero transcript bytes.
func ExtractBodies(agent Agent, region []byte, enc BodyEncoder) ([]byte, []Body, error) {
	out := make([]byte, 0, len(region))
	var bodies []Body
	seen := map[string]bool{}

	start := 0
	emit := func(line []byte) {
		rewritten, lineBodies := RewriteLine(agent, line, enc)
		out = append(out, rewritten...)
		for _, b := range lineBodies {
			if seen[b.SHA256] {
				continue
			}
			seen[b.SHA256] = true
			bodies = append(bodies, b)
		}
	}
	for i := 0; i < len(region); i++ {
		if region[i] != '\n' {
			continue
		}
		emit(region[start : i+1])
		start = i + 1
	}
	if start < len(region) {
		// A line-aligned region ends on a newline, so this only fires defensively
		// for a trailing fragment; pass it through untouched.
		out = append(out, region[start:]...)
	}
	return out, bodies, nil
}

// RewriteLine replaces each tool body in one transcript line with a sentinel and
// returns the rewritten line plus the bodies it lifted. The line keeps its
// trailing newline (or lack of one) untouched: rewriting happens strictly inside
// the JSON value spans, so the line's length changes only by the body/sentinel
// size delta and its boundary stays a boundary. The client uses it to transform
// the transcript one line at a time so a giant tool body is never buffered as part
// of a whole region. enc encodes each lifted body into the bytes the CAS stores and
// names the key; the sentinel carries that key (so the transcript references the
// stored bytes) while still recording the raw body length.
func RewriteLine(agent Agent, line []byte, enc BodyEncoder) ([]byte, []Body) {
	trimmed := line
	var nl []byte
	if n := len(trimmed); n > 0 && trimmed[n-1] == '\n' {
		nl = trimmed[n-1:]
		trimmed = trimmed[:n-1]
	}
	if !gjson.ValidBytes(trimmed) {
		return line, nil
	}
	fields := toolBodyFields(agent, trimmed)
	if len(fields) == 0 {
		return line, nil
	}

	var rewritten []byte
	var bodies []Body
	cursor := 0
	for _, f := range fields {
		// Guard the span against a stale Index: only rewrite when the located span
		// still matches a plausible body region. fields are in source order, so the
		// cursor advances monotonically.
		if f.start < cursor || f.end > len(trimmed) {
			continue
		}
		sha, stored, contentType := enc.EncodeBody([]byte(f.content))
		rewritten = append(rewritten, trimmed[cursor:f.start]...)
		rewritten = append(rewritten, sentinelBytes(sha, len(f.content), f.media)...)
		cursor = f.end
		bodies = append(bodies, Body{
			SHA256:      sha,
			Bytes:       len(f.content),
			MediaType:   f.media,
			Stored:      stored,
			ContentType: contentType,
			Kind:        f.kind,
		})
	}
	rewritten = append(rewritten, trimmed[cursor:]...)
	rewritten = append(rewritten, nl...)
	return rewritten, bodies
}
