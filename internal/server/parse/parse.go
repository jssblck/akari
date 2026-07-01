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
//
// Version 2 changed how the session rollups are folded: they now count only the
// usage and message rows that survive their ON CONFLICT dedup, where version 1
// added every per-region occurrence and so inflated Claude sessions (which repeat
// a usage block across sidechain and summary lines). A version-1 session keeps its
// inflated rollup until a reparse rewinds it, which is why the fix ships with a
// version bump: an incremental advance over a still version-1 session would fold a
// correct delta onto a wrong base. Run `akari-server reparse` to correct the live
// data.
//
// Version 3 added Codex custom_tool_call bodies and binary image attachments (image
// generation results and pasted images) to the projection, so a reparse backfills
// those rows on already-ingested sessions.
//
// Version 4 tags each usage event with is_sidechain (a Claude subagent turn's
// accounting, written into the parent transcript), so a reparse backfills the flag
// and context-health analysis can read main-thread turns alone.
const Version = 4

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

// ParserError marks a failure that came from the parser reducer itself: malformed
// transcript bytes the reducer cannot turn into a projection. It is distinct from an
// operational error (a store query, a CAS read, a cancelled context), which travels
// up un-wrapped. The reparse service uses this distinction: a parser error is
// per-session and deterministic (re-running fails the same way), so it is counted and
// the run still completes; an operational error is treated as transient and aborts
// the run without stamping the epoch, so the next start retries rather than masking it.
type ParserError struct{ err error }

func (e *ParserError) Error() string { return e.err.Error() }
func (e *ParserError) Unwrap() error { return e.err }

// Reparse rebuilds a session's projection from its stored raw bytes by clearing the
// derived rows and replaying the whole session through the same reducer the live path
// uses, atomically (see store.ReparseSession): on any failure the prior projection is
// left intact rather than a cleared session. This is how a parser improvement reaches
// already-ingested data without re-uploading anything.
func Reparse(ctx context.Context, st *store.Store, sessionID int64, agent string) (int, error) {
	if err := st.ReparseSession(ctx, sessionID, Version, reduceFunc(agent)); err != nil {
		return 0, err
	}
	return st.MessageCount(ctx, sessionID)
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
			// A reducer failure is a deterministic parse error on these bytes; mark it so
			// a reparse can tell it apart from an operational store/CAS error.
			return nil, store.ProjectionDelta{}, &ParserError{err: err}
		}
		encoded, err := next.Encode()
		if err != nil {
			return nil, store.ProjectionDelta{}, err
		}
		return encoded, toProjectionDelta(d), nil
	}
}

// toProjectionDelta maps a parser delta to the store delta, pricing each usage
// event from the compiled-in table. It does not accumulate session-level token or
// cost increments: those are derived from the rows that actually persist (the store
// dedups usage on insert), so the rollups count exactly the surviving ledger set.
func toProjectionDelta(p parser.Delta) store.ProjectionDelta {
	d := store.ProjectionDelta{
		Started: p.Started,
		Ended:   p.Ended,
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
			IsSidechain:    u.IsSidechain,
		}
		// Price the event here; whether it counts toward the session total is decided
		// at insert time, where a duplicate usage line is dropped and only the
		// surviving row folds into the rollup (cost_incomplete included).
		if cost, known := pricing.Cost(u.Model, u.Input, u.Output, u.CacheWrite, u.CacheRead); known {
			pu.CostUSD = &cost
		}
		d.Usage = append(d.Usage, pu)
	}

	return d
}
