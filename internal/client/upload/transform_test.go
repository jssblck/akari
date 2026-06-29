package upload

import (
	"context"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/casenc"
	"github.com/jssblck/akari/internal/parser"
)

// openTemp writes content to a temp file and returns it open with its size.
func openTemp(t *testing.T, content string) (*os.File, int64) {
	t.Helper()
	f, err := os.Open(tempFile(t, content))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return f, info.Size()
}

// liftedBody is what collectSink records per body: the descriptor the protocol would
// upload (key over the STORED bytes, raw byte length, semantic media, storage content
// type) plus the recovered raw canonical content for assertions. The raw content is
// kept here rather than on parser.Body, which carries only the encoded stored bytes.
type liftedBody struct {
	SHA256      string
	Bytes       int
	MediaType   string
	ContentType string
	Kind        string
	Content     string // raw canonical content, recovered for assertions
}

// collectSink is a test chunkSink that records the transformed bytes and lifted
// bodies in order, without a server. It encodes each body exactly as the real sink
// does (using the same casenc encoder: stored bytes in hand for a small line, or by
// streaming the file span for a big one), so the recorded keys, lengths, media, and
// storage content types match what the protocol would upload.
type collectSink struct {
	enc      *casenc.Encoder
	data     []byte
	bodies   []liftedBody
	origEnd  int64
	confAt   int // emit conflict on the chunk with this 1-based index, 0 to never
	chunkNum int
}

func (s *collectSink) emitBody(ctx context.Context, ref bodyRef) (parser.Body, error) {
	if s.enc == nil {
		s.enc = casenc.New()
	}
	var sha, contentType string
	var rawLen int
	var content string
	if ref.haveContent {
		sha, contentType, rawLen = ref.sha, ref.contentType, ref.rawLen
		// A raw-stored small body's stored bytes are its canonical content; a
		// compressed one cannot be recovered here without decoding, and no assertion
		// needs it.
		if contentType == parser.ContentRaw {
			content = string(ref.stored)
		}
	} else {
		var err error
		sha, contentType, rawLen, err = s.enc.HashStream(ctx, ref.canonicalReader(ctx))
		if err != nil {
			return parser.Body{}, err
		}
		data, err := io.ReadAll(ref.canonicalReader(ctx))
		if err != nil {
			return parser.Body{}, err
		}
		content = string(data)
	}
	s.bodies = append(s.bodies, liftedBody{
		SHA256: sha, Bytes: rawLen, MediaType: ref.media,
		ContentType: contentType, Kind: ref.kind, Content: content,
	})
	return parser.Body{
		SHA256: sha, Bytes: rawLen, MediaType: ref.media,
		ContentType: contentType, Kind: ref.kind,
	}, nil
}

func (s *collectSink) emitChunk(ctx context.Context, data []byte, origLen int64) (bool, error) {
	s.chunkNum++
	if s.confAt != 0 && s.chunkNum == s.confAt {
		return true, nil
	}
	s.data = append(s.data, data...)
	s.origEnd += origLen
	return false, nil
}

// runTransform drives a transformer over [origStart, size) with a collecting sink
// and returns it after the pass.
func runTransform(t *testing.T, f *os.File, origStart, size int64, agent string, settled bool) *collectSink {
	t.Helper()
	enc := casenc.New()
	sink := &collectSink{enc: enc}
	tr := newTransformer(f, origStart, size, agent, sink, enc, nil, 0)
	if _, _, err := tr.run(context.Background(), settled); err != nil {
		t.Fatal(err)
	}
	return sink
}

// TestTransformPassesThroughBodylessLines confirms a transcript with no tool body
// transforms to itself: the rewritten stream is byte identical, so a plain session
// uploads exactly its raw bytes.
func TestTransformPassesThroughBodylessLines(t *testing.T) {
	content := claudeLine("hello") + claudeLine("world")
	f, size := openTemp(t, content)

	sink := runTransform(t, f, 0, size, "claude", true)
	if got := string(sink.data); got != content {
		t.Fatalf("transformed = %q, want identity %q", got, content)
	}
	if sink.origEnd != size {
		t.Fatalf("origEnd = %d, want %d", sink.origEnd, size)
	}
}

