// Package parse is the server-side pipeline that turns a session's stored raw
// bytes into the queryable projection: it runs the per-agent parser, computes
// cost from the compiled-in pricing table, and writes the result, replacing any
// prior projection for that session.
package parse

import (
	"context"
	"errors"

	"github.com/jssblck/akari/internal/parser"
	"github.com/jssblck/akari/internal/pricing"
	"github.com/jssblck/akari/internal/server/store"
)

// Version is the parser projection version. Bump it when parsing changes so a
// reparse can be told which sessions are stale.
const Version = 1

// SessionFromRaw loads a session's raw bytes, parses them for the given agent,
// computes cost, writes the projection, and returns the message count.
func SessionFromRaw(ctx context.Context, st *store.Store, sessionID int64, agent string) (int, error) {
	raw, err := st.LoadRaw(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	parsed, err := parser.Parse(parser.Agent(agent), raw)
	if err != nil {
		return 0, err
	}
	proj := toProjection(parsed)
	if err := st.WriteProjection(ctx, sessionID, int64(len(raw)), proj); err != nil {
		// A concurrent parse of newer bytes is authoritative; this one is a no-op.
		if errors.Is(err, store.ErrStaleProjection) {
			return proj.MessageCount, nil
		}
		return 0, err
	}
	return proj.MessageCount, nil
}

// toProjection converts a parsed session into the store projection, applying
// pricing to each usage event.
func toProjection(s parser.Session) store.Projection {
	p := store.Projection{
		StartedAt:     s.StartedAt,
		EndedAt:       s.EndedAt,
		ParserVersion: Version,
	}

	for _, m := range s.Messages {
		p.Messages = append(p.Messages, store.ProjMessage{
			Ordinal:       m.Ordinal,
			Role:          string(m.Role),
			Content:       m.Content,
			ThinkingText:  m.ThinkingText,
			Model:         m.Model,
			Timestamp:     m.Timestamp,
			HasThinking:   m.HasThinking,
			HasToolUse:    m.HasToolUse,
			ContentLength: len(m.Content),
		})
		p.MessageCount++
		if m.Role == parser.RoleUser {
			p.UserMessageCount++
		}
	}

	for _, t := range s.ToolCalls {
		tc := store.ProjToolCall{
			MessageOrdinal: t.MessageOrdinal,
			CallIndex:      t.CallIndex,
			ToolName:       t.ToolName,
			Category:       t.Category,
			FilePath:       t.FilePath,
			InputBytes:     int64(len(t.InputJSON)),
		}
		if t.InputJSON != "" {
			tc.InputBody = []byte(t.InputJSON)
			tc.InputMediaType = "application/json"
		}
		if t.ResultStatus != "" {
			tc.HasResult = true
			tc.ResultBytes = int64(t.ResultBytes)
			tc.ResultMediaType = t.ResultMediaType
			if tc.ResultMediaType == "" {
				tc.ResultMediaType = "text/plain"
			}
			tc.ResultStatus = t.ResultStatus
			if len(t.ResultBody) > 0 {
				tc.ResultBody = []byte(t.ResultBody)
			}
		}
		p.ToolCalls = append(p.ToolCalls, tc)
	}

	for _, u := range s.UsageEvent {
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
		}
		if cost, known := pricing.Cost(u.Model, u.Input, u.Output, u.CacheWrite, u.CacheRead); known {
			pu.CostUSD = &cost
			p.TotalCostUSD += cost
		} else if u.Input+u.Output+u.CacheWrite+u.CacheRead+u.Reasoning > 0 {
			// Tokens were spent on a model we cannot price: the session total is
			// a partial sum.
			p.CostIncomplete = true
		}
		p.TotalInput += int64(u.Input)
		p.TotalOutput += int64(u.Output)
		p.TotalCacheWrite += int64(u.CacheWrite)
		p.TotalCacheRead += int64(u.CacheRead)
		p.Usage = append(p.Usage, pu)
	}

	return p
}
