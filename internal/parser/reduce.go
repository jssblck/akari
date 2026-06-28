package parser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// State is the carry-over a parser needs to resume from a byte cursor. It is
// serialized to the session_raw row between chunks and is bounded in size: it
// holds only counters, never any per-message accumulation or an open turn. A
// Codex turn folds a run of reasoning/function_call items and the final text
// into one assistant message, but that fold never crosses a region: the ingest
// protocol cuts chunks on turn boundaries, so a whole turn always lands in one
// region. That is what lets the open turn live in the reducer for the span of a
// single Reduce call and never be serialized here.
type State struct {
	// NextOrdinal is the ordinal the next message will take.
	NextOrdinal int `json:"next_ordinal"`
	// Model is the sticky current model (Codex carries it across lines, and a
	// later region's usage line may need the model named in an earlier region).
	Model string `json:"model"`
}

// initialState is the state a fresh (or freshly reset) session starts from.
func initialState() State {
	return State{}
}

// DecodeState parses serialized parser state, defaulting unset fields to the
// initial state. An empty or "{}" blob yields the initial state.
func DecodeState(b []byte) (State, error) {
	st := initialState()
	if len(b) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("decode parser state: %w", err)
	}
	return st, nil
}

// Encode serializes the state for storage.
func (s State) Encode() ([]byte, error) { return json.Marshal(s) }

// MessageOp is one message write in a Delta. Each ordinal is written exactly
// once: a turn is folded whole within the region that contains it (the ingest
// protocol keeps a turn inside one chunk), so Content and ThinkingText are the
// complete text, not a fragment to append.
type MessageOp struct {
	Ordinal      int
	Role         Role
	Content      string
	ThinkingText string
	Model        string
	HasThinking  bool
	HasToolUse   bool
	Timestamp    time.Time
}

// ToolResultOp back-patches a tool call's result, matched by the agent's call id.
type ToolResultOp struct {
	CallUID   string
	Body      string
	Bytes     int
	MediaType string
	Status    string
}

// Delta is everything one Reduce call produces for one raw region: rows to write
// and the increments to fold into the session aggregates. It carries operations,
// not a whole session, so applying it is append-only work proportional to the
// region, never to the session.
type Delta struct {
	Messages    []MessageOp
	ToolCalls   []ToolCall
	ToolResults []ToolResultOp
	Usage       []Usage

	MessagesAdded     int
	UserMessagesAdded int

	Started time.Time
	Ended   time.Time

	// Cwd and GitBranch are the last values seen in the region. The store ignores
	// them (announce owns those columns); the test-facing Parse wrapper uses them.
	Cwd       string
	GitBranch string
}

// Reduce advances the parse of one agent over a raw region that begins at
// baseOffset. The region must contain only complete lines (the ingest protocol
// guarantees every stored byte ends on a newline). It returns the new carry-over
// state and the projection delta. Malformed individual lines are skipped, exactly
// as the batch parser did; an error means the region could not be processed.
func Reduce(agent Agent, st State, region []byte, baseOffset int64) (State, Delta, error) {
	r := &reducer{st: st, lastUsageOffset: -1}
	var err error
	switch agent {
	case AgentClaude:
		err = r.reduceClaude(region, baseOffset)
	case AgentCodex:
		err = r.reduceCodex(region, baseOffset)
	case AgentPi:
		err = r.reducePi(region, baseOffset)
	default:
		return st, Delta{}, fmt.Errorf("unknown agent %q", agent)
	}
	if err != nil {
		return st, Delta{}, err
	}
	return r.st, r.d, nil
}

// reducer accumulates a Delta for one Reduce call. open is the assistant turn
// being folded across the lines of this region (Codex); claude and pi never use
// it. It lives only for the span of one Reduce call: a turn never crosses a
// region, so there is nothing to carry into the next one. openContent and
// openThink collect that turn's fragments so they are joined once when the op is
// emitted, rather than rebuilt with a growing concatenation on every line (which
// would make one region O(region_text^2)). openCalls is the next call index
// within the open turn.
type reducer struct {
	st          State
	d           Delta
	open        *MessageOp
	openCalls   int
	openContent []string
	openThink   []string

	lastUsageOffset int64
	lastUsageIndex  int
}

// observe widens the region's timestamp span.
func (r *reducer) observe(t time.Time) {
	if t.IsZero() {
		return
	}
	if r.d.Started.IsZero() || t.Before(r.d.Started) {
		r.d.Started = t
	}
	if t.After(r.d.Ended) {
		r.d.Ended = t
	}
}

// addUser appends a user message and advances the ordinal.
func (r *reducer) addUser(content string, ts time.Time) {
	ord := r.st.NextOrdinal
	r.st.NextOrdinal++
	r.d.MessagesAdded++
	r.d.UserMessagesAdded++
	r.d.Messages = append(r.d.Messages, MessageOp{
		Ordinal: ord, Role: RoleUser, Content: content, Timestamp: ts,
	})
}

// addUsage records a usage event tagged with a stable per-line source identity.
func (r *reducer) addUsage(u Usage, offset int64) {
	if offset == r.lastUsageOffset {
		r.lastUsageIndex++
	} else {
		r.lastUsageOffset = offset
		r.lastUsageIndex = 0
	}
	u.SourceOffset = offset
	u.SourceIndex = r.lastUsageIndex
	r.d.Usage = append(r.d.Usage, u)
}

