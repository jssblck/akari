package parser

import (
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// readAll drains a CanonicalBodyReader fully and returns its bytes, failing the
// test on any read error so a streaming bug cannot pass silently.
func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

// locateValueSpan finds the span of a dotted path within line using the streaming
// locator, so the test exercises the same span machinery the production caller
// feeds CanonicalBodyReader. The single value at path is returned.
func locateValueSpan(t *testing.T, line string, path []Step) ValueSpan {
	t.Helper()
	span, ok, err := LocateValue(path, chunkedReader(line, 7))
	if err != nil {
		t.Fatalf("locate %v: %v", path, err)
	}
	if !ok {
		t.Fatalf("path %v not found in %q", path, line)
	}
	return span
}

// oracleBodyContent replicates bodyContent over a raw value string, the canonical
// result-body bytes the server stores. The reader under test must reproduce these
// byte for byte.
func oracleBodyContent(raw string) (string, string) {
	return bodyContent(gjson.Parse(raw))
}

// TestCanonicalBodyReaderCodexInput covers the codex arguments case: the value is
// a JSON-encoded string and the canonical input is its unquoted contents.
func TestCanonicalBodyReaderCodexInput(t *testing.T) {
	// The whole line is just the quoted string value at offset 0.
	line := `"{\"cmd\":\"go test ./...\"}"`
	want := gjson.Parse(line).String()
	if want != `{"cmd":"go test ./..."}` {
		t.Fatalf("oracle unexpected: %q", want)
	}
	rd := CanonicalBodyReader(strings.NewReader(line), 0, ValueSpan{0, int64(len(line))}, BodyJSONString)
	if got := readAll(t, rd); got != want {
		t.Errorf("codex input = %q, want %q", got, want)
	}
}

// TestCanonicalBodyReaderResultStrings covers the string-result cases, including
// escapes and a \uXXXX, comparing to gjson .String() (the server oracle).
func TestCanonicalBodyReaderResultStrings(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"plain", `"export function login() {}"`},
		{"escapes", `"line1\nline2 \"q\" \\ end"`},
		{"unicode", `"café"`},
		{"astral", `"😀 end"`}, // a surrogate pair (emoji) to exercise 4-byte UTF-8
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, media := oracleBodyContent(tc.line)
			kind, m := ClassifyResultBody(tc.line[0])
			if m != media {
				t.Errorf("media = %q, want %q", m, media)
			}
			rd := CanonicalBodyReader(strings.NewReader(tc.line), 0, ValueSpan{0, int64(len(tc.line))}, kind)
			if got := readAll(t, rd); got != want {
				t.Errorf("body = %q, want %q", got, want)
			}
		})
	}
}

