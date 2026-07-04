// Package parse is the server-side pipeline that turns a session's stored raw
// bytes into the queryable projection. Parsing has exactly one shape: rebuild
// the whole session. The per-agent reducer is fed the session's complete bytes,
// each usage event is priced from the compiled-in table, and the store swaps
// the folded projection in atomically (see store.RebuildSession). There is no
// incremental parse, no serialized parser state, and no separate reparse
// mechanism: a session that gained bytes, a session whose last parse failed,
// and a corpus behind a new parser epoch are all "the projection is behind the
// raw bytes", handled by the same rebuild.
package parse

import (
	"context"

	"github.com/jssblck/akari/internal/parser"
	"github.com/jssblck/akari/internal/pricing"
	"github.com/jssblck/akari/internal/server/store"
)

// ParserError marks a failure that came from the parser reducer itself: malformed
// transcript bytes the reducer cannot turn into a projection. It is distinct from an
// operational error (a store query, a CAS read, a cancelled context), which travels
// up un-wrapped. The worker uses this distinction: a parser error is per-session and
// deterministic (re-running fails the same way), so the store records the attempt on
// session_raw's failure markers (parse_error plus the epoch and raw length it tried)
// and the session retries only when its bytes or the epoch move; an operational
// error records nothing, so the next drain retries it.
type ParserError struct{ err error }

func (e *ParserError) Error() string { return e.err.Error() }
func (e *ParserError) Unwrap() error { return e.err }

// Rebuild rebuilds one session's whole projection from its stored raw bytes at
// the running Epoch. It is the only projection write path: the parse worker
// calls it for every due session, whether the session gained bytes, failed its
// last parse, or sits behind a bumped epoch.
func Rebuild(ctx context.Context, st *store.Store, sessionID int64, agent string) error {
	r, err := newSessionReducer(agent)
	if err != nil {
		return &ParserError{err: err}
	}
	return st.RebuildSession(ctx, sessionID, Epoch, r)
}

// sessionReducer adapts the per-agent parser reducer to the store's
// SessionReducer seam: Feed delegates to the reducer (marking its failures as
// ParserError so the store and worker can classify them), and Finish prices the
// completed delta into the store's shape.
type sessionReducer struct {
	r *parser.Reducer
}

func newSessionReducer(agent string) (*sessionReducer, error) {
	r, err := parser.NewReducer(parser.Agent(agent))
	if err != nil {
		return nil, err
	}
	return &sessionReducer{r: r}, nil
}

func (s *sessionReducer) Feed(region []byte, baseOffset int64) error {
	if err := s.r.Feed(region, baseOffset); err != nil {
		return &ParserError{err: err}
	}
	return nil
}

func (s *sessionReducer) Finish() store.ProjectionDelta {
	return toProjectionDelta(s.r.Finish())
}