// ensureAssistant returns the ordinal of the open assistant turn, opening one if
// none is. The open turn lives only within this region; the protocol guarantees
// the whole turn is here, so it is folded and emitted before the region ends.
func (r *reducer) ensureAssistant(ts time.Time) int {
	if r.open != nil {
		return r.open.Ordinal
	}
	ord := r.st.NextOrdinal
	r.st.NextOrdinal++
	r.openCalls = 0
	r.d.MessagesAdded++
	r.open = &MessageOp{Ordinal: ord, Role: RoleAssistant, Model: r.st.Model, Timestamp: ts}
	return ord
}

// addOpenContent and addOpenThinking collect a fragment of the open turn; they
// are joined once when the op is emitted.
func (r *reducer) addOpenContent(s string) {
	if s != "" {
		r.openContent = append(r.openContent, s)
	}
}

func (r *reducer) addOpenThinking(s string) {
	if s != "" {
		r.openThink = append(r.openThink, s)
		if r.open != nil {
			r.open.HasThinking = true
		}
	}
}

// buildOpen joins the open turn's collected fragments into its op and resets the
// fragment buffers for the next turn.
func (r *reducer) buildOpen() {
	r.open.Content = strings.Join(r.openContent, "\n")
	r.open.ThinkingText = strings.Join(r.openThink, "\n")
	r.openContent, r.openThink = nil, nil
}

// closeTurn finalizes and emits the open assistant turn (a user line ends it).
func (r *reducer) closeTurn() {
	if r.open == nil {
		return
	}
	r.buildOpen()
	r.d.Messages = append(r.d.Messages, *r.open)
	r.open = nil
}

// flushRegion emits a still-open turn at the end of a region. Under the protocol
// this only fires for the final, in-progress turn of a settled session (its
// closing user line never arrives); it is emitted as a complete message.
func (r *reducer) flushRegion() {
	r.closeTurn()
}

// eachLine walks the complete JSONL lines in region, calling fn with the trimmed
// line and the raw byte offset of its start. Blank lines are skipped but still
// advance the offset.
func eachLine(region []byte, base int64, fn func(line []byte, offset int64) error) error {
	start := 0
	for i := 0; i < len(region); i++ {
		if region[i] != '\n' {
			continue
		}
		if line := bytes.TrimSpace(region[start:i]); len(line) > 0 {
			if err := fn(line, base+int64(start)); err != nil {
				return err
			}
		}
		start = i + 1
	}
	// A line-aligned region ends exactly on a newline, so there is normally no
	// trailing fragment; tolerate one defensively.
	if start < len(region) {
		if line := bytes.TrimSpace(region[start:]); len(line) > 0 {
			if err := fn(line, base+int64(start)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Parse parses a whole session in one shot, assembling a Session from the same
// reducer the incremental path uses. Keeping the batch parser as a thin wrapper
// over Reduce guarantees full and incremental parsing can never diverge.
func Parse(agent Agent, raw []byte) (Session, error) {
	_, d, err := Reduce(agent, initialState(), raw, 0)
	if err != nil {
		return Session{}, err
	}
	return assemble(d), nil
}

// assemble folds a single-region Delta back into a Session for the test-facing
// batch API: messages keyed by ordinal with appends joined, tool results matched
// to their calls by id.
func assemble(d Delta) Session {
	s := Session{Cwd: d.Cwd, GitBranch: d.GitBranch, StartedAt: d.Started, EndedAt: d.Ended}

	byOrd := map[int]*Message{}
	var order []int
	for _, op := range d.Messages {
		m, ok := byOrd[op.Ordinal]
		if !ok {
			m = &Message{Ordinal: op.Ordinal, Role: op.Role, Timestamp: op.Timestamp}
			byOrd[op.Ordinal] = m
			order = append(order, op.Ordinal)
		}
		m.Content = joinNonEmpty(m.Content, op.Content)
		m.ThinkingText = joinNonEmpty(m.ThinkingText, op.ThinkingText)
		if op.Model != "" {
			m.Model = op.Model
		}
		if op.HasToolUse {
			m.HasToolUse = true
		}
	}
	sort.Ints(order)
	for _, ord := range order {
		m := byOrd[ord]
		m.HasThinking = m.ThinkingText != ""
		s.Messages = append(s.Messages, *m)
	}

	idxByUID := map[string]int{}
	for _, tc := range d.ToolCalls {
		s.ToolCalls = append(s.ToolCalls, tc)
		if tc.CallUID != "" {
			idxByUID[tc.CallUID] = len(s.ToolCalls) - 1
		}
	}
	for _, tr := range d.ToolResults {
		i, ok := idxByUID[tr.CallUID]
		if !ok {
			continue
		}
		tc := &s.ToolCalls[i]
		tc.ResultBody, tc.ResultBytes, tc.ResultMediaType, tc.ResultStatus = tr.Body, tr.Bytes, tr.MediaType, tr.Status
	}

	s.UsageEvent = d.Usage
	return s
}