// TestTransformLiftsClaudeToolBodies confirms a Claude tool input and result are
// replaced by sentinels in the transcript and surfaced as bodies whose content is
// exactly what the server would CAS.
func TestTransformLiftsClaudeToolBodies(t *testing.T) {
	assistant := `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.go"}}]}}` + "\n"
	result := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"package a","is_error":false}]}}` + "\n"
	content := assistant + result
	f, size := openTemp(t, content)

	sink := runTransform(t, f, 0, size, "claude", true)
	out := string(sink.data)
	if strings.Contains(out, `"file_path":"a.go"`) {
		t.Errorf("input body still inline in transformed transcript: %s", out)
	}
	if strings.Contains(out, "package a") {
		t.Errorf("result body still inline in transformed transcript: %s", out)
	}
	if !strings.Contains(out, sentinelMarker) {
		t.Errorf("transformed transcript carries no sentinel: %s", out)
	}

	if len(sink.bodies) != 2 {
		t.Fatalf("lifted %d bodies, want 2", len(sink.bodies))
	}
	want := map[string]string{
		`{"file_path":"a.go"}`: "input",
		"package a":            "result",
	}
	for _, b := range sink.bodies {
		kind, ok := want[b.Content]
		if !ok {
			t.Errorf("unexpected lifted body %q", b.Content)
			continue
		}
		if b.Kind != kind {
			t.Errorf("body %q kind = %q, want %q", b.Content, b.Kind, kind)
		}
		if b.SHA256 != parser.HashString(b.Content) || b.Bytes != len(b.Content) {
			t.Errorf("body %q metadata mismatch: sha=%s bytes=%d", b.Content, b.SHA256, b.Bytes)
		}
	}
}

// sentinelMarker is the namespaced key the transform writes in place of a body. It
// is duplicated here (the parser keeps the canonical constant unexported) so the
// test asserts the on-wire shape without widening the parser API.
const sentinelMarker = "__akari_cas__"

// TestTransformCodexTurnBoundaries confirms a Codex chunk closes only after a
// turn-ending user line and the trailing open turn is withheld until settle, the
// same boundary discipline the original protocol enforced, now over the
// transformed stream.
func TestTransformCodexTurnBoundaries(t *testing.T) {
	meta := `{"type":"session_meta","payload":{"cwd":"/x"}}` + "\n"
	user := func(s string) string {
		return `{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":` + jsonString(s) + `}]}}` + "\n"
	}
	asst := func(s string) string {
		return `{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":` + jsonString(s) + `}]}}` + "\n"
	}
	// meta, user a, assistant x, user b (closes turn), assistant y (open trailing).
	content := meta + user("a") + asst("x") + user("b") + asst("y")
	f, size := openTemp(t, content)

	// Unsettled: the trailing open turn (assistant y) is withheld.
	sink := runTransform(t, f, 0, size, "codex", false)
	wantUnsettled := meta + user("a") + asst("x") + user("b")
	if got := string(sink.data); got != wantUnsettled {
		t.Fatalf("unsettled transform = %q, want through the closing user line %q", got, wantUnsettled)
	}
	if sink.origEnd != int64(len(wantUnsettled)) {
		t.Fatalf("unsettled origEnd = %d, want %d", sink.origEnd, len(wantUnsettled))
	}

	// Settled: the final turn is flushed whole.
	sink = runTransform(t, f, 0, size, "codex", true)
	if got := string(sink.data); got != content {
		t.Fatalf("settled transform = %q, want the whole file", got)
	}
}

