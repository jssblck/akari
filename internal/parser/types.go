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

// ToolCall is one tool invocation attached to a message. InputJSON and
// ResultText carry the bulky bodies; the store keeps only their size and media
// type until the CAS milestone lands. ResultBytes and ResultMediaType describe
// the original result body, not the flattened display text, so the recorded size
// is faithful to what the CAS will eventually hold.
type ToolCall struct {
	MessageOrdinal  int
	CallIndex       int
	ToolName        string
	Category        string
	FilePath        string
	InputJSON       string
	ResultText      string
	ResultBytes     int
	ResultMediaType string
	ResultStatus    string // "ok" | "error" | "" (pending)
}

// Usage is one token-accounting record. MessageOrdinal is nil for session-level
// totals not tied to a single message.
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
}
