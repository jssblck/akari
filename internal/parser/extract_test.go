package parser

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// idEncoder is an identity BodyEncoder: it stores every body verbatim and keys it by
// the hash of its raw bytes. The parser tests use it to exercise the locate-and-splice
// logic of RewriteLine/ExtractBodies deterministically, independent of the real zstd
// policy (which lives in internal/casenc and is tested there and end to end). With
// identity encoding the sentinel key is HashString(raw), so the existing raw-hash
// parity assertions hold unchanged.
type idEncoder struct{}

func (idEncoder) EncodeBody(raw []byte) (string, []byte, string) {
	return HashString(string(raw)), raw, ContentRaw
}

// tagEncoder is a BodyEncoder whose outputs are deliberately distinct from the raw
// body: a fixed key prefix, a transformed stored form, and the zstd content type. The
// identity encoder above cannot tell whether the parser actually uses the encoder's
// results or just hashes the raw bytes itself, since for identity encoding the two
// coincide. This encoder makes them diverge, so a parser that ignored the injected
// encoder (and fell back to a raw-bytes hash or the raw bytes as stored) would fail the
// assertions below.
type tagEncoder struct{}

func (tagEncoder) EncodeBody(raw []byte) (string, []byte, string) {
	return "key-" + HashString(string(raw)), append([]byte("ENC:"), raw...), ContentZstd
}

// TestRewriteLineUsesEncoderOutput is the dependency-injection contract: RewriteLine
// must key the sentinel and the lifted body on the encoder's stored-byte hash, carry
// the encoder's stored bytes and storage content type, and still record the RAW body
// length. That is what lets the client swap in real zstd encoding without the parser
// (and therefore the server) linking any compression code.
func TestRewriteLineUsesEncoderOutput(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.go"}}]}}` + "\n")
	rewritten, bodies := RewriteLine(AgentClaude, line, tagEncoder{})

	if len(bodies) != 1 {
		t.Fatalf("lifted %d bodies, want 1", len(bodies))
	}
	raw := `{"file_path":"a.go"}`
	wantKey := "key-" + HashString(raw)
	b := bodies[0]
	if b.SHA256 != wantKey {
		t.Errorf("body key = %q, want the encoder's key %q (parser hashed the raw bytes itself?)", b.SHA256, wantKey)
	}
	if string(b.Stored) != "ENC:"+raw {
		t.Errorf("stored bytes = %q, want the encoder's stored form %q", b.Stored, "ENC:"+raw)
	}
	if b.ContentType != ContentZstd {
		t.Errorf("content type = %q, want the encoder's %q", b.ContentType, ContentZstd)
	}
	if b.Bytes != len(raw) {
		t.Errorf("recorded bytes = %d, want the RAW length %d (compression must not change the recorded size)", b.Bytes, len(raw))
	}

	// The sentinel embedded in the rewritten line must carry the encoder's key and the
	// raw body length, so the transcript references the stored bytes.
	if !strings.Contains(string(rewritten), `"sha256":"`+wantKey+`"`) {
		t.Errorf("sentinel does not carry the encoder key %q: %s", wantKey, rewritten)
	}
	if !strings.Contains(string(rewritten), fmt.Sprintf(`"bytes":%d`, len(raw))) {
		t.Errorf("sentinel does not record the raw length %d: %s", len(raw), rewritten)
	}
}

