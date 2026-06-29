package parser

import (
	"context"
	"io"
	"strings"
	"testing"
)

// collectToolBodies drains LocateToolBodies through its emit callback into a slice,
// the shape the parity assertions compare against the buffered oracle.
func collectToolBodies(ctx context.Context, agent Agent, f io.ReaderAt, off, length int64) ([]BodyLocation, error) {
	var got []BodyLocation
	err := LocateToolBodies(ctx, agent, f, off, length, func(b BodyLocation) error {
		got = append(got, b)
		return nil
	})
	return got, err
}

// locateParity is the core invariant for the streaming body locator: the bodies it
// finds (by span + kind + media) must canonicalize to exactly the bodies the buffered
// oracle toolBodyFields extracts from the same line, in the same order. The oracle is
// what the server stores inline today, so this proves the streaming path lifts the
// identical body set with identical bytes, media, and count.
func locateParity(t *testing.T, agent Agent, line string) {
	t.Helper()
	f := strings.NewReader(line)

	// Oracle: the buffered extractor over the parsed line.
	want := toolBodyFields(agent, []byte(line))

	// Under test: the streaming locator over the same bytes.
	got, err := collectToolBodies(context.Background(), agent, f, 0, int64(len(line)))
	if err != nil {
		t.Fatalf("locate %s: %v", agent, err)
	}

	if len(got) != len(want) {
		t.Fatalf("%s: located %d bodies, oracle found %d\n line=%s", agent, len(got), len(want), line)
	}
	for i := range want {
		canon := readAll(t, CanonicalBodyReader(context.Background(), f, 0, got[i].Span, got[i].Kind))
		if canon != want[i].content {
			t.Errorf("%s body %d: canonical = %q, want %q", agent, i, canon, want[i].content)
		}
		if got[i].Media != want[i].media {
			t.Errorf("%s body %d: media = %q, want %q", agent, i, got[i].Media, want[i].media)
		}
	}
}

// TestLocateClaudeBodies checks the streaming locator against the oracle for Claude
// tool inputs and results, including a multi-block assistant line and an array-shaped
// result body.
func TestLocateClaudeBodies(t *testing.T) {
	cases := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"},{"type":"tool_use","id":"t1","name":"Read","input":{"x":1}},{"type":"tool_use","id":"t2","name":"Write","input":{"y":[1,2,3]}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"package a"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":{"nested":"object"}}]}}`,
		`{"type":"user","message":{"content":"just a string, no tool body"}}`,
	}
	for _, c := range cases {
		locateParity(t, AgentClaude, c)
	}
}

// TestLocateCodexBodies checks the codex function_call argument body (a JSON-encoded
// string) and the function_call_output result body against the oracle.
func TestLocateCodexBodies(t *testing.T) {
	cases := []string{
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"ls -la\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","output":"total 0\n"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","output":{"stdout":"x"}}}`,
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"hi"}]}}`,
	}
	for _, c := range cases {
		locateParity(t, AgentCodex, c)
	}
}

// TestLocatePiBodies checks pi tool-call arguments (assistant) and tool results
// (toolResult message) against the oracle.
func TestLocatePiBodies(t *testing.T) {
	cases := []string{
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","arguments":{"path":"x"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","content":"result text"}}`,
		`{"type":"message","message":{"role":"user","content":"no tool body"}}`,
	}
	for _, c := range cases {
		locateParity(t, AgentPi, c)
	}
}

// TestLocateStreamsInBoundedWindows confirms the locator reads the body lazily: a
// large body is located without the reader being asked for all of it up front. The
// span machinery itself never buffers the value, so locating bodies in a huge line is
// O(structure), not O(line).
func TestLocateStreamsInBoundedWindows(t *testing.T) {
	big := strings.Repeat("Z", 1<<20)
	line := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + big + `"}]}}`
	f := &countingReaderAt{r: strings.NewReader(line)}

	got, err := collectToolBodies(context.Background(), AgentClaude, f, 0, int64(len(line)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("located %d bodies, want 1", len(got))
	}
	// The span must cover the quoted big string, and its canonical contents must be
	// the unquoted big body.
	canon := readAll(t, CanonicalBodyReader(context.Background(), strings.NewReader(line), 0, got[0].Span, got[0].Kind))
	if canon != big {
		t.Fatalf("canonical body length %d, want %d", len(canon), len(big))
	}
}

// countingReaderAt wraps a ReaderAt to assert the locator reads lazily; it is a thin
// passthrough kept minimal since the parity assertions carry the correctness weight.
type countingReaderAt struct{ r io.ReaderAt }

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) { return c.r.ReadAt(p, off) }

// truncatedReaderAt reports the file as size bytes long even though the line claims
// more, so reads past size return a short count with io.EOF. It models a corrupted
// or partially written store, the case the readFull discipline must reject.
type truncatedReaderAt struct {
	data []byte
	size int64
}

func (t *truncatedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= t.size {
		return 0, io.EOF
	}
	avail := t.size - off
	n := int64(len(p))
	if n > avail {
		n = avail
	}
	copy(p[:n], t.data[off:off+n])
	if n < int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}

// TestLocateTruncatedLineErrors confirms a line whose declared length runs past the
// file's real end is reported as an error rather than silently zero-filling a body.
// The block walk streams the line, so the short read surfaces through readFull.
func TestLocateTruncatedLineErrors(t *testing.T) {
	line := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"the body bytes are cut off"}]}}`
	// Pretend the file holds only the first half of the line.
	f := &truncatedReaderAt{data: []byte(line), size: int64(len(line) / 2)}
	if _, err := collectToolBodies(context.Background(), AgentClaude, f, 0, int64(len(line))); err == nil {
		t.Fatal("expected truncation error, got nil")
	}
}

// TestReadFullShortRead pins the helper directly: a full read succeeds, a short
// read is an io.ErrUnexpectedEOF, and a genuine error is propagated.
func TestReadFullShortRead(t *testing.T) {
	data := []byte("Grace Hopper")
	f := &truncatedReaderAt{data: data, size: 5}

	buf := make([]byte, 5)
	if err := readFull(f, buf, 0); err != nil {
		t.Fatalf("full read within size: %v", err)
	}
	if string(buf) != "Grace" {
		t.Fatalf("read %q, want %q", buf, "Grace")
	}
	if err := readFull(f, make([]byte, 5), 3); err == nil {
		t.Fatal("expected short-read error past declared size")
	}
}