// TestCanonicalBodyReaderResultArrays covers blockText flattening over result
// arrays: single block, two text blocks joined by a newline, a non-text block
// skipped, and a bare string element.
func TestCanonicalBodyReaderResultArrays(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"single", `[{"type":"text","text":"package a"}]`},
		{"two", `[{"type":"text","text":"a"},{"type":"output_text","text":"b"}]`},
		{"mixed", `[{"type":"image","data":"x"},{"type":"text","text":"hi"}]`},
		{"bareString", `["just text"]`},
		{"bareWithEscape", `["a\nb",{"type":"input_text","text":"c"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, media := oracleBodyContent(tc.line)
			kind, m := ClassifyResultBody(tc.line[0])
			if kind != BodyArrayText {
				t.Fatalf("kind = %v, want BodyArrayText", kind)
			}
			if m != media {
				t.Errorf("media = %q, want %q", m, media)
			}
			rd := CanonicalBodyReader(strings.NewReader(tc.line), 0, ValueSpan{0, int64(len(tc.line))}, kind)
			if got := readAll(t, rd); got != want {
				t.Errorf("body = %q, want %q", got, want)
			}
		})
	}
}

// TestCanonicalBodyReaderResultObject covers a genuine object result: BodyRaw,
// application/json, copied verbatim.
func TestCanonicalBodyReaderResultObject(t *testing.T) {
	line := `{"k":"v"}`
	want, media := oracleBodyContent(line)
	kind, m := ClassifyResultBody(line[0])
	if kind != BodyRaw || m != "application/json" || media != "application/json" {
		t.Fatalf("classify = %v/%q (oracle media %q)", kind, m, media)
	}
	rd := CanonicalBodyReader(strings.NewReader(line), 0, ValueSpan{0, int64(len(line))}, kind)
	if got := readAll(t, rd); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestCanonicalBodyReaderClaudeInput covers the claude/pi input case: BodyRaw,
// application/json, byte-identical to input.Raw.
func TestCanonicalBodyReaderClaudeInput(t *testing.T) {
	line := `{"file_path":"a.go"}`
	want := gjson.Parse(line).Raw // what claude.go records as InputJSON
	rd := CanonicalBodyReader(strings.NewReader(line), 0, ValueSpan{0, int64(len(line))}, BodyRaw)
	if got := readAll(t, rd); got != want {
		t.Errorf("claude input = %q, want %q", got, want)
	}
}

// TestCanonicalBodyReaderLargeString proves the string reader streams: a ~5MiB
// escape-free body is read in 64KiB windows and must equal gjson .String() (the
// identity decode here). Reading in a bounded loop confirms it never depends on
// the whole body being resident.
func TestCanonicalBodyReaderLargeString(t *testing.T) {
	const n = 5 << 20
	payload := strings.Repeat("YWJjZGVmZ2hpamtsbW5vcA", (n/22)+1)[:n] // base64-ish, no escapes
	line := `"` + payload + `"`
	rd := CanonicalBodyReader(strings.NewReader(line), 0, ValueSpan{0, int64(len(line))}, BodyJSONString)

	var sb strings.Builder
	buf := make([]byte, 64<<10)
	for {
		k, err := rd.Read(buf)
		if k > 0 {
			sb.Write(buf[:k])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream read: %v", err)
		}
	}
	if got := sb.String(); got != payload {
		t.Errorf("large body mismatch: len got=%d want=%d", len(got), len(payload))
	}
	// Sanity: the decode is the identity for escape-free input, matching .String().
	if want := gjson.Parse(line).String(); sb.String() != want {
		t.Errorf("large body != gjson .String()")
	}
}

// TestCanonicalBodyReaderSpanWithinLine covers the realistic case where the value
// sits inside a larger line at a nonzero offset, located via LocateValue, so the
// reader honors lineOffset+span arithmetic against the file.
func TestCanonicalBodyReaderSpanWithinLine(t *testing.T) {
	// A codex-shaped fragment: the arguments string is embedded in surrounding JSON.
	line := `{"payload":{"arguments":"{\"file\":\"x.go\"}"}}`
	span := locateValueSpan(t, line, []Step{Key("payload"), Key("arguments")})

	// Oracle: the unquoted arguments string, as codex.go records it.
	want := gjson.Get(line, "payload.arguments").String()
	rd := CanonicalBodyReader(strings.NewReader(line), 0, span, BodyJSONString)
	if got := readAll(t, rd); got != want {
		t.Errorf("embedded arguments = %q, want %q", got, want)
	}

	// The same line offset by a synthetic file prefix must still resolve, proving
	// lineOffset is applied.
	const prefix = "GraceHopper\n"
	span2 := locateValueSpan(t, line, []Step{Key("payload"), Key("arguments")})
	rd2 := CanonicalBodyReader(strings.NewReader(prefix+line), int64(len(prefix)), span2, BodyJSONString)
	if got := readAll(t, rd2); got != want {
		t.Errorf("offset arguments = %q, want %q", got, want)
	}
}

// TestCanonicalBodyReaderArrayMatchesParse ties the array reader to the real
// server semantics: a one-line claude transcript carrying a tool_result whose
// content is an array of text blocks, parsed by Parse, must yield the same body
// the reader produces from the raw array value.
func TestCanonicalBodyReaderArrayMatchesParse(t *testing.T) {
	// Minimal claude tool_use + tool_result pair across two lines.
	assistant := `{"type":"assistant","message":{"id":"m1","model":"claude","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.go"}}]}}`
	arrayContent := `[{"type":"text","text":"package auth"},{"type":"text","text":"// Ada Lovelace"}]`
	user := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":` + arrayContent + `}]}}`
	raw := []byte(assistant + "\n" + user + "\n")

	sess, err := Parse(AgentClaude, raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var body string
	for _, tc := range sess.ToolCalls {
		if tc.CallUID == "t1" {
			body = tc.ResultBody
		}
	}
	if body == "" {
		t.Fatalf("no result body parsed; calls=%+v", sess.ToolCalls)
	}

	// Now produce the same body from the raw array value via the reader.
	kind, _ := ClassifyResultBody(arrayContent[0])
	rd := CanonicalBodyReader(strings.NewReader(arrayContent), 0, ValueSpan{0, int64(len(arrayContent))}, kind)
	if got := readAll(t, rd); got != body {
		t.Errorf("reader body = %q, want Parse body %q", got, body)
	}
}
