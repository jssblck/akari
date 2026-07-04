package parser

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// MessageOp is one message write in a Delta. Each ordinal is written exactly
// once, carrying the turn's complete text: the reducer folds a whole session in
// one pass, so a turn's fragments are joined before the op is emitted, never
// appended in place. ThinkingBytes is the turn's reasoning-trace weight (see
// Message.ThinkingBytes): plaintext length where the agent logs it, else the
// encrypted payload length, so a redacted turn still records how much it
// thought.
type MessageOp struct {
	Ordinal       int
	Role          Role
	Content       string
	ThinkingText  string
	ThinkingBytes int
	Model         string
	HasThinking   bool
	HasToolUse    bool
	Timestamp     time.Time
}

// ToolResultOp back-patches a tool call's result, matched by the agent's call id.
//
// A result body normally travels inline (Body holds it) and the server writes it
// to the CAS. When the client has already lifted the body to the CAS and left a
// sentinel in the transcript, BodySHA256 is set instead: the server records the
// reference and writes no blob, but Bytes and MediaType still describe the body
// exactly so the row's metadata is unchanged.
type ToolResultOp struct {
	CallUID    string
	Body       string
	BodySHA256 string
	Bytes      int
	MediaType  string
	Status     string
}

// AttachmentOp records one binary attachment (today a lifted image) against a
// message. Like a tool body it reaches the CAS by one of two paths: when the client
// lifted the image and left a sentinel, SHA256 is set and the server records the
// reference with no blob write; otherwise Content holds the decoded bytes inline for
// the server to store. Bytes and MediaType describe the decoded image either way, so
// the row's metadata is the same whichever path delivered the bytes.
type AttachmentOp struct {
	MessageOrdinal int
	SHA256         string
	Content        string
	Bytes          int
	MediaType      string
	Filename       string
}

// FallbackOp records that a Claude Fable turn was declined by the safety classifier
// and re-served by a lower model. It is emitted only from explicit Claude Code markers
// (a "fallback" content block, a usage.iterations "fallback_message" entry, or a
// "model_refusal_fallback" system entry): never from a bare model-string change, which
// is an intentional switch (a /model command, a resume under a different default, a
// subagent on a smaller model), not a fallback.
//
// One logical fallback surfaces across several JSONL lines that the store merges by
// DedupKey: Claude splits one API message into several assistant entries sharing the
// requestId (each repeating the same iterations), and a separate system entry carries
// the refusal category. The assistant side brings MessageOrdinal and the declined
// attempt's token counts; the system side brings Trigger, RefusalCategory, and
// RefusalExplanation. Either can be the first to arrive, so the store's merge fills a
// field from whichever line carries it. A field the source did not observe is left
// zero (MessageOrdinal nil, token counts zero, strings empty) so the merge can tell
// "unset" from a real value.
type FallbackOp struct {
	// MessageOrdinal ties an assistant-side op to the message op the same entry produced
	// (the same way Usage ties to its message). It is nil on a system-side op, which
	// produces no message row and must not disturb ordinals.
	MessageOrdinal *int
	FromModel      string
	ToModel        string
	// Trigger, RefusalCategory, and RefusalExplanation come only from the system entry.
	Trigger            string
	RefusalCategory    string
	RefusalExplanation string
	// Declined* are the token counts of the declined attempt (the type=="message"
	// iteration entries), meaningful only on an assistant-side op that saw a
	// fallback_message entry. Zero elsewhere.
	DeclinedInput      int
	DeclinedOutput     int
	DeclinedCacheWrite int
	DeclinedCacheRead  int
	// DeclinedObserved is true only when the declined spend was actually summed from
	// fallback_message iteration entries. An assistant entry that carries a fallback
	// content block but no usage.iterations is a real fallback whose declined counts
	// were never reported, so it leaves this false and the zero Declined* stay
	// "unmeasured" rather than reading as a measured zero-token attempt.
	DeclinedObserved bool
	OccurredAt       time.Time
	// DedupKey is the top-level requestId when present, else the assistant message id.
	// Every line of one logical fallback repeats it, so the store dedups and merges on it.
	DedupKey string
}

