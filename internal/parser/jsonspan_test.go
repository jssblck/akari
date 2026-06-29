package parser

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// stepsToGjson renders a Step path as the dotted path gjson expects, so a single
// source of truth drives both the scanner call and the oracle lookup.
func stepsToGjson(path []Step) string {
	parts := make([]string, len(path))
	for i, st := range path {
		switch v := st.(type) {
		case Key:
			parts[i] = string(v)
		case Idx:
			parts[i] = itoa(int(v))
		}
	}
	return strings.Join(parts, ".")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// chunkedReader returns a next() that yields the line in fixed-size chunks,
// proving the scanner works under arbitrary chunking.
func chunkedReader(line string, chunk int) func() ([]byte, error) {
	b := []byte(line)
	pos := 0
	return func() ([]byte, error) {
		if pos >= len(b) {
			return nil, io.EOF
		}
		end := pos + chunk
		if end > len(b) {
			end = len(b)
		}
		out := b[pos:end]
		pos = end
		if pos >= len(b) {
			return out, io.EOF
		}
		return out, nil
	}
}

// oracleSpan returns gjson's authoritative span for a path, plus whether the
// path exists.
func oracleSpan(line, gpath string) (ValueSpan, bool) {
	r := gjson.Get(line, gpath)
	if !r.Exists() {
		return ValueSpan{}, false
	}
	start := int64(r.Index)
	return ValueSpan{Start: start, End: start + int64(len(r.Raw))}, true
}

func TestJSONSpanParity(t *testing.T) {
	cases := []struct {
		name string
		line string
		path []Step
	}{
		{
			name: "claude tool_use input object",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"src/auth.ts"}}]}}`,
			path: []Step{Key("message"), Key("content"), Idx(0), Key("input")},
		},
		{
			name: "claude tool_result content string",
			line: `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"export function login() {}","is_error":false}]}}`,
			path: []Step{Key("message"), Key("content"), Idx(0), Key("content")},
		},
		{
			name: "claude tool_result content array",
			line: `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"package a"}],"is_error":false}]}}`,
			path: []Step{Key("message"), Key("content"), Idx(0), Key("content")},
		},
		{
			name: "codex arguments json-encoded string",
			line: `{"type":"response_item","payload":{"type":"function_call","name":"shell_command","call_id":"c1","arguments":"{\"cmd\":\"go test ./...\"}"}}`,
			path: []Step{Key("payload"), Key("arguments")},
		},
		{
			name: "codex output scalar string",
			line: `{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"ok"}}`,
			path: []Step{Key("payload"), Key("output")},
		},
		{
			name: "pi arguments object",
			line: `{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"read","arguments":{"file_path":"auth.go"}}]}}`,
			path: []Step{Key("message"), Key("content"), Idx(0), Key("arguments")},
		},
		{
			name: "value with braces and brackets inside string",
			line: `{"message":{"content":[{"content":"a{b}[c]\"d"}]}}`,
			path: []Step{Key("message"), Key("content"), Idx(0), Key("content")},
		},
		{
			name: "number scalar value",
			line: `{"a":{"b":42},"c":7}`,
			path: []Step{Key("a"), Key("b")},
		},
		{
			name: "boolean scalar value",
			line: `{"a":{"flag":true},"c":7}`,
			path: []Step{Key("a"), Key("flag")},
		},
		{
			name: "null scalar value",
			line: `{"a":{"v":null}}`,
			path: []Step{Key("a"), Key("v")},
		},
		{
			name: "scalar at end of line",
			line: `{"a":{"n":12345}}`,
			path: []Step{Key("a"), Key("n")},
		},
		{
			name: "nested array of arrays",
			line: `{"a":[[1,2],[3,4]]}`,
			path: []Step{Key("a"), Idx(1)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gpath := stepsToGjson(tc.path)
			want, ok := oracleSpan(tc.line, gpath)
			if !ok {
				t.Fatalf("oracle says path %q absent in %q", gpath, tc.line)
			}

			// Run under several chunk sizes, including one byte at a time, to prove
			// chunk-independence.
			for _, chunk := range []int{1, 3, 7, len(tc.line)} {
				res, err := LocateValues([][]Step{tc.path}, chunkedReader(tc.line, chunk))
				if err != nil {
					t.Fatalf("chunk=%d: LocateValues error: %v", chunk, err)
				}
				if len(res) != 1 {
					t.Fatalf("chunk=%d: want 1 span, got %d (%+v)", chunk, len(res), res)
				}
				got := res[0].Span
				if got != want {
					raw := gjson.Get(tc.line, gpath).Raw
					t.Fatalf("chunk=%d: span mismatch\n got=%+v\nwant=%+v\nraw=%q\nline=%q",
						chunk, got, want, raw, tc.line)
				}
				// Cross-check that the delimited bytes equal gjson's Raw exactly,
				// which is the property the sha256 hashing relies on.
				if string([]byte(tc.line)[got.Start:got.End]) != gjson.Get(tc.line, gpath).Raw {
					t.Fatalf("chunk=%d: delimited bytes != gjson Raw", chunk)
				}
			}
		})
	}
}

func TestJSONSpanLargeValueStreaming(t *testing.T) {
	// Build a ~5 MB string value and confirm the span matches gjson while feeding
	// the line in 64 KiB chunks. The scanner never accumulates value bytes by
	// design (see feed: only object keys are buffered), so a correct span here
	// under chunked input demonstrates streaming behavior end to end.
	big := strings.Repeat("x", 5*1024*1024)
	line := `{"payload":{"output":"` + big + `"}}`
	path := []Step{Key("payload"), Key("output")}
	gpath := stepsToGjson(path)

	want, ok := oracleSpan(line, gpath)
	if !ok {
		t.Fatalf("oracle says path %q absent", gpath)
	}

	res, err := LocateValues([][]Step{path}, chunkedReader(line, 64*1024))
	if err != nil {
		t.Fatalf("LocateValues error: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("want 1 span, got %d", len(res))
	}
	if res[0].Span != want {
		t.Fatalf("span mismatch: got=%+v want=%+v", res[0].Span, want)
	}
}

func TestJSONSpanAbsentPath(t *testing.T) {
	line := `{"message":{"content":[{"input":{"k":"v"}}]}}`
	path := []Step{Key("message"), Key("nope")}
	res, err := LocateValues([][]Step{path}, chunkedReader(line, 5))
	if err != nil {
		t.Fatalf("LocateValues error: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("absent path should yield no span, got %+v", res)
	}

	// LocateValue convenience wrapper reports ok=false.
	if _, ok, err := LocateValue(path, chunkedReader(line, 5)); err != nil || ok {
		t.Fatalf("LocateValue absent: ok=%v err=%v", ok, err)
	}
}

func TestJSONSpanMultiplePathsSourceOrder(t *testing.T) {
	// Two tool_use blocks: both input objects must be located, in source order,
	// each matching gjson.
	line := `{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.ts"}},` +
		`{"type":"tool_use","id":"t2","name":"Write","input":{"file_path":"b.ts","contents":"x"}}` +
		`]}}`
	paths := [][]Step{
		{Key("message"), Key("content"), Idx(0), Key("input")},
		{Key("message"), Key("content"), Idx(1), Key("input")},
	}

	res, err := LocateValues(paths, chunkedReader(line, 11))
	if err != nil {
		t.Fatalf("LocateValues error: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 spans, got %d (%+v)", len(res), res)
	}
	// Source order: the Idx(0) value appears before the Idx(1) value.
	if res[0].PathIndex != 0 || res[1].PathIndex != 1 {
		t.Fatalf("paths not in source order: %+v", res)
	}
	for _, r := range res {
		gpath := stepsToGjson(paths[r.PathIndex])
		want, ok := oracleSpan(line, gpath)
		if !ok {
			t.Fatalf("oracle absent for %q", gpath)
		}
		if r.Span != want {
			t.Fatalf("span mismatch for %q: got=%+v want=%+v", gpath, r.Span, want)
		}
	}
}