// inlineBodies parses the original transcript and returns, in a stable order, the
// tool input and result bodies the reducer would write to the CAS today: the set
// the client extractor must reproduce exactly.
func inlineBodies(t *testing.T, agent Agent, raw []byte) []Body {
	t.Helper()
	s, err := Parse(agent, raw)
	if err != nil {
		t.Fatalf("parse original: %v", err)
	}
	var bodies []Body
	for _, tc := range s.ToolCalls {
		if tc.InputJSON != "" {
			bodies = append(bodies, Body{
				SHA256: HashString(tc.InputJSON), Bytes: len(tc.InputJSON),
				MediaType: "application/json", Stored: []byte(tc.InputJSON),
				ContentType: ContentRaw, Kind: "input",
			})
		}
		if tc.ResultBody != "" {
			bodies = append(bodies, Body{
				SHA256: HashString(tc.ResultBody), Bytes: len(tc.ResultBody),
				MediaType: tc.ResultMediaType, Stored: []byte(tc.ResultBody),
				ContentType: ContentRaw, Kind: "result",
			})
		}
	}
	// Binary attachments (lifted images) are the third lifted-body kind. On the inline
	// parse Content holds the decoded bytes and SHA256 is left empty (the server keys
	// the blob it writes, exactly as the inline tool-body path does), so the oracle hashes
	// the content to get the key the server would store under: same sha, raw byte length,
	// and media as the client extractor produces.
	for _, a := range s.Attachments {
		bodies = append(bodies, Body{
			SHA256: HashString(a.Content), Bytes: a.Bytes,
			MediaType: a.MediaType, Stored: []byte(a.Content),
			ContentType: ContentRaw, Kind: bodyKindAttachment,
		})
	}
	return bodies
}

func sortBodies(b []Body) {
	sort.Slice(b, func(i, j int) bool {
		if b[i].SHA256 != b[j].SHA256 {
			return b[i].SHA256 < b[j].SHA256
		}
		return b[i].Kind < b[j].Kind
	})
}

// assertSameBodies compares two body sets by sha, bytes, media, and content,
// independent of order.
func assertSameBodies(t *testing.T, got, want []Body) {
	t.Helper()
	sortBodies(got)
	sortBodies(want)
	if len(got) != len(want) {
		t.Fatalf("body count = %d, want %d\ngot=%+v\nwant=%+v", len(got), len(want), got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.SHA256 != w.SHA256 || g.Bytes != w.Bytes || g.MediaType != w.MediaType ||
			g.ContentType != w.ContentType || string(g.Stored) != string(w.Stored) {
			t.Errorf("body %d mismatch:\n got=%+v\nwant=%+v", i, g, w)
		}
	}
}

// TestExtractionParity is the headline invariant: for each agent's sample
// transcript (including one with a base64-image tool result), the bodies the
// client extractor lifts equal exactly what the server reducer would CAS inline,
// down to sha256, byte length, and media type. If these diverge, dedup breaks and
// the frontend cannot serve the bodies.
func TestExtractionParity(t *testing.T) {
	cases := []struct {
		name  string
		agent Agent
		raw   []byte
	}{
		{"claude", AgentClaude, loadFixture(t, "claude.jsonl")},
		{"codex", AgentCodex, loadFixture(t, "codex.jsonl")},
		{"pi", AgentPi, loadFixture(t, "pi.jsonl")},
		{"claude-image", AgentClaude, claudeImageTranscript()},
		{"codex-image", AgentCodex, codexImageTranscript()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, extracted, err := ExtractBodies(c.agent, c.raw, idEncoder{})
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			assertSameBodies(t, extracted, inlineBodies(t, c.agent, c.raw))
		})
	}
}

