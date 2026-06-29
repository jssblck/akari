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
			}

			if len(orig.UsageEvent) != len(roundTripped.UsageEvent) {
				t.Errorf("usage count: orig=%d transformed=%d", len(orig.UsageEvent), len(roundTripped.UsageEvent))
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
