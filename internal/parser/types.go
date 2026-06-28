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
	Cwd        string
	GitBranch  string
	StartedAt  time.Time
	EndedAt    time.Time
	Messages   []Message
	ToolCalls  []ToolCall
	UsageEvent []Usage
}

// Role is a normalized message role.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
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
// row so a tool result arriving in a later line is back-patched by an UPDATE
// keyed on it rather than by a parser-held id->row map. SourceOffset is the raw
// byte offset of the line that introduced the call. Both are carried on inserts
// only; a Session assembled for tests ignores them.
type ToolCall struct {
	MessageOrdinal  int
	CallIndex       int
	ToolName        string
	Category        string
	FilePath        string
	InputJSON       string
	ResultBody      string
	ResultBytes     int
	ResultMediaType string
	ResultStatus    string // "ok" | "error" | "" (pending)
	CallUID         string
	SourceOffset    int64
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
