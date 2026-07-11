package parser

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
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
	// BodyBase64 treats the value as a JSON string carrying a base64-encoded binary
	// blob (optionally wrapped in a `data:<media>;base64,` URI) and emits the decoded
	// binary bytes. This is the canonical form for the image payloads Codex inlines
	// (image_generation results, data-URI image_url blocks, pasted images): the CAS
	// stores the real PNG/JPEG bytes, not the base64 text, so a reader serves the blob
	// directly under its image media type. The decode ignores \r and \n exactly as
	// encoding/base64 does, so the streamed result is byte-identical to the buffered
	// base64.StdEncoding.DecodeString the extractor uses.
	BodyBase64
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
//
// ctx threads into the BodyArrayText enumeration so a canceled hash or upload of a
// huge array result aborts during the lazy walk rather than scanning the whole
// array. The BodyRaw and BodyJSONString readers stream a single contiguous span
// and do not run a walk, so they take no ctx of their own.
func CanonicalBodyReader(ctx context.Context, f io.ReaderAt, lineOffset int64, span ValueSpan, kind BodyKind) io.Reader {
	switch kind {
	case BodyJSONString:
		return newJSONStringReader(f, lineOffset+span.Start, lineOffset+span.End)
	case BodyBase64:
		return newBase64BodyReader(f, lineOffset+span.Start, lineOffset+span.End)
	case BodyArrayText:
		return newArrayTextReader(ctx, f, lineOffset, span)
	default: // BodyRaw
		// A raw value is its source bytes unchanged; a section reader streams them
		// straight from the file with no buffering of our own. The section reader is
		// wrapped so a file truncated mid-body surfaces as a hard error instead of a
		// short clean EOF that would let the caller hash partial bytes.
		length := span.End - span.Start
		return &lengthEnforcingReader{
			r:         io.NewSectionReader(f, lineOffset+span.Start, length),
			remaining: length,
			start:     lineOffset + span.Start,
			end:       lineOffset + span.End,
		}
	}
}

// lengthEnforcingReader wraps a reader that is supposed to yield exactly a known
// number of bytes (a raw body's declared span) and converts a premature EOF into
// a contextual short-read error. A SectionReader over a truncated file returns a
// clean io.EOF after fewer bytes than the span claims, which would let a caller
// hash a partial body and mint a CAS sentinel for the wrong bytes. Tracking the
// bytes still owed and failing when EOF arrives early keeps a corrupted store from
// being mistaken for a valid shorter body.
type lengthEnforcingReader struct {
	r          io.Reader
	remaining  int64
	start, end int64 // the [start,end) file range, for the error message
}