// Delta is everything one parse of a session produces: the rows to write and
// the session's timestamp span. It deliberately carries no message/token
// counters: the session rollups are derived downstream from the deduped set the
// rebuild actually writes, so a counter here would only risk drifting from it.
type Delta struct {
	Messages    []MessageOp
	ToolCalls   []ToolCall
	ToolResults []ToolResultOp
	Usage       []Usage
	Attachments []AttachmentOp
	Fallbacks   []FallbackOp

	Started time.Time
	Ended   time.Time

	// Cwd and GitBranch are the last values seen in the region. The store ignores
	// them (announce owns those columns); the test-facing Parse wrapper uses them.
	Cwd       string
	GitBranch string
}

// Reducer folds one session's raw bytes into a Delta. A parse always covers the
// whole session from byte zero: construct a Reducer, Feed the stored regions in
// offset order (each must contain only complete lines; the ingest protocol
// guarantees every stored byte ends on a newline), and Finish to flush the open
// turn and take the Delta. Because the whole parse is one Reducer, an open turn
// folds freely across region boundaries and no state is ever serialized.
// Malformed individual lines are skipped; a Feed error means the region could
// not be processed.
type Reducer struct {
	agent Agent
	r     reducer
	done  bool
}

// NewReducer returns a Reducer for one session of the given agent.
func NewReducer(agent Agent) (*Reducer, error) {
	switch agent {
	case AgentClaude, AgentCodex, AgentPi:
		return &Reducer{agent: agent, r: reducer{lastUsageOffset: -1}}, nil
	default:
		return nil, fmt.Errorf("unknown agent %q", agent)
	}
}

// Feed advances the parse over the next raw region, which begins at baseOffset.
func (x *Reducer) Feed(region []byte, baseOffset int64) error {
	if x.done {
		return fmt.Errorf("reducer already finished")
	}
	switch x.agent {
	case AgentClaude:
		return x.r.reduceClaude(region, baseOffset)
	case AgentCodex:
		return x.r.reduceCodex(region, baseOffset)
	default:
		return x.r.reducePi(region, baseOffset)
	}
}

// Finish emits any still-open turn (the final, in-progress turn of a live
// session has no closing line) and returns the session's Delta.
func (x *Reducer) Finish() Delta {
	if !x.done {
		x.r.closeTurn()
		x.done = true
	}
	return x.r.d
}