// TestRoundTripProjection confirms that parsing the transformed transcript (bodies
// replaced by sentinels) yields the same projection as parsing the original: the
// same messages, the same tool_call input/result sha256/bytes/media, and the same
// usage. This is what makes the on-wire ref format equivalent to the inline one.
func TestRoundTripProjection(t *testing.T) {
	cases := []struct {
		name  string
		agent Agent
		raw   []byte
	}{
		{"claude", AgentClaude, loadFixture(t, "claude.jsonl")},
		{"codex", AgentCodex, loadFixture(t, "codex.jsonl")},
		{"pi", AgentPi, loadFixture(t, "pi.jsonl")},
		{"claude-image", AgentClaude, claudeImageTranscript()},
		{"codex-image", AgentCodex, codexImageTranscript()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig, err := Parse(c.agent, c.raw)
			if err != nil {
				t.Fatalf("parse original: %v", err)
			}
			transformed, _, err := ExtractBodies(c.agent, c.raw, idEncoder{})
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			roundTripped, err := Parse(c.agent, transformed)
			if err != nil {
				t.Fatalf("parse transformed: %v", err)
			}

			if len(orig.Messages) != len(roundTripped.Messages) {
				t.Fatalf("message count: orig=%d transformed=%d", len(orig.Messages), len(roundTripped.Messages))
			}
			for i := range orig.Messages {
				if orig.Messages[i].Content != roundTripped.Messages[i].Content ||
					orig.Messages[i].Role != roundTripped.Messages[i].Role {
					t.Errorf("message %d differs:\n orig=%+v\n new=%+v", i, orig.Messages[i], roundTripped.Messages[i])
				}
			}

			if len(orig.ToolCalls) != len(roundTripped.ToolCalls) {
				t.Fatalf("tool call count: orig=%d transformed=%d", len(orig.ToolCalls), len(roundTripped.ToolCalls))
			}
			for i := range orig.ToolCalls {
				o, n := orig.ToolCalls[i], roundTripped.ToolCalls[i]
				// The transformed parse carries the input/result by reference; the sha,
				// bytes, and media must match the inline parse's body exactly.
				if HashString(o.InputJSON) != refOrHash(n.InputSHA256, n.InputJSON) {
					t.Errorf("call %d input sha mismatch: inline=%s transformed=%s", i, HashString(o.InputJSON), refOrHash(n.InputSHA256, n.InputJSON))
				}
				if n.InputSHA256 != "" && n.InputBytes != len(o.InputJSON) {
					t.Errorf("call %d input bytes = %d, want %d", i, n.InputBytes, len(o.InputJSON))
				}
				if o.ResultBody != "" {
					if n.ResultSHA256 != HashString(o.ResultBody) {
						t.Errorf("call %d result sha mismatch: inline=%s transformed=%s", i, HashString(o.ResultBody), n.ResultSHA256)
					}
					if n.ResultBytes != len(o.ResultBody) || n.ResultMediaType != o.ResultMediaType {
						t.Errorf("call %d result meta: bytes=%d media=%q, want %d/%q", i, n.ResultBytes, n.ResultMediaType, len(o.ResultBody), o.ResultMediaType)
					}
				}
				if o.ResultStatus != n.ResultStatus {
					t.Errorf("call %d result status = %q, want %q", i, n.ResultStatus, o.ResultStatus)
				}
				// Lifting the input must not lose the file path the reducer projects
				// onto the call; the sentinel carries it in the input's place.
				if o.FilePath != n.FilePath {
					t.Errorf("call %d file path = %q, want %q", i, n.FilePath, o.FilePath)
				}
				// The detail is derived the same way: the inline parse derives it from
				// the raw input, and the sentinel carries it once the body is lifted, so
				// the two must agree exactly.
				if o.Detail != n.Detail {
					t.Errorf("call %d detail = %q, want %q", i, n.Detail, o.Detail)
				}
			}

			if len(orig.UsageEvent) != len(roundTripped.UsageEvent) {
				t.Errorf("usage count: orig=%d transformed=%d", len(orig.UsageEvent), len(roundTripped.UsageEvent))
			}
		})
	}
}

// TestSentinelFilePathInputsOnly pins the sentinel file_path rule: a JSON tool
// input's top-level file_path rides on its sentinel, while a result body never
// gets one even when its content is a JSON object carrying that key (results are
// displayed, not projected onto the call), and a non-JSON input (a Codex
// custom_tool_call patch) never gets one either.
func TestSentinelFilePathInputsOnly(t *testing.T) {
	input := `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.go"}}]}}` + "\n"
	result := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":{"file_path":"decoy.go","ok":true}}]}}` + "\n"
	patch := `{"type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","call_id":"c1","input":"*** patch with file_path text"}}` + "\n"

	rewritten, _ := RewriteLine(AgentClaude, []byte(input), idEncoder{})
	if !strings.Contains(string(rewritten), `"file_path":"a.go"`) {
		t.Errorf("input sentinel lost the file_path: %s", rewritten)
	}
	rewritten, _ = RewriteLine(AgentClaude, []byte(result), idEncoder{})
	if strings.Contains(string(rewritten), "decoy.go") {
		t.Errorf("result sentinel must not carry a file_path: %s", rewritten)
	}
	rewritten, _ = RewriteLine(AgentCodex, []byte(patch), idEncoder{})
	if strings.Contains(string(rewritten), `"file_path"`) {
		t.Errorf("non-JSON input sentinel must not carry a file_path: %s", rewritten)
	}
}

