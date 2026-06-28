// Package parse is the server-side pipeline that turns a session's stored raw
// bytes into the queryable projection. It runs the per-agent reducer over the
// unparsed tail of a session, prices each usage event from the compiled-in
// pricing table, and applies the result incrementally: each chunk does work
// proportional to its own bytes, not to the whole session.
package parse

import (
	"context"

	"github.com/jssblck/akari/internal/parser"
	"github.com/jssblck/akari/internal/pricing"
	"github.com/jssblck/akari/internal/server/store"
)

// Version is the parser projection version. Bump it when parsing changes so a
// reparse can be told which sessions are stale. A session parsed past byte 0 by a
// different version cannot be resumed incrementally; reparse rewinds and replays
// it from scratch.
const Version = 1

// Advance parses any not-yet-parsed bytes of a session and applies them to the
// projection, looping until the parse cursor catches up to the stored length. It
// returns the session's message count. The raw bytes are never modified; a parser
// error leaves the cursor where it was for the next chunk or a reparse to retry.
func Advance(ctx context.Context, st *store.Store, sessionID int64, agent string) (int, error) {
	reduce := reduceFunc(agent)
	for {
		_, caughtUp, err := st.AdvanceProjection(ctx, sessionID, Version, reduce)
		if err != nil {
			return 0, err
		}
		if caughtUp {
			break
		}
	}
	return st.MessageCount(ctx, sessionID)
}

// Reparse rebuilds a session's projection from its stored raw bytes: it clears
// the derived rows and rewinds the parse cursor, then replays the whole session
// through the same reducer the live path uses. This is how a parser improvement
// reaches already-ingested data without re-uploading anything.
func Reparse(ctx context.Context, st *store.Store, sessionID int64, agent string) (int, error) {
	if err := st.ResetProjectionForReparse(ctx, sessionID, Version); err != nil {
		return 0, err
	}
	return Advance(ctx, st, sessionID, agent)
}

// reduceFunc adapts the per-agent reducer to the store's ReduceFunc: it decodes
// the carry-over state, runs the reducer over the region, prices the usage, and
// returns the re-encoded state plus the store-shaped delta.
func reduceFunc(agent string) store.ReduceFunc {
	return func(stateBytes, region []byte, baseOffset int64) ([]byte, store.ProjectionDelta, error) {
		st, err := parser.DecodeState(stateBytes)
		if err != nil {
			return nil, store.ProjectionDelta{}, err
		}
		next, d, err := parser.Reduce(parser.Agent(agent), st, region, baseOffset)
		if err != nil {
			return nil, store.ProjectionDelta{}, err
		}
		encoded, err := next.Encode()
		if err != nil {
			return nil, store.ProjectionDelta{}, err
		}
		return encoded, toProjectionDelta(d), nil
	}
}

// toProjectionDelta maps a parser delta to the store delta, applying pricing to
// each usage event and accumulating the session-level token and cost increments.
func toProjectionDelta(p parser.Delta) store.ProjectionDelta {
	d := store.ProjectionDelta{
		MessagesAdded:     p.MessagesAdded,
		UserMessagesAdded: p.UserMessagesAdded,
		Started:           p.Started,
		Ended:             p.Ended,
	}

	for _, m := range p.Messages {
		d.Messages = append(d.Messages, store.MessageDelta{
			Ordinal:      m.Ordinal,
			Role:         string(m.Role),
			Content:      m.Content,
			ThinkingText: m.ThinkingText,
			Model:        m.Model,
			HasThinking:  m.HasThinking,
			HasToolUse:   m.HasToolUse,
			Timestamp:    m.Timestamp,
		})
	}

	for _, t := range p.ToolCalls {
		tc := store.ProjToolCall{
			MessageOrdinal: t.MessageOrdinal,
			CallIndex:      t.CallIndex,
			ToolName:       t.ToolName,
			Category:       t.Category,
			FilePath:       t.FilePath,
			InputBytes:     int64(len(t.InputJSON)),
			CallUID:        t.CallUID,
		}
		if t.InputJSON != "" {
			tc.InputBody = []byte(t.InputJSON)
			tc.InputMediaType = "application/json"
		}
		d.ToolCalls = append(d.ToolCalls, tc)
	}

	for _, tr := range p.ToolResults {
		trd := store.ToolResultDelta{
			CallUID:   tr.CallUID,
			Bytes:     int64(tr.Bytes),
			MediaType: tr.MediaType,
			Status:    tr.Status,
		}
		if len(tr.Body) > 0 {
			trd.Body = []byte(tr.Body)
		}
		d.ToolResults = append(d.ToolResults, trd)
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
		if cost, known := pricing.Cost(u.Model, u.Input, u.Output, u.CacheWrite, u.CacheRead); known {
			pu.CostUSD = &cost
			d.AddCostUSD += cost
		} else if u.Input+u.Output+u.CacheWrite+u.CacheRead+u.Reasoning > 0 {
			// Tokens spent on a model we cannot price: the session total is a partial
			// sum and the flag says so.
			d.CostIncomplete = true
		}
		d.AddInput += int64(u.Input)
		d.AddOutput += int64(u.Output)
		d.AddCacheWrite += int64(u.CacheWrite)
		d.AddCacheRead += int64(u.CacheRead)
		d.Usage = append(d.Usage, pu)
	}

	return d
}