// reducer accumulates the Delta for one session. open is the assistant turn
// being folded across lines: Codex folds a run of reasoning/function_call items
// into one turn, and Claude folds the content-block lines that share one API
// message id. openContent and openThink collect that turn's fragments so they
// are joined once when the op is emitted, rather than rebuilt with a growing
// concatenation on every line (which would make one turn O(turn_text^2)).
// openCalls is the next call index within the open turn.
type reducer struct {
	// nextOrdinal is the ordinal the next message will take. model is the sticky
	// current model (Codex carries it across lines).
	nextOrdinal int
	model       string

	d         Delta
	open      *MessageOp
	openCalls int
	// openClaudeID is the API message id of the open Claude turn, the fold key
	// that groups Claude's split content-block lines (issue #98). Empty for other
	// agents and for an id-less line, which never folds with a neighbor.
	openClaudeID string
	openContent  []string
	openThink    []string
	// openThinkBytes accumulates the open turn's reasoning-trace weight (plaintext
	// where present, else the encrypted payload length), and openThinkSeen records
	// that a reasoning block appeared at all, so a turn whose reasoning was redacted
	// to empty text still marks HasThinking and carries its byte weight.
	openThinkBytes int
	openThinkSeen  bool

	lastUsageOffset int64
	lastUsageIndex  int

	// seenAttach dedups attachments by their content key within this region so the
	// same image carried by more than one event (a Codex image_generation_call and
	// the image_generation_end that mirrors it, or a user_message event echoing a
	// message's pasted image) is recorded once. A whole turn lands in one region, so
	// the duplicates always fall in the same Reduce call and this catches them.
	seenAttach map[string]bool
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

// addUser appends a user message and advances the ordinal, returning the ordinal it
// took so a caller can attach images carried by the same line to it. It closes any
// open assistant turn first: a user message always ends the turn before it.
func (r *reducer) addUser(content string, ts time.Time) int {
	r.closeTurn()
	ord := r.nextOrdinal
	r.nextOrdinal++
	r.d.Messages = append(r.d.Messages, MessageOp{
		Ordinal: ord, Role: RoleUser, Content: content, Timestamp: ts,
	})
	return ord
}

// addContext appends an injected-context message (RoleContext): agent framing that
// is not a human prompt. It advances the ordinal like any turn and returns it, but
// takes RoleContext so the store's role='user' readers (count, hygiene, title) all
// skip it. A context turn carries no images or usage, so unlike addUser its callers
// have no attachment to hang on the returned ordinal.
func (r *reducer) addContext(content string, ts time.Time) int {
	r.closeTurn()
	ord := r.nextOrdinal
	r.nextOrdinal++
	r.d.Messages = append(r.d.Messages, MessageOp{
		Ordinal: ord, Role: RoleContext, Content: content, Timestamp: ts,
	})
	return ord
}

// addAttachment records one binary attachment against a message. The common path is a
// CAS sentinel: the client lifts every image to the CAS (both the small-line rewrite
// and the streaming big-line path lift images), so the server records a reference and
// never holds the bytes. The fallback decodes an inline base64 image to its binary form
// for the server to store. It dedups by content key within the region, so the same
// image echoed by a second event is recorded once. A value that is neither a sentinel
// nor a decodable base64 image is ignored, mirroring the extractor's gate.
//
// The inline decode is memory-bounded by a fixed window, not by session size. A large
// image is always lifted to a sentinel by the client (a large line is the big-line
// streaming path's job, and an image there is located and lifted, not inlined), so the
// decode branch only runs for an image that arrived inline: one small enough to have
// ridden under the client's 1 MiB big-line threshold, a legacy transcript from a client
// that predates image lifting, or a small test fixture. Once a session is reparsed under
// this version its images are sentinels, so the inline buffer never tracks input size.
// This mirrors how the inline tool-body path carries InputJSON and ResultBody in the
// delta, and the parser is CAS-free by design (it cannot stream into the store from
// here), so the bounded buffer is the right shape rather than a streamed write.
func (r *reducer) addAttachment(ord int, v gjson.Result, filename string) {
	op := AttachmentOp{MessageOrdinal: ord, Filename: filename}
	var key string
	if ref, ok := asCASRef(v); ok {
		op.SHA256, op.Bytes, op.MediaType = ref.SHA256, ref.Bytes, ref.MediaType
		key = ref.SHA256
	} else {
		if v.Type != gjson.String {
			return
		}
		s := v.String()
		if !looksLikeBase64Image(imageHead(s)) {
			return
		}
		decoded, ok := decodeBase64Body(s)
		if !ok {
			return
		}
		op.Content = string(decoded)
		op.Bytes = len(decoded)
		op.MediaType = imageMediaType(imageHead(s))
		// The inline key is the hash of the raw decoded bytes, which equals the
		// sentinel key whenever the encoder stores the body verbatim (the small-image
		// path the batch/test parser takes), so dedup matches across the two paths.
		key = HashString(op.Content)
	}
	if key != "" {
		if r.seenAttach == nil {
			r.seenAttach = map[string]bool{}
		}
		if r.seenAttach[key] {
			return
		}
		r.seenAttach[key] = true
	}
	r.d.Attachments = append(r.d.Attachments, op)
}

// attachOrdinal returns the message ordinal an attachment lifted from a non-message
// event should hang on: the open assistant turn while one is folding (an image
// generated mid-turn), else the most recent message (a user_message event mirroring
// the user line just recorded). With nothing recorded yet it opens an assistant turn
// so the image still has a home.
func (r *reducer) attachOrdinal(ts time.Time) int {
	if r.open != nil {
		return r.open.Ordinal
	}
	if r.nextOrdinal > 0 {
		return r.nextOrdinal - 1
	}
	return r.ensureAssistant(ts)
}

// lastPathSegment returns the final path component of a file path, splitting on either
// separator so a Windows saved_path yields a clean filename on the Linux server. An
// empty path yields an empty name.
func lastPathSegment(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
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
// none is. The open turn lives in memory until something closes it (a user line,
// a new Claude message id, or Finish), so a fold crosses region boundaries freely.
func (r *reducer) ensureAssistant(ts time.Time) int {
	if r.open != nil {
		return r.open.Ordinal
	}
	ord := r.nextOrdinal
	r.nextOrdinal++
	r.openCalls = 0
	r.open = &MessageOp{Ordinal: ord, Role: RoleAssistant, Model: r.model, Timestamp: ts}
	return ord
}

// addOpenContent collects a fragment of the open turn's visible text; it is joined
// once when the op is emitted.
func (r *reducer) addOpenContent(s string) {
	if s != "" {
		r.openContent = append(r.openContent, s)
	}
}

// addOpenReasoning records one reasoning block on the open turn: its plaintext (kept
// for search and display, empty when the agent redacted it) and its weight, the byte
// size that stands in for the reasoning volume (the plaintext length where the agent
// logs it, else the encrypted payload length). Seeing a reasoning block at all marks
// the turn as having thought, so a redacted block with empty text and a nonzero
// weight still reads as thinking.
func (r *reducer) addOpenReasoning(text string, weight int) {
	r.openThinkSeen = true
	if r.open != nil {
		r.open.HasThinking = true
	}
	if text != "" {
		r.openThink = append(r.openThink, text)
	}
	r.openThinkBytes += weight
}

// buildOpen joins the open turn's collected fragments into its op and resets the
// fragment buffers for the next turn.
func (r *reducer) buildOpen() {
	r.open.Content = strings.Join(r.openContent, "\n")
	r.open.ThinkingText = strings.Join(r.openThink, "\n")
	r.open.ThinkingBytes = r.openThinkBytes
	if r.openThinkSeen {
		r.open.HasThinking = true
	}
	r.openContent, r.openThink = nil, nil
	r.openThinkBytes, r.openThinkSeen = 0, false
}

// closeTurn finalizes and emits the open assistant turn (a user line, a new
// Claude message id, or Finish ends it).
func (r *reducer) closeTurn() {
	if r.open == nil {
		return
	}
	r.buildOpen()
	r.d.Messages = append(r.d.Messages, *r.open)
	r.open = nil
	r.openClaudeID = ""
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
// reducer the server's rebuild uses. Keeping the batch parser as a thin wrapper
// over Reducer guarantees the two can never diverge.
func Parse(agent Agent, raw []byte) (Session, error) {
	x, err := NewReducer(agent)
	if err != nil {
		return Session{}, err
	}
	if err := x.Feed(raw, 0); err != nil {
		return Session{}, err
	}
	return assemble(x.Finish()), nil
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
		m.ThinkingBytes += op.ThinkingBytes
		if op.Model != "" {
			m.Model = op.Model
		}
		if op.HasThinking {
			m.HasThinking = true
		}
		if op.HasToolUse {
			m.HasToolUse = true
		}
	}
	sort.Ints(order)
	for _, ord := range order {
		s.Messages = append(s.Messages, *byOrd[ord])
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
		tc.ResultBody, tc.ResultSHA256 = tr.Body, tr.BodySHA256
		tc.ResultBytes, tc.ResultMediaType, tc.ResultStatus = tr.Bytes, tr.MediaType, tr.Status
	}

	for _, a := range d.Attachments {
		s.Attachments = append(s.Attachments, Attachment{
			MessageOrdinal: a.MessageOrdinal,
			SHA256:         a.SHA256,
			Bytes:          a.Bytes,
			MediaType:      a.MediaType,
			Filename:       a.Filename,
			Content:        a.Content,
		})
	}

	s.UsageEvent = d.Usage
	s.Fallbacks = d.Fallbacks
	return s
}
