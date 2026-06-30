package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bodyCeiling bounds how many bytes a single body-reading tool will return,
// whatever max_bytes a caller asks for. It keeps one tool result from pulling an
// oversized CAS object or a giant raw transcript into a response. Callers that need
// more page with max_bytes against successive sessions, not one unbounded fetch.
const bodyCeiling = 8 << 20 // 8 MiB

// defaultBodyMax is the cap applied when a body-reading tool is called without an
// explicit max_bytes: generous enough for nearly every tool body and most whole
// transcripts, small enough not to flood a context window by accident.
const defaultBodyMax = 1 << 20 // 1 MiB

// registerTools wires every read tool onto s, each closing over the store.
func registerTools(s *mcp.Server, st *store.Store) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "whoami",
		Description: "Return the akari account the current credential authenticates as: user id, username, and whether it is an admin.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, whoamiDTO, error) {
		uid, err := callerID(req)
		if err != nil {
			return nil, whoamiDTO{}, err
		}
		u, err := st.UserByID(ctx, uid)
		if err != nil {
			return nil, whoamiDTO{}, mapNotFound(err, "user")
		}
		return jsonResult(whoamiDTO{UserID: u.ID, Username: u.Username, IsAdmin: u.IsAdmin})
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "overview",
		Description: "Fleet-wide usage for a trailing window: total cost, token volume by class, session count, a daily time series, and by-model and by-agent breakdowns. Also lists the accounts present, so their ids can scope later calls. This is the data behind the web Overview.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in overviewInput) (*mcp.CallToolResult, overviewDTO, error) {
		a, err := st.Analytics(ctx, store.AnalyticsFilter{Since: sinceFromDays(in.Days), UserIDs: in.UserIDs})
		if err != nil {
			return nil, overviewDTO{}, err
		}
		users, err := st.ListUsers(ctx)
		if err != nil {
			return nil, overviewDTO{}, err
		}
		out := overviewDTO{Window: windowLabel(in.Days), Analytics: analyticsToDTO(a), Users: usersToRefs(users)}
		return jsonResult(out)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_projects",
		Description: "Every project (a git remote or a local folder), most recently active first, each with its session count, cost, and token totals. This is the projects index.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, projectsDTO, error) {
		ps, err := st.ListProjects(ctx)
		if err != nil {
			return nil, projectsDTO{}, err
		}
		out := projectsDTO{Projects: make([]projectDTO, 0, len(ps))}
		for _, p := range ps {
			out.Projects = append(out.Projects, projectToDTO(p))
		}
		return jsonResult(out)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_project",
		Description: "One project's identity plus its usage analytics for a trailing window (optionally narrowed by agent, user, or machine) and the distinct agents, users, and machines that ran in it. This is the project page.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in getProjectInput) (*mcp.CallToolResult, projectDetailDTO, error) {
		p, err := st.Project(ctx, in.ProjectID)
		if err != nil {
			return nil, projectDetailDTO{}, mapNotFound(err, "project")
		}
		a, err := st.Analytics(ctx, store.AnalyticsFilter{
			ProjectID: in.ProjectID, Since: sinceFromDays(in.Days),
			Agent: in.Agent, Username: in.Username, Machine: in.Machine,
		})
		if err != nil {
			return nil, projectDetailDTO{}, err
		}
		f, err := st.SessionFacets(ctx, in.ProjectID)
		if err != nil {
			return nil, projectDetailDTO{}, err
		}
		out := projectDetailDTO{
			Project:   projectToDTO(p),
			Window:    windowLabel(in.Days),
			Analytics: analyticsToDTO(a),
			Facets:    facetValuesDTO{Agents: f.Agents, Machines: f.Machines, Users: f.Users},
		}
		return jsonResult(out)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_sessions",
		Description: "The cross-project session feed, newest activity first, with optional filters (project_id, agent, username, machine, and a trailing-day window) and sortable columns. Returns the facet rail too: the busiest agents, users, machines, and projects with counts, whose values are the exact strings to pass back as filters. Up to 500 rows; page with limit and offset.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listSessionsInput) (*mcp.CallToolResult, sessionsDTO, error) {
		rows, err := st.ListAllSessions(ctx, store.SessionFilter{
			ProjectID: in.ProjectID, Agent: in.Agent, Machine: in.Machine, Username: in.Username,
			Since: sinceFromDays(in.Days), Sort: in.Sort, Desc: in.Desc, Limit: in.Limit, Offset: in.Offset,
		})
		if err != nil {
			return nil, sessionsDTO{}, err
		}
		facets, err := st.GlobalFacets(ctx)
		if err != nil {
			return nil, sessionsDTO{}, err
		}
		out := sessionsDTO{Sessions: make([]sessionDTO, 0, len(rows)), Facets: globalFacetsToDTO(facets)}
		for _, r := range rows {
			out.Sessions = append(out.Sessions, sessionRowToDTO(r))
		}
		return jsonResult(out)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_session",
		Description: "One session in full: its header (project, agent, user, machine, branch, working directory, token and cost totals, timing), and, unless include_transcript is false, the whole transcript: messages with thinking text and model, tool-call metadata (the bodies live in the CAS, fetch them with read_tool_body), attachments, and any subagent sessions it spawned.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in getSessionInput) (*mcp.CallToolResult, sessionDetailDTO, error) {
		d, err := st.SessionDetailByID(ctx, in.SessionID)
		if err != nil {
			return nil, sessionDetailDTO{}, mapNotFound(err, "session")
		}
		out := sessionDetailToDTO(d)

		dup, err := st.DuplicateCallUIDCount(ctx, in.SessionID)
		if err != nil {
			return nil, sessionDetailDTO{}, err
		}
		out.DuplicateToolCallIDs = dup

		subs, err := st.Subagents(ctx, in.SessionID)
		if err != nil {
			return nil, sessionDetailDTO{}, err
		}
		for _, sub := range subs {
			out.Subagents = append(out.Subagents, sessionSummaryToDTO(sub))
		}

		if in.IncludeTranscript == nil || *in.IncludeTranscript {
			msgs, err := st.Messages(ctx, in.SessionID)
			if err != nil {
				return nil, sessionDetailDTO{}, err
			}
			for _, m := range msgs {
				out.Messages = append(out.Messages, messageToDTO(m))
			}
			calls, err := st.ToolCalls(ctx, in.SessionID)
			if err != nil {
				return nil, sessionDetailDTO{}, err
			}
			for _, c := range calls {
				out.ToolCalls = append(out.ToolCalls, toolCallToDTO(c))
			}
			atts, err := st.Attachments(ctx, in.SessionID)
			if err != nil {
				return nil, sessionDetailDTO{}, err
			}
			for _, a := range atts {
				out.Attachments = append(out.Attachments, attachmentToDTO(a))
			}
		}
		return jsonResult(out)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_tool_body",
		Description: "Fetch a tool call's input or result body from the content-addressed store by its sha256, as it appears on a tool call from get_session. The session must reference the hash (the same gate the web UI enforces). Text bodies return as text; binary bodies return base64-encoded. Capped by max_bytes (default 1 MiB, ceiling 8 MiB).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in readBodyInput) (*mcp.CallToolResult, bodyDTO, error) {
		if in.SHA256 == "" {
			return nil, bodyDTO{}, errors.New("sha256 is required")
		}
		ok, err := st.SessionReferencesBlob(ctx, in.SessionID, in.SHA256)
		if err != nil {
			return nil, bodyDTO{}, err
		}
		if !ok {
			return nil, bodyDTO{}, errors.New("session does not reference that body, or it does not exist")
		}
		limit := clampMax(in.MaxBytes)
		var buf cappedBuffer
		buf.limit = limit
		mediaType, err := st.WriteBlobTo(ctx, &buf, in.SHA256)
		if err != nil {
			return nil, bodyDTO{}, mapNotFound(err, "tool body")
		}
		meta, err := st.BlobMeta(ctx, in.SHA256)
		if err != nil {
			return nil, bodyDTO{}, mapNotFound(err, "tool body")
		}
		out := bodyDTO{
			SHA256:    in.SHA256,
			MediaType: mediaType,
			ByteLen:   meta.ByteLen,
			Truncated: buf.truncated || buf.written < meta.ByteLen,
		}
		encodeBody(&out, buf.buf.Bytes())
		return jsonResult(out)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_session_raw",
		Description: "The raw, lossless bytes a session was ingested from (the agent's own JSONL log), the source every parsed projection is rebuilt from. Use this to inspect exactly what was uploaded, behind the parsed transcript. Capped by max_bytes (default 1 MiB, ceiling 8 MiB); total_bytes reports the full size so you know whether the read was truncated.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in rawInput) (*mcp.CallToolResult, rawDTO, error) {
		limit := clampMax(in.MaxBytes)
		var buf bytes.Buffer
		written, truncated, total, err := st.SessionRawTo(ctx, &buf, in.SessionID, limit)
		if err != nil {
			return nil, rawDTO{}, mapNotFound(err, "session")
		}
		out := rawDTO{TotalBytes: total, BytesReturned: written, Truncated: truncated}
		encodeRaw(&out, buf.Bytes())
		return jsonResult(out)
	})
}