// TestJSONSpanPathsRequestedOutOfOrder confirms results come back in source
// order even when the caller lists later-appearing paths first, since the
// streaming scanner records spans as it encounters them.
func TestJSONSpanPathsRequestedOutOfOrder(t *testing.T) {
	line := `{"a":{"x":1,"y":2}}`
	paths := [][]Step{
		{Key("a"), Key("y")},
		{Key("a"), Key("x")},
	}
	res, err := LocateValues(paths, chunkedReader(line, 2))
	if err != nil {
		t.Fatalf("LocateValues error: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 spans, got %d", len(res))
	}
	// "x" appears before "y" in the source, so its result comes first regardless
	// of request order.
	if res[0].PathIndex != 1 || res[1].PathIndex != 0 {
		t.Fatalf("expected source order (x then y): %+v", res)
	}
}

// TestWalkArrayElementsParity checks the single-pass array walker against gjson:
// every element span and every matched subKey span must equal gjson's authoritative
// span, elements arrive in source order, and a subKey absent from an element is
// reported absent. It runs each line under several chunk sizes to prove the walk is
// chunk-independent and truly single-pass (one next() drain per call).
func TestWalkArrayElementsParity(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		arr     []Step
		gprefix string // gjson dotted prefix of the array (empty for a root array)
		subKeys []Step
	}{
		{
			name:    "claude assistant content blocks",
			line:    `{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"},{"type":"tool_use","id":"t1","name":"Read","input":{"x":1}},{"type":"tool_use","id":"t2","name":"Write","input":{"y":[1,2,3]}}]}}`,
			arr:     []Step{Key("message"), Key("content")},
			gprefix: "message.content",
			subKeys: []Step{Key("type"), Key("input")},
		},
		{
			name:    "claude result text array",
			line:    `[{"type":"text","text":"line one"},{"type":"output_text","text":"line two"}]`,
			arr:     []Step{},
			gprefix: "",
			subKeys: []Step{Key("type"), Key("text")},
		},
		{
			name:    "mixed bare strings, scalars, objects",
			line:    `["just text",{"type":"input_text","text":"c"},42,{"type":"text","text":"hi"},true]`,
			arr:     []Step{},
			gprefix: "",
			subKeys: []Step{Key("type"), Key("text")},
		},
		{
			name:    "empty array yields no elements",
			line:    `{"message":{"content":[]}}`,
			arr:     []Step{Key("message"), Key("content")},
			gprefix: "message.content",
			subKeys: []Step{Key("type")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arrGjson := tc.gprefix
			if arrGjson == "" {
				arrGjson = "@this"
			}
			want := gjson.Get(tc.line, arrGjson).Array()

			for _, chunk := range []int{1, 4, 13, len(tc.line)} {
				var idx int
				err := WalkArrayElements(tc.arr, tc.subKeys, chunkedReader(tc.line, chunk),
					func(i int, elem ValueSpan, subs map[Step]ValueSpan) error {
						if i != idx {
							t.Fatalf("chunk=%d: element index %d, expected %d", chunk, i, idx)
						}
						gp := tc.gprefix
						if gp != "" {
							gp += "."
						}
						gp += itoa(i)
						g := gjson.Get(tc.line, gp)
						if int64(g.Index) != elem.Start || int64(g.Index+len(g.Raw)) != elem.End {
							t.Errorf("chunk=%d elem %d: span [%d,%d), want [%d,%d)", chunk, i, elem.Start, elem.End, g.Index, g.Index+len(g.Raw))
						}
						for _, sk := range tc.subKeys {
							k := string(sk.(Key))
							gs := gjson.Get(tc.line, gp+"."+k)
							span, have := subs[sk]
							// gjson reports a present member with a real Index; a member at
							// the very start of the document has Index 0, which cannot occur
							// here since elements are never at offset 0.
							present := gs.Exists() && gs.Index > 0
							if present != have {
								t.Errorf("chunk=%d elem %d sub %q: present=%v have=%v", chunk, i, k, present, have)
								continue
							}
							if have {
								if int64(gs.Index) != span.Start || int64(gs.Index+len(gs.Raw)) != span.End {
									t.Errorf("chunk=%d elem %d sub %q: span [%d,%d), want [%d,%d)", chunk, i, k, span.Start, span.End, gs.Index, gs.Index+len(gs.Raw))
								}
							}
						}
						idx++
						return nil
					})
				if err != nil {
					t.Fatalf("chunk=%d: walk error: %v", chunk, err)
				}
				if idx != len(want) {
					t.Fatalf("chunk=%d: walked %d elements, want %d", chunk, idx, len(want))
				}
			}
		})
	}
}

// TestWalkArrayElementsVisitErrorPropagates confirms a visit error aborts the walk
// and surfaces to the caller rather than being swallowed.
func TestWalkArrayElementsVisitErrorPropagates(t *testing.T) {
	line := `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`
	sentinel := errors.New("stop here")
	calls := 0
	err := WalkArrayElements([]Step{}, []Step{Key("type")}, chunkedReader(line, 3),
		func(int, ValueSpan, map[Step]ValueSpan) error {
			calls++
			return sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("walk should abort after first visit error, got %d calls", calls)
	}
}