// TestSentinelDetail pins the sentinel detail rule: a JSON tool input's first
// candidate key (in the command, pattern, url, query, description, skill priority
// order) rides on its sentinel, an over-cap command falls back to the description,
// and the field is denied to everything the rule excludes (a result body, a
// non-JSON input, a non-string command). It exercises the buffered RewriteLine
// path; the streaming twin's parity is covered by the locate tests.
func TestSentinelDetail(t *testing.T) {
	overCap := strings.Repeat("x", maxSentinelDetail+1)
	cases := []struct {
		name    string
		agent   Agent
		line    string
		want    string // exact detail substring expected in the sentinel
		wantNo  bool   // when true, assert no "detail" field appears at all
		absentS string // a value that must NOT appear (a skipped/lower-priority candidate)
	}{
		{
			name:  "bash command",
			agent: AgentClaude,
			line:  `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./...","description":"run the tests"}}]}}`,
			want:  `"detail":"go test ./..."`,
		},
		{
			name:    "over-cap command falls back to description",
			agent:   AgentClaude,
			line:    `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"` + overCap + `","description":"a heredoc"}}]}}`,
			want:    `"detail":"a heredoc"`,
			absentS: overCap,
		},
		{
			name:  "grep pattern",
			agent: AgentClaude,
			line:  `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Grep","input":{"pattern":"func Reduce"}}]}}`,
			want:  `"detail":"func Reduce"`,
		},
		{
			name:  "webfetch url",
			agent: AgentClaude,
			line:  `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"WebFetch","input":{"url":"https://ada.example/spec"}}]}}`,
			want:  `"detail":"https://ada.example/spec"`,
		},
		{
			name:  "agent description",
			agent: AgentClaude,
			line:  `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"description":"explore the store layer"}}]}}`,
			want:  `"detail":"explore the store layer"`,
		},
		{
			name:   "tool result never gets a detail",
			agent:  AgentClaude,
			line:   `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":{"command":"rm -rf /","ok":true}}]}}`,
			wantNo: true,
		},
		{
			name:   "non-JSON custom_tool_call input gets none",
			agent:  AgentCodex,
			line:   `{"type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","call_id":"c1","input":"*** a patch with a command: word"}}`,
			wantNo: true,
		},
		{
			name:   "non-string command (Codex shell array) is skipped",
			agent:  AgentCodex,
			line:   `{"type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c1","arguments":"{\"command\":[\"bash\",\"-lc\",\"ls\"]}"}}`,
			wantNo: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rewritten, _ := RewriteLine(c.agent, []byte(c.line+"\n"), idEncoder{})
			got := string(rewritten)
			if c.wantNo {
				if strings.Contains(got, `"detail"`) {
					t.Errorf("expected no detail, got: %s", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("detail %q not in sentinel: %s", c.want, got)
			}
			if c.absentS != "" && strings.Contains(got, c.absentS) {
				t.Errorf("skipped candidate %q leaked into sentinel: %s", c.absentS, got)
			}
		})
	}
}

// refOrHash returns the recorded input sha when present, else the hash of the
// inline input, so the parity check works whether the transformed parse used a
// reference or (defensively) fell back to inline.
func refOrHash(sha, inline string) string {
	if sha != "" {
		return sha
	}
	return HashString(inline)
}

// claudeImageTranscript is a Claude session whose tool result is a base64-encoded
// image, the large-body case the client-CAS protocol exists to handle. The result
// arrives as an array of typed blocks; the reducer flattens it to text, so the
// extractor must lift exactly that flattened body.
func claudeImageTranscript() []byte {
	img := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x89PNG fake image bytes ", 2000)))
	var b strings.Builder
	b.WriteString(`{"type":"user","message":{"content":"screenshot this"}}` + "\n")
	b.WriteString(`{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"t1","name":"Screenshot","input":{"region":"full"}}]}}` + "\n")
	// A tool result delivered as a text block carrying the base64 payload.
	b.WriteString(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":` + jsonQuote(img) + `}],"is_error":false}]}}` + "\n")
	return []byte(b.String())
}

// fakePNGBase64 returns the base64 of a byte string that begins with the real PNG
// signature, so the media sniffer recognizes it as image/png, padded out to a size
// worth lifting. It is synthetic (not a decodable image), which is all the parser
// needs: it keys and stores the bytes, it never renders them.
func fakePNGBase64() string {
	return base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("kitten pixels ", 300)))
}