// toProjectionDelta maps a parser delta to the store delta, pricing each usage
// event from the compiled-in table. It does not accumulate session-level token or
// cost counters: those are derived by the store's fold from the deduped set that
// actually persists, so the rollups count exactly the surviving ledger set.
func toProjectionDelta(p parser.Delta) store.ProjectionDelta {
	d := store.ProjectionDelta{
		Started: p.Started,
		Ended:   p.Ended,
	}

	for _, m := range p.Messages {
		d.Messages = append(d.Messages, store.MessageDelta{
			Ordinal:       m.Ordinal,
			Role:          string(m.Role),
			Content:       m.Content,
			ThinkingText:  m.ThinkingText,
			ThinkingBytes: m.ThinkingBytes,
			Model:         m.Model,
			HasThinking:   m.HasThinking,
			HasToolUse:    m.HasToolUse,
			Timestamp:     m.Timestamp,
		})
	}

	for _, t := range p.ToolCalls {
		tc := store.ProjToolCall{
			MessageOrdinal: t.MessageOrdinal,
			CallIndex:      t.CallIndex,
			ToolName:       t.ToolName,
			Category:       t.Category,
			FilePath:       t.FilePath,
			Detail:         t.Detail,
			CallUID:        t.CallUID,
		}
		switch {
		case t.InputSHA256 != "":
			// The client lifted the input to the CAS; record the reference and its
			// declared metadata with no inline body for the server to re-store.
			tc.InputSHA256 = t.InputSHA256
			tc.InputBytes = int64(t.InputBytes)
			tc.InputMediaType = t.InputMediaType
		case t.InputJSON != "":
			// Carry the parsed input string straight through. gjson aliases the
			// region, and the blob writer streams it in slices, so the body is never
			// copied whole into a second buffer on the way to the CAS. Most agents'
			// inputs are JSON; a parser that recorded a different media (a custom tool
			// call's plain-text input) overrides the default.
			tc.InputBody = t.InputJSON
			tc.InputBytes = int64(len(t.InputJSON))
			tc.InputMediaType = t.InputMediaType
			if tc.InputMediaType == "" {
				tc.InputMediaType = "application/json"
			}
		}
		d.ToolCalls = append(d.ToolCalls, tc)
	}

	for _, tr := range p.ToolResults {
		d.ToolResults = append(d.ToolResults, store.ToolResultDelta{
			CallUID:    tr.CallUID,
			Body:       tr.Body,
			BodySHA256: tr.BodySHA256,
			Bytes:      int64(tr.Bytes),
			MediaType:  tr.MediaType,
			Status:     tr.Status,
		})
	}

	for _, a := range p.Attachments {
		d.Attachments = append(d.Attachments, store.AttachmentDelta{
			MessageOrdinal: a.MessageOrdinal,
			SHA256:         a.SHA256,
			Body:           a.Content,
			Bytes:          int64(a.Bytes),
			MediaType:      a.MediaType,
			Filename:       a.Filename,
		})
	}

	for _, u := range p.Usage {
		pu := store.ProjUsage{
			MessageOrdinal: u.MessageOrdinal,
			Model:          u.Model,
			Input:          u.Input,
			Output:         u.Output,
			CacheWrite:     u.CacheWrite,
			CacheRead:      u.CacheRead,
			Reasoning:      u.Reasoning,
			OccurredAt:     u.OccurredAt,
			DedupKey:       u.DedupKey,
			SourceOffset:   u.SourceOffset,
			SourceIndex:    u.SourceIndex,
		}
		// Price the event here at the rate in effect when it occurred (OccurredAt
		// selects the date-effective window); whether it counts toward the session
		// total is decided by the store's dedup fold, where a duplicate usage line is
		// dropped and only the surviving row folds into the rollup.
		if cost, known := pricing.Cost(u.Model, u.OccurredAt, u.Input, u.Output, u.CacheWrite, u.CacheRead); known {
			pu.CostUSD = &cost
		}
		d.Usage = append(d.Usage, pu)
	}

	for _, fb := range p.Fallbacks {
		d.Fallbacks = append(d.Fallbacks, store.ProjFallback{
			MessageOrdinal:     fb.MessageOrdinal,
			FromModel:          fb.FromModel,
			ToModel:            fb.ToModel,
			Trigger:            fb.Trigger,
			RefusalCategory:    fb.RefusalCategory,
			RefusalExplanation: fb.RefusalExplanation,
			// The declined token counts are meaningful only when the reducer actually summed
			// them from fallback_message iteration entries (DeclinedObserved). A system-side op,
			// or an assistant op that carried a fallback block but no iterations, never observed
			// the declined spend, so its zero maps to NULL ("unmeasured") for the merge rather
			// than folding a spurious zero over a value the paired assistant line supplied.
			DeclinedInput:      declinedTokens(fb.DeclinedObserved, fb.DeclinedInput),
			DeclinedOutput:     declinedTokens(fb.DeclinedObserved, fb.DeclinedOutput),
			DeclinedCacheWrite: declinedTokens(fb.DeclinedObserved, fb.DeclinedCacheWrite),
			DeclinedCacheRead:  declinedTokens(fb.DeclinedObserved, fb.DeclinedCacheRead),
			OccurredAt:         fb.OccurredAt,
			DedupKey:           fb.DedupKey,
		})
	}

	return d
}

// declinedTokens turns a reducer-side declined token count into the store's nullable
// column: only an op that observed the declined attempt (summed it from fallback_message
// iteration entries) carries a measured value, so its count (even zero) maps to a pointer;
// an op that never observed the spend maps to NULL, leaving the column for the paired
// assistant line to fill rather than pinning it to a spurious zero.
func declinedTokens(observed bool, n int) *int {
	if !observed {
		return nil
	}
	return &n
}