// sinceFromDays converts a trailing-day window into the lower time bound the store
// filters expect. Zero or negative means all of history (the zero time), matching
// the "all" range the web UI offers.
func sinceFromDays(days int) time.Time {
	if days <= 0 {
		return time.Time{}
	}
	return time.Now().Add(-time.Duration(days) * 24 * time.Hour)
}

// windowLabel describes the applied window in the output, so a reader knows whether
// a figure is all-time or bounded without inferring it from the request.
func windowLabel(days int) string {
	if days <= 0 {
		return "all"
	}
	return fmt.Sprintf("%dd", days)
}

// clampMax resolves a requested byte cap to the default when unset and to the
// ceiling when oversized, so a body tool never returns an unbounded payload.
func clampMax(max int) int64 {
	if max <= 0 {
		return defaultBodyMax
	}
	if max > bodyCeiling {
		return bodyCeiling
	}
	return int64(max)
}

// cappedBuffer is an io.Writer that retains at most limit bytes and notes whether
// more was offered. WriteBlobTo streams a whole (decompressed) blob through it, so
// the cap bounds memory even when the underlying body is large; bytes past the cap
// are counted (to detect truncation) but discarded.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.written; room > 0 {
		take := int64(len(p))
		if take > room {
			take = room
			c.truncated = true
		}
		c.buf.Write(p[:take])
	} else if len(p) > 0 {
		c.truncated = true
	}
	c.written += int64(len(p))
	return len(p), nil
}

// encodeBody fills a bodyDTO's content as UTF-8 text when the bytes are valid text,
// and base64 otherwise, so a textual tool body reads directly while a binary one
// still round-trips.
func encodeBody(out *bodyDTO, b []byte) {
	if utf8.Valid(b) {
		out.Encoding = "text"
		out.Content = string(b)
		return
	}
	out.Encoding = "base64"
	out.Content = base64.StdEncoding.EncodeToString(b)
}

// encodeRaw mirrors encodeBody for the raw-bytes tool. Ingested transcripts are
// JSONL text in practice, so this is text almost always, with base64 as the safe
// fallback for anything that is not valid UTF-8.
func encodeRaw(out *rawDTO, b []byte) {
	if utf8.Valid(b) {
		out.Encoding = "text"
		out.Content = string(b)
		return
	}
	out.Encoding = "base64"
	out.Content = base64.StdEncoding.EncodeToString(b)
}