// codexImageTranscript is a Codex session that inlines images two ways: a user turn
// that pastes one as a data-URI image_url block, and an assistant turn that emits a
// generated image as an image_generation_end event carrying the base64 result. The
// image_generation_end shape is what made the 16 real sessions exceed the message cap;
// both must be lifted to binary attachments rather than left as base64 inline. The two
// images are distinct so the parity oracle sees two separate lifted bodies.
func codexImageTranscript() []byte {
	pasted := dataURIImage(fakeJPEGBase64())
	generated := fakePNGBase64()
	var b strings.Builder
	b.WriteString(`{"type":"session_meta","payload":{"cwd":"/home/ada/proj","git":{"branch":"main"}}}` + "\n")
	b.WriteString(`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"trace this"},{"type":"input_image","image_url":` + jsonQuote(pasted) + `}]}}` + "\n")
	b.WriteString(`{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":"here you go"}]}}` + "\n")
	b.WriteString(`{"type":"event_msg","timestamp":"2026-06-05T23:37:09Z","payload":{"type":"image_generation_end","result":` + jsonQuote(generated) + `,"saved_path":"/home/ada/kitten.png"}}` + "\n")
	return []byte(b.String())
}

// fakeJPEGBase64 returns base64 that begins with the JPEG SOI marker so the sniffer
// reads it as image/jpeg, distinct from the PNG used for the generated image.
func fakeJPEGBase64() string {
	return base64.StdEncoding.EncodeToString([]byte("\xff\xd8\xff\xe0" + strings.Repeat("jpeg pixels ", 200)))
}

// dataURIImage wraps raw base64 in the data:<media>;base64, envelope Codex uses for a
// pasted image_url, so the extractor and reducer exercise the data-URI strip path.
func dataURIImage(b64 string) string {
	return "data:image/jpeg;base64," + b64
}

