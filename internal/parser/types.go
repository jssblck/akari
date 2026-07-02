// Package parser turns the raw bytes of an agent session file into akari's
// normalized projection: ordered messages, tool calls, and token usage. It runs
// on the server, so all per-agent format knowledge lives here in one place and
// can be improved and re-run against stored raw bytes.
package parser

import "time"

// Agent identifies which on-disk format a session uses.
type Agent string

const (
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
	AgentPi     Agent = "pi"
)

// Session is the parsed projection of one session file.
type Session struct {
	Cwd         string
	GitBranch   string
	StartedAt   time.Time
	EndedAt     time.Time
	Messages    []Message
	ToolCalls   []ToolCall
	UsageEvent  []Usage
	Attachments []Attachment
	Fallbacks   []FallbackOp
}

// Attachment is one binary blob attached to a message: today a lifted image (a Codex
// image-generation result or a pasted image). The bytes live in the CAS keyed by
// SHA256; Bytes is the raw (decoded) size and MediaType its semantic type, so the UI
// can render it without fetching. Content carries the decoded bytes only on the inline
// path (a small image with no sentinel, used by the batch/test parser); when the
// client lifted the image it is empty and SHA256 names the already-uploaded blob.
type Attachment struct {
	MessageOrdinal int
	SHA256         string
	Bytes          int
	MediaType      string
	Filename       string
	Content        string
}

// Role is a normalized message role.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
	// RoleContext marks an injected-context turn: agent framing (project
	// instructions, the environment block) that an agent prepends to a session
	// rather than a human prompt. It is a distinct role so every role='user'
	// reader (the session title, user_message_count, the prompt-hygiene
	// aggregate) excludes it, and the transcript renders it in its own Context
	// section instead of as the opening turn. Only Codex records such framing as a
	// transcript message today (Claude and pi keep their framing in the system
	// prompt, which akari never ingests); the role is agent-agnostic so any
	// reducer can classify into it.
	RoleContext Role = "context"
)

// Message is one turn. Content holds the conversational text (stored inline and
// searchable); ThinkingText holds concatenated reasoning blocks.
type Message struct {
	Ordinal      int
	Role         Role
	Content      string
	ThinkingText string
	Model        string
	Timestamp    time.Time
	HasThinking  bool
	HasToolUse   bool
}

// ToolCall is one tool invocation attached to a message. InputJSON and ResultBody
// carry the bulky bodies the CAS stores; ResultBytes and ResultMediaType describe
// ResultBody exactly, so the recorded size and media type always match the stored
// content. InputJSON is the raw tool-input JSON; ResultBody is the result body
// (a tool result that is an array of text blocks is flattened to its text).
//
// CallUID is the agent's own call id; the incremental pipeline records it on the
// row so a tool result arriving in a later line (Claude delivers tool_result in
// the following user entry, which may land in a later region) is back-patched by
// an UPDATE keyed on it rather than by a parser-held id->row map. It is carried
// on inserts only; a Session assembled for tests ignores it.
// InputSHA256, InputBytes, and InputMediaType carry a CAS reference when the
// client lifted the input body to the CAS and left a sentinel in the transcript:
// the server records the reference instead of re-storing the body. InputJSON is
// empty in that case. When the body travels inline, InputJSON holds it and the
// SHA/bytes/media fields are unset (the server hashes and sizes the inline body).
//
// Detail is a bounded, human-scannable summary of the input the UI shows when a
// call has no file_path: a shell command, a search pattern, a fetched URL, or an
// agent's description, derived from the input's top-level JSON keys. On the inline
// path it is derived here from the raw input; on the sentinel path the body is no
// longer readable, so it rides the sentinel and comes back through the casRef,
// exactly the way FilePath does.
type ToolCall struct {
	MessageOrdinal  int
	CallIndex       int
	ToolName        string
	Category        string
	FilePath        string
	Detail          string
	InputJSON       string
	InputSHA256     string
	InputBytes      int
	InputMediaType  string
	ResultBody      string
	ResultSHA256    string
	ResultBytes     int
	ResultMediaType string
	ResultStatus    string // "ok" | "error" | "" (pending)
	CallUID         string
}

// Usage is one token-accounting record. MessageOrdinal is nil for session-level
// totals not tied to a single message. SourceOffset and SourceIndex identify the
// originating line (and its position within it) so incremental inserts are
// idempotent even for agents whose usage carries no native dedup key.
type Usage struct {
	MessageOrdinal *int
	Model          string
	Input          int
	Output         int
	CacheWrite     int
	CacheRead      int
	Reasoning      int
	OccurredAt     time.Time
	DedupKey       string
	SourceOffset   int64
	SourceIndex    int
}