// TestTransformResumesFromOrigBase confirms a second pass that starts where the
// first left off transforms only the appended tail.
func TestTransformResumesFromOrigBase(t *testing.T) {
	first := claudeLine("one")
	content := first + claudeLine("two")
	f, size := openTemp(t, content)

	sink := runTransform(t, f, int64(len(first)), size, "claude", true)
	if got := string(sink.data); got != claudeLine("two") {
		t.Fatalf("resumed transform = %q, want only the appended line", got)
	}
}

// TestTransformOversizedLineRejected confirms a single transformed line past
// hardCap is refused rather than buffered, mirroring the original oversized-message
// guard. The body is left inline (a giant input that is not a clean JSON value to
// lift), so the line itself exceeds the cap.
func TestTransformOversizedLineRejected(t *testing.T) {
	orig := hardCap
	hardCap = 64
	defer func() { hardCap = orig }()

	// A user line with no tool body, longer than the shrunken cap: nothing to lift,
	// so the whole line stays and trips the per-line cap.
	content := claudeLine(strings.Repeat("x", 200))
	f, size := openTemp(t, content)

	sink := &collectSink{}
	tr := newTransformer(f, 0, size, "claude", sink, casenc.New(), nil, 0)
	if _, _, err := tr.run(context.Background(), true); err == nil {
		t.Fatal("expected an oversized-line error past hardCap")
	}
}

// TestTransformStreamsBigLineBody confirms a tool body larger than bigLineThreshold
// is lifted by the streaming path: the body never rides inline, the sentinel
// replaces it, and the lifted body's hash and length match the canonical content,
// proving the streamed body is byte identical to what the buffered path would store.
func TestTransformStreamsBigLineBody(t *testing.T) {
	setBigLineThreshold(t, 256)

	// A Claude tool_result whose content string is well past the (shrunken) big-line
	// threshold, so the line takes the streaming path.
	big := strings.Repeat("Z", 4096)
	result := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + big + `"}]}}` + "\n"
	f, size := openTemp(t, result)

	sink := runTransform(t, f, 0, size, "claude", true)

	out := string(sink.data)
	if strings.Contains(out, big) {
		t.Fatal("big body still inline after streaming transform")
	}
	if !strings.Contains(out, sentinelMarker) {
		t.Fatalf("streamed line carries no sentinel: %s", out)
	}
	if len(sink.bodies) != 1 {
		t.Fatalf("lifted %d bodies, want 1", len(sink.bodies))
	}
	b := sink.bodies[0]
	if b.Content != big {
		t.Fatalf("streamed body content mismatch (len %d, want %d)", len(b.Content), len(big))
	}
	// The body is well past the compression threshold, so it is stored zstd and keyed
	// by the hash of the compressed bytes, not the raw body. Bytes is still the raw
	// length the sentinel records. The key and encoding must match what the encoder
	// produces for the same content in hand.
	wantSHA, _, wantCT := casenc.New().EncodeBody([]byte(big))
	if b.SHA256 != wantSHA || b.Bytes != len(big) {
		t.Fatalf("streamed body metadata mismatch: sha=%s bytes=%d", b.SHA256, b.Bytes)
	}
	if b.ContentType != wantCT || b.ContentType != parser.ContentZstd {
		t.Fatalf("streamed result content type = %q, want %q", b.ContentType, parser.ContentZstd)
	}
	if b.MediaType != "text/plain" {
		t.Fatalf("streamed result media = %q, want text/plain", b.MediaType)
	}
}

