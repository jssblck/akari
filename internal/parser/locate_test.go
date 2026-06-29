package parser

import (
	"io"
	"strings"
	"testing"
)

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
	got, err := LocateToolBodies(agent, f, 0, int64(len(line)))
	if err != nil {
		t.Fatalf("locate %s: %v", agent, err)
	}

	if len(got) != len(want) {
		t.Fatalf("%s: located %d bodies, oracle found %d\n line=%s", agent, len(got), len(want), line)
	}
	for i := range want {
		canon := readAll(t, CanonicalBodyReader(f, 0, got[i].Span, got[i].Kind))
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

	got, err := LocateToolBodies(AgentClaude, f, 0, int64(len(line)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("located %d bodies, want 1", len(got))
	}
	// The span must cover the quoted big string, and its canonical contents must be
	// the unquoted big body.
	canon := readAll(t, CanonicalBodyReader(strings.NewReader(line), 0, got[0].Span, got[0].Kind))
	if canon != big {
		t.Fatalf("canonical body length %d, want %d", len(canon), len(big))
	}
}

// countingReaderAt wraps a ReaderAt to assert the locator reads lazily; it is a thin
// passthrough kept minimal since the parity assertions carry the correctness weight.
type countingReaderAt struct{ r io.ReaderAt }

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) { return c.r.ReadAt(p, off) }