func (r *lengthEnforcingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.remaining -= int64(n)
	if err == io.EOF && r.remaining > 0 {
		// The underlying stream ended before the declared span was satisfied: the
		// file is shorter than the span claims, a truncation rather than a real end.
		return n, fmt.Errorf("raw body short read at [%d,%d): %d bytes missing: %w", r.start, r.end, r.remaining, io.ErrUnexpectedEOF)
	}
	return n, err
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
			// A trailing unpaired high surrogate has no following byte to trigger
			// flushSurrogate. Emit its replacement rune before surfacing EOF so
			// the streaming path stays byte-for-byte aligned with gjson.String().
			r.flushSurrogate()
			if len(r.out) != r.off {
				break
			}
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
	// want bytes all lie before the declared end of the string, so a short read here
	// is a truncated file, not the natural end of the value. Treat it as a hard
	// error rather than zero-filling, which would silently corrupt the decoded body.
	if err := readFull(r.f, buf, r.pos); err != nil {
		r.err = err
		return err
	}
	r.pos += want
	r.decode(buf)
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
		// A high surrogate not followed by a low one: emit its replacement, then
		// handle the current code point afresh.
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
// astral-plane code points produced by combined surrogate pairs. JSON decoders
// replace an unpaired surrogate with U+FFFD; doing the same here keeps streamed
// bodies aligned with the gjson projection path.
func appendRune4(out []byte, r rune) []byte {
	if !utf8.ValidRune(r) {
		r = utf8.RuneError
	}
	return utf8.AppendRune(out, r)
}

// dataURIBase64Marker is the literal that separates a data URI's media/parameters
// from its base64 payload. A value that contains it (within a short head) is a data
// URI; the bytes after it are the base64 to decode, and the media is the token
// between "data:" and the first ";" or this marker.
const dataURIBase64Marker = ";base64,"

// dataURIScan bounds how far into a value the data-URI prefix detector looks. A real
// data URI header (media type plus parameters) is short; capping the scan keeps the
// detector from buffering a body's worth of bytes while still covering any plausible
// header, and makes the streaming and buffered paths agree on a fixed window.
const dataURIScan = 256

// stripDataURIPrefix removes a leading `data:<media>;base64,` wrapper from a base64
// body, returning the bare base64. A value with no such prefix (raw base64) is
// returned unchanged. The streaming reader strips the identical prefix over a peek of
// the same bounded window, so both paths decode the same bytes.
func stripDataURIPrefix(s string) string {
	if !strings.HasPrefix(s, "data:") {
		return s
	}
	head := s
	if len(head) > dataURIScan {
		head = head[:dataURIScan]
	}
	if i := strings.Index(head, dataURIBase64Marker); i >= 0 {
		return s[i+len(dataURIBase64Marker):]
	}
	return s
}

// imageMediaType returns the semantic media type for a base64/data-URI image body,
// read from the data-URI media token when present and otherwise sniffed from the
// base64 magic prefix. head is the first bytes of the string value (before decoding);
// base64/data-URI content is pure ASCII with no JSON escapes, so the raw head bytes
// are the literal content. An unrecognized blob falls back to application/octet-stream
// so a non-image is still stored faithfully rather than mislabeled.
func imageMediaType(head string) string {
	if strings.HasPrefix(head, "data:") {
		if i := strings.Index(head, dataURIBase64Marker); i >= 0 {
			media := head[len("data:"):i]
			if j := strings.IndexByte(media, ';'); j >= 0 {
				media = media[:j] // drop any ;charset= or other parameters
			}
			if media != "" {
				return media
			}
		}
		// A data: URI we could not fully parse: sniff the payload after the marker.
		head = stripDataURIPrefix(head)
	}
	return sniffBase64ImageMedia(head)
}

// sniffBase64ImageMedia maps the leading characters of a raw base64 blob to an image
// media type by their decoded magic bytes. The prefixes are the base64 encodings of
// each format's signature: "iVBOR" is \x89PNG, "/9j/" is the JPEG SOI, "R0lGOD" is
// GIF8, "UklGR" is the RIFF header WebP rides on. Anything else is treated as opaque.
func sniffBase64ImageMedia(b64 string) string {
	switch {
	case strings.HasPrefix(b64, "iVBOR"):
		return "image/png"
	case strings.HasPrefix(b64, "/9j/"):
		return "image/jpeg"
	case strings.HasPrefix(b64, "R0lGOD"):
		return "image/gif"
	case strings.HasPrefix(b64, "UklGR"):
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// looksLikeBase64Image reports whether a string value's head is a base64 image worth
// lifting: a data-URI base64 wrapper, or raw base64 whose magic matches a known image
// signature. It gates image extraction so a field that is not actually an encoded
// image is left inline (and, if large, rides inline via the client's big-line
// fallback) rather than lifted and then failing to base64-decode mid-upload.
func looksLikeBase64Image(head string) bool {
	if strings.HasPrefix(head, "data:") && strings.Contains(head[:min(len(head), dataURIScan)], dataURIBase64Marker) {
		return true
	}
	return sniffBase64ImageMedia(head) != "application/octet-stream"
}

// imageHeadLen bounds how many leading characters of a string value the buffered
// extractor inspects to classify it (data-URI prefix or base64 magic). It matches the
// window the streaming peek uses so both paths classify a value identically.
const imageHeadLen = dataURIScan

// imageHead returns the leading bytes of a string value used to classify it as an
// image and pick its media type, bounded so a huge base64 blob is not sliced whole.
func imageHead(s string) string {
	if len(s) > imageHeadLen {
		return s[:imageHeadLen]
	}
	return s
}

// decodeBase64Body decodes a base64/data-URI string value into its raw binary bytes,
// the buffered counterpart of newBase64BodyReader. It strips any data-URI wrapper and
// base64-decodes the rest with the same StdEncoding the streaming decoder uses, so a
// body buffered in memory and one streamed from disk produce identical bytes (and so
// an identical CAS key). It declines (ok=false) when the value is not valid base64, so
// a misclassified field is left inline rather than lifted to a body that cannot decode.
func decodeBase64Body(s string) ([]byte, bool) {
	decoded, err := base64.StdEncoding.DecodeString(stripDataURIPrefix(s))
	if err != nil {
		return nil, false
	}
	return decoded, true
}

// newBase64BodyReader streams the decoded binary bytes of a base64/data-URI string
// value whose raw span is [rawStart,rawEnd) in f (rawStart points at the opening
// quote). It layers a base64 decoder over the JSON-string reader, first discarding any
// `data:<media>;base64,` prefix, so a hundreds-of-MiB image decodes in O(window)
// memory and the bytes it yields match the buffered base64.StdEncoding.DecodeString of
// the same value exactly (both ignore \r and \n).
func newBase64BodyReader(f io.ReaderAt, rawStart, rawEnd int64) io.Reader {
	src := newJSONStringReader(f, rawStart, rawEnd)
	br := bufio.NewReaderSize(src, dataURIScan*2)
	skipDataURIPrefix(br)
	return base64.NewDecoder(base64.StdEncoding, br)
}

// skipDataURIPrefix consumes a leading `data:<media>;base64,` wrapper from br when
// present, so the base64 decoder downstream sees only the payload. It peeks a bounded
// head (never more than the source holds) and discards exactly through the marker; a
// value with no such prefix is left untouched. A peek error is non-fatal: there is
// then no prefix to strip and the decoder reports any real read failure itself.
func skipDataURIPrefix(br *bufio.Reader) {
	head, err := br.Peek(dataURIScan)
	if len(head) == 0 && err != nil {
		return
	}
	if !strings.HasPrefix(string(head), "data:") {
		return
	}
	if i := strings.Index(string(head), dataURIBase64Marker); i >= 0 {
		_, _ = br.Discard(i + len(dataURIBase64Marker))
	}
}

// raw span is span within the line beginning at lineOffset in f. It is fully lazy:
// rather than enumerating every contributing piece up front, it drives one
// WalkArrayElements pass that is paced by reads, pulling the next contributing
// piece span only when the current piece is drained. The reader therefore holds
// only the current piece's decoding reader plus the walk's O(depth) state, never a
// list of all piece spans, so an array result with very many text blocks streams in
// bounded memory. ctx aborts the walk promptly when the caller cancels.
func newArrayTextReader(ctx context.Context, f io.ReaderAt, lineOffset int64, span ValueSpan) io.Reader {
	regionBase := lineOffset + span.Start
	r := &arrayTextReader{
		ctx:        ctx,
		f:          f,
		lineOffset: lineOffset,
		regionBase: regionBase,
		pieces:     make(chan pieceSpan),
		done:       make(chan struct{}),
	}
	go r.walk(ctx, regionBase, span)
	return r
}

// pieceSpan carries one contributing piece's line-relative span (or a walk error)
// from the enumeration goroutine to the reader. err non-nil ends the stream.
type pieceSpan struct {
	span ValueSpan
	err  error
}

// arrayTextReader streams blockText over an array by pacing a single
// WalkArrayElements pass with the consumer's reads. The walk runs in a goroutine
// and hands one contributing piece span at a time over the pieces channel, blocking
// until the reader is ready for the next, so at most one piece span is in flight and
// nothing accumulates. The reader emits a single "\n" between consecutive pieces
// (never leading or trailing, the strings.Join(parts, "\n") shape) and constructs
// each piece's decoding reader only when the previous piece is fully drained,
// bounding memory to one window.
type arrayTextReader struct {
	ctx        context.Context
	f          io.ReaderAt
	lineOffset int64
	regionBase int64

	// pieces delivers contributing piece spans from the walk goroutine in source
	// order; it is closed when the walk finishes. done is closed by the reader to
	// signal early teardown so a blocked walk goroutine can exit.
	pieces chan pieceSpan
	done   chan struct{}

	cur     *jsonStringReader // the piece currently being drained, or nil
	started bool              // whether any piece has been emitted yet (separator gating)
	needSep bool              // a newline is owed before the next piece begins
	eof     bool              // the walk has signaled completion
	err     error             // a terminal walk error, surfaced on the next Read
	stopped bool              // done has been closed (idempotent teardown guard)
}

// stop releases the walk goroutine if it is parked on a send, so a reader that
// reaches its terminal state (EOF or error) never strands the goroutine. It is
// idempotent because Read can hit a terminal branch more than once.
func (r *arrayTextReader) stop() {
	if !r.stopped {
		r.stopped = true
		close(r.done)
	}
}

// walk runs the single WalkArrayElements pass, sending each contributing piece's
// line-relative span over the pieces channel. It mirrors blockText exactly: a bare
// string element contributes its own span; an object element contributes its "text"
// span only when its "type" is one of the text-like kinds. Spans the walker reports
// are relative to the array region's first byte, so they are rebased by span.Start
// to become line-relative like the consumer expects. The goroutine exits when the
// walk ends, an error occurs, or the reader closes done.
func (r *arrayTextReader) walk(ctx context.Context, regionBase int64, span ValueSpan) {
	defer close(r.pieces)
	regionLen := span.End - span.Start
	send := func(ps pieceSpan) error {
		select {
		case r.pieces <- ps:
			return nil
		case <-r.done:
			// The reader was closed; stop the walk by returning an error that
			// WalkArrayElements propagates without further visits.
			return errReaderClosed
		case <-ctx.Done():
			// The caller canceled. Releasing the parked goroutine on ctx (not only on the
			// reader's explicit stop) means a consumer that abandons the reader without
			// reading to EOF, but cancels its context, never strands this goroutine.
			return ctx.Err()
		}
	}
	err := WalkArrayElements(ctx, []Step{}, []Step{Key("type"), Key("text")},
		sectionNext(r.f, regionBase, regionLen),
		func(_ int, elem ValueSpan, subs map[Step]ValueSpan) error {
			typSpan, hasType := subs[Key("type")]
			if !hasType {
				// A bare string element (no nested type) is itself the text, matching
				// blockText's b.Type == String branch.
				isStr, err := isStringSpan(r.f, regionBase+elem.Start)
				if err != nil {
					return err
				}
				if isStr {
					return send(pieceSpan{span: rebase(elem, span.Start)})
				}
				return nil
			}
			// An object block contributes its text only for a text-like type.
			kind, err := readSpanString(r.f, regionBase, typSpan)
			if err != nil {
				return err
			}
			switch kind {
			case "text", "output_text", "input_text":
				if textSpan, ok := subs[Key("text")]; ok {
					return send(pieceSpan{span: rebase(textSpan, span.Start)})
				}
			}
			return nil
		})
	if err != nil && err != errReaderClosed {
		// Surface a walk failure (including ctx cancellation) to the reader, unless
		// the reader itself tore the walk down. The send selects on done too so a
		// reader that has already stopped does not strand this goroutine.
		_ = send(pieceSpan{err: err})
	}
}

// errReaderClosed signals that the consumer closed the reader, so the walk
// goroutine should stop quietly rather than reporting a failure.
var errReaderClosed = fmt.Errorf("array text reader closed")

func (r *arrayTextReader) Read(p []byte) (int, error) {
	for {
		if r.err != nil {
			return 0, r.err
		}
		// Surface cancellation directly, independent of the walk goroutine's delivery
		// race: an already-canceled context must abort the read rather than streaming
		// the array or returning a premature EOF.
		if err := r.ctx.Err(); err != nil {
			r.err = err
			r.stop()
			return 0, err
		}
		// Drain the current piece first; on exhaustion clear cur and arm a separator
		// when another piece follows (decided when the next piece arrives).
		if r.cur != nil {
			n, err := r.cur.Read(p)
			if err == io.EOF {
				r.cur = nil
				r.needSep = true
				err = nil
			}
			if n > 0 || err != nil {
				return n, err
			}
			continue
		}
		// Pull the next contributing piece span, blocking until the walk produces it.
		if r.eof {
			return 0, io.EOF
		}
		ps, ok := <-r.pieces
		if !ok {
			// The walk finished and closed the channel: the stream is complete.
			r.eof = true
			r.stop()
			return 0, io.EOF
		}
		if ps.err != nil {
			r.err = ps.err
			r.stop()
			return 0, r.err
		}
		// Emit the separator before the piece's bytes so it lands strictly between
		// two contributing pieces, never leading or trailing.
		if r.started && r.needSep {
			r.needSep = false
			r.cur = newJSONStringReader(r.f, r.lineOffset+ps.span.Start, r.lineOffset+ps.span.End)
			if len(p) == 0 {
				return 0, nil
			}
			p[0] = '\n'
			return 1, nil
		}
		r.started = true
		r.needSep = false
		r.cur = newJSONStringReader(r.f, r.lineOffset+ps.span.Start, r.lineOffset+ps.span.End)
	}
}

// rebase shifts an array-region-relative span to be line-relative by adding the
// array's own start offset.
func rebase(s ValueSpan, base int64) ValueSpan {
	return ValueSpan{Start: s.Start + base, End: s.End + base}
}

// isStringSpan reports whether the value at fileOff begins with a quote, i.e. it
// is a JSON string. A bare string array element is detected this way so it can be
// decoded as a piece. A read error is propagated rather than swallowed: the byte is
// known to exist (the element span is non-empty), so a short read means the file is
// truncated, which must not be silently treated as "not a string".
func isStringSpan(f io.ReaderAt, fileOff int64) (bool, error) {
	var one [1]byte
	if err := readFull(f, one[:], fileOff); err != nil {
		return false, err
	}
	return one[0] == '"', nil
}

// readSpanStringCap bounds how many decoded bytes readSpanString will accept. A
// block discriminator (its "type") is always a short token, so a value that
// decodes past this cap is malformed or hostile and is rejected rather than
// buffered, keeping this structural read from ever holding a body's worth of bytes.
const readSpanStringCap = 64 << 10

// readSpanString decodes a small JSON string value (a block's type) into memory,
// refusing to buffer more than readSpanStringCap bytes. Only structural strings
// flow through here; the large text bodies stream via newJSONStringReader instead.
// Exceeding the cap returns an error rather than silently truncating, because a
// truncated discriminator could be misclassified.
func readSpanString(f io.ReaderAt, base int64, rel ValueSpan) (string, error) {
	rd := newJSONStringReader(f, base+rel.Start, base+rel.End)
	limited := io.LimitReader(rd, readSpanStringCap+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", err
	}
	if len(data) > readSpanStringCap {
		return "", fmt.Errorf("block type string exceeds %d-byte cap", readSpanStringCap)
	}
	return string(data), nil
}

// sectionNext adapts a file region into the chunked next() that LocateValues and
// WalkArrayElements consume, reading in bounded windows so the walker stays
// O(window) over the region rather than buffering it. Each window is read in full
// or the read fails: a short read before the declared region end is a truncated
// file (surfaced as an error), while reaching the region end returns a clean
// io.EOF. Using readFull rather than a SectionReader prevents a truncation from
// masquerading as a normal end-of-stream.
func sectionNext(f io.ReaderAt, start, length int64) func() ([]byte, error) {
	pos := int64(0)
	buf := make([]byte, jsonStringChunk)
	return func() ([]byte, error) {
		if pos >= length {
			return nil, io.EOF
		}
		n := length - pos
		if n > int64(len(buf)) {
			n = int64(len(buf))
		}
		window := buf[:n]
		if err := readFull(f, window, start+pos); err != nil {
			return nil, err
		}
		pos += n
		var perr error
		if pos >= length {
			perr = io.EOF
		}
		return window, perr
	}
}