// TestTransformBigAndSmallEquivalent confirms the streaming big-line path and the
// buffered small-line path produce byte-identical transformed output and identical
// lifted bodies for the same line, so the threshold is purely a memory tactic with
// no effect on the result.
func TestTransformBigAndSmallEquivalent(t *testing.T) {
	body := strings.Repeat("payload-", 300) // ~2400 bytes
	line := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + body + `"}]}}` + "\n"
	f, size := openTemp(t, line)

	// Small path: threshold above the line.
	setBigLineThreshold(t, 1<<20)
	small := runTransform(t, f, 0, size, "claude", true)

	// Big path: threshold below the line.
	setBigLineThreshold(t, 256)
	bigp := runTransform(t, f, 0, size, "claude", true)

	if string(small.data) != string(bigp.data) {
		t.Fatalf("big vs small transform differ:\n small=%q\n big  =%q", small.data, bigp.data)
	}
	if len(small.bodies) != 1 || len(bigp.bodies) != 1 {
		t.Fatalf("body counts: small=%d big=%d", len(small.bodies), len(bigp.bodies))
	}
	if small.bodies[0].SHA256 != bigp.bodies[0].SHA256 {
		t.Fatalf("big vs small body hash differ: %s vs %s", small.bodies[0].SHA256, bigp.bodies[0].SHA256)
	}
}

