package parser

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"time"
)

// maxLine bounds a single JSONL line. Sessions occasionally embed large tool
// outputs, so the limit is generous.
const maxLine = 64 << 20

// Parse dispatches to the per-agent parser. Unparseable individual lines are
// skipped so a malformed line cannot lose the rest of a session; a returned
// error means the input could not be processed at all.
func Parse(agent Agent, raw []byte) (Session, error) {
	switch agent {
	case AgentClaude:
		return parseClaude(raw)
	case AgentCodex:
		return parseCodex(raw)
	case AgentPi:
		return parsePi(raw)
	default:
		return Session{}, fmt.Errorf("unknown agent %q", agent)
	}
}

// scanLines returns the non-empty lines of raw, tolerating very long lines. A
// scan error (a line over maxLine, or a read failure) is returned rather than
// swallowed: a parser that silently dropped the tail would let WriteProjection
// replace a complete projection with a truncated one.
func scanLines(raw []byte) ([]string, error) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	var out []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan session lines: %w", err)
	}
	return out, nil
}

// parseTime parses the timestamp formats these agents emit (RFC3339, with or
// without sub-second precision). A blank or unparseable value yields the zero
// time.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// span tracks the earliest and latest timestamps seen across a session.
type span struct {
	started time.Time
	ended   time.Time
}

func (s *span) observe(t time.Time) {
	if t.IsZero() {
		return
	}
	if s.started.IsZero() || t.Before(s.started) {
		s.started = t
	}
	if t.After(s.ended) {
		s.ended = t
	}
}

// toolCategory maps a raw tool name to a coarse category used for filtering and
// stats. Unknown tools get an empty category rather than a guess.
func toolCategory(name string) string {
	switch strings.ToLower(name) {
	case "read", "readfile", "view", "cat":
		return "read"
	case "write", "writefile", "create":
		return "write"
	case "edit", "apply_patch", "applypatch", "str_replace", "multiedit":
		return "edit"
	case "bash", "shell", "shell_command", "exec_command", "run", "execute":
		return "bash"
	case "glob", "grep", "search", "find", "rg":
		return "search"
	default:
		return ""
	}
}