// TestCodexImageAttachmentLifted is the headline attachment case: a Codex
// image_generation_end payload is lifted to a binary attachment on both the client
// extractor and the server reducer, with matching sha, decoded size, and image media,
// and the inline-decoded bytes equal the base64-decoded source. The filename is
// recovered from saved_path so the UI can label it.
func TestCodexImageAttachmentLifted(t *testing.T) {
	// A focused single-image transcript (just the image_generation_end overflow shape),
	// kept separate from the shared two-image fixture so the count assertions are exact.
	img := fakePNGBase64()
	raw := []byte(
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"draw a kitten"}]}}` + "\n" +
			`{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":"here you go"}]}}` + "\n" +
			`{"type":"event_msg","timestamp":"2026-06-05T23:37:09Z","payload":{"type":"image_generation_end","result":` + jsonQuote(img) + `,"saved_path":"/home/ada/kitten.png"}}` + "\n")

	wantBytes, err := base64.StdEncoding.DecodeString(fakePNGBase64())
	if err != nil {
		t.Fatalf("decode source image: %v", err)
	}
	wantSHA := HashString(string(wantBytes))

	// Client extractor: the image rides out as one attachment-kind body whose stored
	// bytes are the decoded image, keyed by its sha, replaced inline by a sentinel.
	transformed, bodies, err := ExtractBodies(AgentCodex, raw, idEncoder{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("lifted %d bodies, want 1", len(bodies))
	}
	b := bodies[0]
	if b.Kind != bodyKindAttachment {
		t.Errorf("body kind = %q, want %q", b.Kind, bodyKindAttachment)
	}
	if b.MediaType != "image/png" {
		t.Errorf("media = %q, want image/png", b.MediaType)
	}
	if b.SHA256 != wantSHA || b.Bytes != len(wantBytes) || string(b.Stored) != string(wantBytes) {
		t.Errorf("lifted body does not match decoded image: sha=%s bytes=%d", b.SHA256, b.Bytes)
	}
	if strings.Contains(string(transformed), fakePNGBase64()) {
		t.Error("transformed transcript still contains the inline base64 image")
	}
	if !strings.Contains(string(transformed), `"`+sentinelKey+`":1`) {
		t.Error("transformed transcript carries no CAS sentinel")
	}

	// Server reducer, inline path: the original transcript decodes the image to bytes
	// and records one attachment carrying them, on the assistant turn, named from
	// saved_path.
	inline, err := Parse(AgentCodex, raw)
	if err != nil {
		t.Fatalf("parse inline: %v", err)
	}
	if len(inline.Attachments) != 1 {
		t.Fatalf("inline parse recorded %d attachments, want 1", len(inline.Attachments))
	}
	a := inline.Attachments[0]
	// The inline path carries the decoded bytes and leaves SHA256 empty: the server keys
	// the blob it writes (the same discriminator the inline tool-body path uses), so the
	// stored key is the hash of the content.
	if a.SHA256 != "" {
		t.Errorf("inline attachment SHA256 = %q, want empty (server keys the written blob)", a.SHA256)
	}
	if HashString(a.Content) != wantSHA || a.Bytes != len(wantBytes) || a.MediaType != "image/png" {
		t.Errorf("inline attachment meta mismatch: contentSHA=%s bytes=%d media=%s", HashString(a.Content), a.Bytes, a.MediaType)
	}
	if a.Content != string(wantBytes) {
		t.Error("inline attachment content is not the decoded image bytes")
	}
	if a.Filename != "kitten.png" {
		t.Errorf("filename = %q, want kitten.png", a.Filename)
	}
	// The image hangs on the assistant turn (ordinal 1: user is 0).
	if a.MessageOrdinal != 1 {
		t.Errorf("attachment ordinal = %d, want 1 (the assistant turn)", a.MessageOrdinal)
	}

	// Server reducer, reference path: parsing the transformed transcript records the
	// same attachment by reference, with the bytes left in the CAS (Content empty) and
	// identical sha, size, and media. This is the lift/record lockstep that keeps a
	// lifted image from being swept as unreferenced.
	ref, err := Parse(AgentCodex, transformed)
	if err != nil {
		t.Fatalf("parse transformed: %v", err)
	}
	if len(ref.Attachments) != 1 {
		t.Fatalf("ref parse recorded %d attachments, want 1", len(ref.Attachments))
	}
	ra := ref.Attachments[0]
	if ra.SHA256 != wantSHA || ra.Bytes != len(wantBytes) || ra.MediaType != "image/png" {
		t.Errorf("ref attachment meta mismatch: sha=%s bytes=%d media=%s", ra.SHA256, ra.Bytes, ra.MediaType)
	}
	if ra.Content != "" {
		t.Error("ref attachment carries inline content; the client already stored the bytes")
	}
	if ra.Filename != "kitten.png" || ra.MessageOrdinal != 1 {
		t.Errorf("ref attachment filename/ordinal mismatch: %q / %d", ra.Filename, ra.MessageOrdinal)
	}
}

// jsonQuote returns a JSON string literal for s, used to embed a large payload in
// a fixture line.
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