// TestPrefixDigestRecomputesBigBodyKeys covers the cold-cache verification path for a
// big line whose body is lifted by streaming. transformPrefixDigest must reproduce the
// exact transformed bytes the upload produced, which for a big line means re-streaming
// the body through the encoder to recompute its key, and here re-compressing it (the
// key is the hash of the compressed bytes). The existing cold-verify test uses bodyless
// input, so it never reaches the rewriteForDigest big-line branch.
func TestPrefixDigestRecomputesBigBodyKeys(t *testing.T) {
	setBigLineThreshold(t, 256)
	// A result body well past both the big-line threshold and the compression
	// threshold, and compressible, so the cold path streams and zstd-compresses it.
	big := strings.Repeat("compress me ", 400)
	content := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + big + `"}]}}` + "\n"
	f, size := openTemp(t, content)

	// The transform produces the canonical transformed bytes (and would upload the body).
	transformed := runTransform(t, f, 0, size, "claude", true).data

	// The cold path must recompute a byte-identical transformed prefix over the whole
	// file and recover the original cursor, by re-streaming and re-compressing the body.
	h, orig, ok, err := transformPrefixDigest(context.Background(), f, "claude", size, int64(len(transformed)), casenc.New())
	if err != nil || !ok {
		t.Fatalf("cold prefix digest over a big body: ok=%v err=%v", ok, err)
	}
	if orig != size {
		t.Fatalf("recovered original base = %d, want the full size %d", orig, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hexSHA(string(transformed)) {
		t.Fatal("cold prefix digest does not match the transformed bytes the upload produced")
	}
}

// TestRewindResetsBothCursors confirms rewind drops the transformed and original
// cursors together so a re-upload starts from zero.
func TestRewindResetsBothCursors(t *testing.T) {
	fs := &fileSync{base: 50, origBase: 80, prefixSize: 100}
	fs.rewind(150)
	if fs.base != 0 || fs.origBase != 0 {
		t.Fatalf("after rewind base=%d origBase=%d, want 0/0", fs.base, fs.origBase)
	}
	if fs.prefixSize != 150 {
		t.Fatalf("prefixSize = %d, want 150", fs.prefixSize)
	}
}

// setBigLineThreshold temporarily overrides the big-line threshold for a test,
// restoring it on cleanup.
func setBigLineThreshold(t *testing.T, n int64) {
	t.Helper()
	orig := bigLineThreshold
	bigLineThreshold = n
	t.Cleanup(func() { bigLineThreshold = orig })
}

// codexUser / codexAsst build Codex turn lines for the incremental tests.
func codexUser(s string) string {
	return `{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":` + jsonString(s) + `}]}}` + "\n"
}
func codexAsst(s string) string {
	return `{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":` + jsonString(s) + `}]}}` + "\n"
}

// TestTransformOpenTurnResumesFromCache proves the open-Codex-turn cache stops the
// quadratic re-transform: a second tick over a file whose final turn is still open
// scans only the appended delta, not the whole held turn. The scanner's resume offset
// (the cached scanEnd) must land past the lines processed on the first tick.
func TestTransformOpenTurnResumesFromCache(t *testing.T) {
	// meta, then a closed turn (user a, assistant x, user b), then an open trailing
	// turn that grows between ticks.
	meta := `{"type":"session_meta","payload":{"cwd":"/x"}}` + "\n"
	base := meta + codexUser("a") + codexAsst("x") + codexUser("b")
	open1 := codexAsst("y1")
	content1 := base + open1
	path := tempFile(t, content1)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	// First tick: unsettled, so the open turn (assistant y1) is withheld and cached.
	sink1 := &collectSink{}
	tr1 := newTransformer(f, 0, int64(len(content1)), "codex", sink1, casenc.New(), nil, 0)
	if _, _, err := tr1.run(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := string(sink1.data); got != base {
		t.Fatalf("first tick emitted %q, want through the closed turn %q", got, base)
	}
	pend := tr1.snapshot()
	if pend == nil {
		t.Fatal("expected an open-turn snapshot after the first tick")
	}
	if pend.scanEnd != int64(len(content1)) {
		t.Fatalf("cached scanEnd = %d, want past the held open line %d", pend.scanEnd, len(content1))
	}

	// Grow the open turn and settle it. The second tick must resume at scanEnd: its
	// scanner starts there, so it reads only the appended delta.
	open2 := codexAsst("y2")
	if err := os.WriteFile(path, []byte(content1+open2), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := f.Stat()
	size2 := info.Size()

	sink2 := &collectSink{}
	tr2 := newTransformer(f, 0, size2, "codex", sink2, casenc.New(), pend, 0)
	// The resumed scanner must begin at the cached offset, not at origBase 0.
	if tr2.sc.bufBase != pend.scanEnd {
		t.Fatalf("resumed scan base = %d, want cached scanEnd %d", tr2.sc.bufBase, pend.scanEnd)
	}
	if _, _, err := tr2.run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// Settled now, so the whole open turn (y1 cached + y2 new) flushes, and the two
	// ticks together reconstruct the full file.
	full := string(sink1.data) + string(sink2.data)
	if full != content1+open2 {
		t.Fatalf("two ticks reconstructed %q, want the full file %q", full, content1+open2)
	}
}

// TestTransformOversizedOpenTurnBackstop proves the documented constant cap fires: an
// open Codex turn whose rewritten size exceeds maxTurnBytes is force-flushed
// line-aligned rather than held without bound. Memory stays bounded by the cap.
func TestTransformOversizedOpenTurnBackstop(t *testing.T) {
	orig := maxTurnBytes
	maxTurnBytes = 4 << 10
	defer func() { maxTurnBytes = orig }()

	// A long run of assistant lines with no closing user line: a single open turn that
	// never closes. Its rewritten size (no bodies to lift) crosses the shrunken cap.
	meta := `{"type":"session_meta","payload":{"cwd":"/x"}}` + "\n"
	var b strings.Builder
	b.WriteString(meta)
	for i := 0; i < 200; i++ {
		b.WriteString(codexAsst(strings.Repeat("z", 64)))
	}
	content := b.String()
	f, size := openTemp(t, content)

	// Unsettled: without the backstop the whole open turn would be withheld and never
	// emitted. With it, the run force-flushes once it crosses the cap.
	sink := runTransform(t, f, 0, size, "codex", false)
	if len(sink.data) == 0 {
		t.Fatal("backstop did not fire: nothing emitted for an oversized open turn")
	}
	if sink.chunkNum == 0 {
		t.Fatal("expected at least one forced partial chunk")
	}
}

// TestTransformTruncationIsHardError proves a file truncated mid-line during the
// transform aborts rather than splicing zero-filled or partial bytes into the
// transcript. The scanner frames a big line, then the file shrinks before its body is
// read, so the body read must fail.
func TestTransformTruncationIsHardError(t *testing.T) {
	setBigLineThreshold(t, 64)
	big := strings.Repeat("Z", 4096)
	line := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + big + `"}]}}` + "\n"
	path := tempFile(t, line)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	// Claim the original (untruncated) size to the transformer, then truncate the file
	// so the big-line body read runs past EOF and must error.
	fullSize := int64(len(line))
	if err := os.Truncate(path, 128); err != nil {
		t.Fatal(err)
	}

	sink := &collectSink{}
	tr := newTransformer(f, 0, fullSize, "claude", sink, casenc.New(), nil, 0)
	if _, _, err := tr.run(context.Background(), true); err == nil {
		t.Fatal("expected a hard error on a file truncated mid-line, got nil")
	}
}

// TestScannerPartialLineResumesSearch proves the scanner skips re-searching the
// already-scanned prefix of an incomplete trailing line: given a resume offset, it
// finds the newline only in the appended tail. It also confirms the scanner reports
// how far it searched when a line is still incomplete, so the cursor can advance.
func TestScannerPartialLineResumesSearch(t *testing.T) {
	// A complete line, then a partial trailing line with no newline yet.
	complete := claudeLine("done")
	partial := `{"type":"user","message":{"content":"in progress`
	path := tempFile(t, complete+partial)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	size1 := int64(len(complete + partial))

	sc := newOrigLineScanner(f, 0, size1)
	line, _, _, _, ok, err := sc.next()
	if err != nil || !ok || string(line) != complete {
		t.Fatalf("first line: ok=%v err=%v line=%q", ok, err, line)
	}
	_, _, _, _, ok, err = sc.next()
	if err != nil || ok {
		t.Fatalf("partial line should yield no complete line: ok=%v err=%v", ok, err)
	}
	if sc.searchedTo != size1 {
		t.Fatalf("searchedTo = %d, want the full size %d", sc.searchedTo, size1)
	}

	// Append the rest of the partial line plus its newline. A fresh scanner that
	// resumes the search at the cached offset must still find the line and return it
	// whole, having skipped re-searching the old prefix.
	tail := ` more"}}` + "\n"
	if err := os.WriteFile(path, []byte(complete+partial+tail), 0o644); err != nil {
		t.Fatal(err)
	}
	size2 := int64(len(complete + partial + tail))
	resume := newOrigLineScanner(f, int64(len(complete)), size2).withResumeSearch(size1)
	got, _, _, _, ok, err := resume.next()
	if err != nil || !ok {
		t.Fatalf("resumed line: ok=%v err=%v", ok, err)
	}
	if string(got) != partial+tail {
		t.Fatalf("resumed line = %q, want the full partial line %q", got, partial+tail)
	}
}

// TestSeenCacheStaysBounded proves the recently-seen body cache never exceeds its cap,
// so its memory does not grow with the number of distinct bodies.
func TestSeenCacheStaysBounded(t *testing.T) {
	c := newSeenCache()
	for i := 0; i < seenCacheCap*3; i++ {
		c.add(hexSHA(strings.Repeat("x", i%97) + jsonString(string(rune(i)))))
	}
	if len(c.m) > seenCacheCap {
		t.Fatalf("seen cache holds %d entries, cap is %d", len(c.m), seenCacheCap)
	}
}

// setChunkTarget temporarily overrides the chunk target so a small input emits
// multiple chunks, exercising the steady-state drain and commit accounting.
func setChunkTarget(t *testing.T, n int) {
	t.Helper()
	orig := chunkTarget
	chunkTarget = n
	t.Cleanup(func() { chunkTarget = orig })
}

// TestTransformMultiChunkAccounting confirms a stream that emits several chunks (a
// small target over many boundary lines) reassembles to the whole transcript and
// advances the original cursor exactly: the per-chunk commit must drop exactly the
// lines it emitted, no more, no fewer.
func TestTransformMultiChunkAccounting(t *testing.T) {
	setChunkTarget(t, 8) // tiny, so nearly every Claude line flushes its own chunk
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString(claudeLine(strings.Repeat("x", 10)))
	}
	content := b.String()
	f, size := openTemp(t, content)

	sink := runTransform(t, f, 0, size, "claude", true)
	if string(sink.data) != content {
		t.Fatalf("multi-chunk reassembly mismatch:\n got len=%d\n want len=%d", len(sink.data), len(content))
	}
	if sink.origEnd != size {
		t.Fatalf("origEnd = %d, want %d", sink.origEnd, size)
	}
	if sink.chunkNum < 2 {
		t.Fatalf("expected multiple chunks, got %d", sink.chunkNum)
	}
}

// TestTransformConflictUnwindsWithoutAdvancing confirms a chunk conflict stops the
// transform and reports the conflict, without emitting the conflicted chunk's bytes:
// the caller re-announces and the pass is retried, so a conflict must not advance the
// recorded transcript.
func TestTransformConflictUnwindsWithoutAdvancing(t *testing.T) {
	content := claudeLine("a") + claudeLine("b") + claudeLine("c")
	f, size := openTemp(t, content)

	// The small file flushes as one chunk at finish; conflict on it and confirm the
	// transform reports the conflict and accepts nothing.
	sink := &collectSink{confAt: 1}
	tr := newTransformer(f, 0, size, "claude", sink, casenc.New(), nil, 0)
	_, conflicted, err := tr.run(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !conflicted {
		t.Fatal("expected the transform to report the chunk conflict")
	}
	if len(sink.data) != 0 {
		t.Fatalf("conflicted transform recorded %q, want nothing accepted", sink.data)
	}
}

// TestTransformBigLineNoBodyRejected confirms a line past the big-line threshold
// that carries no liftable tool body is refused rather than buffered: there is
// nothing to lift, so the line cannot be made small and must not be read whole.
func TestTransformBigLineNoBodyRejected(t *testing.T) {
	setBigLineThreshold(t, 256)
	// A plain user line with a big inline content string and no tool_result block:
	// nothing to lift to the CAS.
	content := claudeLine(strings.Repeat("x", 4096))
	f, size := openTemp(t, content)

	sink := &collectSink{}
	tr := newTransformer(f, 0, size, "claude", sink, casenc.New(), nil, 0)
	if _, _, err := tr.run(context.Background(), true); err == nil {
		t.Fatal("expected a big-line-with-no-body refusal")
	}
}

// TestScannerDetectsBigLine confirms the scanner reports a line longer than the
// threshold as big (nil bytes, isBig=true) with the correct offset and length, and a
// following small line normally, proving the two-tier handoff.
func TestScannerDetectsBigLine(t *testing.T) {
	setBigLineThreshold(t, 16)
	big := strings.Repeat("Z", 64) + "\n"
	small := "tiny\n"
	f, size := openTemp(t, big+small)

	sc := newOrigLineScanner(f, 0, size)
	line, off, n, isBig, ok, err := sc.next()
	if err != nil || !ok {
		t.Fatalf("first next: ok=%v err=%v", ok, err)
	}
	if !isBig || line != nil {
		t.Fatalf("first line should be big with nil bytes, got isBig=%v line=%q", isBig, line)
	}
	if off != 0 || n != int64(len(big)) {
		t.Fatalf("big line off=%d n=%d, want 0/%d", off, n, len(big))
	}
	line, off, n, isBig, ok, err = sc.next()
	if err != nil || !ok {
		t.Fatalf("second next: ok=%v err=%v", ok, err)
	}
	if isBig || string(line) != small {
		t.Fatalf("second line = %q isBig=%v, want %q small", line, isBig, small)
	}
	if off != int64(len(big)) || n != int64(len(small)) {
		t.Fatalf("small line off=%d n=%d, want %d/%d", off, n, len(big), len(small))
	}
}
