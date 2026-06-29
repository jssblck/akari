package upload

import (
	"os"
	"strings"
	"testing"

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

// concatChunks joins the transformed chunk data in order.
func concatChunks(chunks []transformedChunk) []byte {
	var out []byte
	for _, c := range chunks {
		out = append(out, c.Data...)
	}
	return out
}

// TestTransformPassesThroughBodylessLines confirms a transcript with no tool body
// transforms to itself: the rewritten stream is byte identical, so a plain session
// uploads exactly its raw bytes.
func TestTransformPassesThroughBodylessLines(t *testing.T) {
	content := claudeLine("hello") + claudeLine("world")
	f, size := openTemp(t, content)

	res, err := transformTail(f, 0, size, "claude", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(concatChunks(res.chunks)); got != content {
		t.Fatalf("transformed = %q, want identity %q", got, content)
	}
	if res.origEnd != size {
		t.Fatalf("origEnd = %d, want %d", res.origEnd, size)
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

	res, err := transformTail(f, 0, size, "claude", true)
	if err != nil {
		t.Fatal(err)
	}
	out := string(concatChunks(res.chunks))
	if strings.Contains(out, `"file_path":"a.go"`) {
		t.Errorf("input body still inline in transformed transcript: %s", out)
	}
	if strings.Contains(out, "package a") {
		t.Errorf("result body still inline in transformed transcript: %s", out)
	}
	if !strings.Contains(out, sentinelMarker) {
		t.Errorf("transformed transcript carries no sentinel: %s", out)
	}

	// Two bodies, with content equal to what the parser records inline.
	var bodies []parser.Body
	for _, c := range res.chunks {
		bodies = append(bodies, c.Bodies...)
	}
	if len(bodies) != 2 {
		t.Fatalf("lifted %d bodies, want 2", len(bodies))
	}
	want := map[string]string{
		`{"file_path":"a.go"}`: "input",
		"package a":            "result",
	}
	for _, b := range bodies {
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
	res, err := transformTail(f, 0, size, "codex", false)
	if err != nil {
		t.Fatal(err)
	}
	wantUnsettled := meta + user("a") + asst("x") + user("b")
	if got := string(concatChunks(res.chunks)); got != wantUnsettled {
		t.Fatalf("unsettled transform = %q, want through the closing user line %q", got, wantUnsettled)
	}
	if res.origEnd != int64(len(wantUnsettled)) {
		t.Fatalf("unsettled origEnd = %d, want %d", res.origEnd, len(wantUnsettled))
	}

	// Settled: the final turn is flushed whole.
	res, err = transformTail(f, 0, size, "codex", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(concatChunks(res.chunks)); got != content {
		t.Fatalf("settled transform = %q, want the whole file", got)
	}
}

// TestTransformResumesFromOrigBase confirms a second pass that starts where the
// first left off transforms only the appended tail.
func TestTransformResumesFromOrigBase(t *testing.T) {
	first := claudeLine("one")
	content := first + claudeLine("two")
	f, size := openTemp(t, content)

	res, err := transformTail(f, int64(len(first)), size, "claude", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(concatChunks(res.chunks)); got != claudeLine("two") {
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

	if _, err := transformTail(f, 0, size, "claude", true); err == nil {
		t.Fatal("expected an oversized-line error past hardCap")
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
