package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultTranscriptLimit bounds how many messages get_session returns when the
// caller does not ask for a specific window, so a single call never materializes an
// arbitrarily large session. Callers page further with transcript_after.
const defaultTranscriptLimit = 200

// maxTranscriptWindow caps a single transcript window regardless of the requested
// limit, so one get_session call cannot pull an unbounded slice into a response. It
// sits below the store's own clamp so the has_more peek (limit+1) never trips it.
const maxTranscriptWindow = 1000

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
			Project:   projectIdentity(p),
			Window:    windowLabel(in.Days),
			Analytics: analyticsToDTO(a),
			Facets:    facetValuesDTO{Agents: f.Agents, Machines: f.Machines, Users: f.Users},
		}
		return jsonResult(out)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_sessions",
		Description: "The cross-project session feed, newest session first, with optional filters (project_id, agent, username, machine, and a trailing-day window). Each row carries its last-activity time (updated_at) so you can order a page by recency. Returns the facet rail too: the busiest agents, users, machines, and projects with counts, whose values are the exact strings to pass back as filters. Up to 500 rows per page; page forward with the returned next_cursor (a stable id keyset, so paging the whole feed is complete even as sessions are re-activated mid-walk).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listSessionsInput) (*mcp.CallToolResult, sessionsDTO, error) {
		cursor, err := decodeCursor(in.Cursor)
		if err != nil {
			return nil, sessionsDTO{}, err
		}
		rows, next, err := st.SessionFeed(ctx, store.SessionFilter{
			ProjectID: in.ProjectID, Agent: in.Agent, Machine: in.Machine, Username: in.Username,
			Since: sinceFromDays(in.Days),
			// The MCP feed lists every session an agent might inspect, including ones
			// whose parse produced no readable message (still reachable by id); the
			// empty-hiding is a global-web-feed affordance, not this API's contract.
			IncludeEmpty: true,
		}, in.Limit, cursor)
		if err != nil {
			return nil, sessionsDTO{}, err
		}
		facets, err := st.GlobalFacets(ctx)
		if err != nil {
			return nil, sessionsDTO{}, err
		}
		out := sessionsDTO{
			Sessions:   make([]sessionDTO, 0, len(rows)),
			NextCursor: encodeCursor(next),
			Facets:     globalFacetsToDTO(facets),
		}
		for _, r := range rows {
			out.Sessions = append(out.Sessions, sessionRowToDTO(r))
		}
		return jsonResult(out)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_session",
		Description: "One session: its header (project, agent, user, machine, branch, working directory, token and cost totals, timing) and subagents, plus, unless include_transcript is false, a bounded window of the transcript (messages with thinking and model, the tool-call metadata and attachments hanging on them). Tool bodies live in the CAS; fetch them with read_tool_body. The window is up to transcript_limit messages; page by passing the prior window's transcript.next_after as transcript_after until transcript.has_more is false.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in getSessionInput) (*mcp.CallToolResult, sessionDetailDTO, error) {
		d, err := st.SessionDetailByID(ctx, in.SessionID)
		if err != nil {
			return nil, sessionDetailDTO{}, mapNotFound(err, "session")
		}
		out := sessionDetailToDTO(d)

		includeTranscript := in.IncludeTranscript == nil || *in.IncludeTranscript

		// The duplicate-id count is a session-wide aggregate (a GROUP BY over every
		// tool_call), so computing it on each transcript page would make a full paged
		// read superlinear. Compute it only on the first view: a header-only call, or the
		// first transcript window (transcript_after unset). Later pages omit it.
		if !includeTranscript || in.TranscriptAfter == nil {
			dup, err := st.DuplicateCallUIDCount(ctx, in.SessionID)
			if err != nil {
				return nil, sessionDetailDTO{}, err
			}
			out.DuplicateToolCallIDs = &dup
		}

		subs, err := st.Subagents(ctx, in.SessionID)
		if err != nil {
			return nil, sessionDetailDTO{}, err
		}
		for _, sub := range subs {
			out.Subagents = append(out.Subagents, sessionSummaryToDTO(sub))
		}

		if includeTranscript {
			tr, err := loadTranscript(ctx, st, in.SessionID, in.TranscriptAfter, in.TranscriptLimit, d.MessageCount)
			if err != nil {
				return nil, sessionDetailDTO{}, err
			}
			out.Transcript = tr
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
		// Read the stored size first, then pull at most `limit` bytes of the body. The
		// large-object reader transfers only the prefix it copies, so a capped preview of a
		// bulky CAS body costs O(limit), not O(blob), and truncation is the stored length
		// against the cap rather than something measured by reading the whole object.
		meta, err := st.BlobMeta(ctx, in.SHA256)
		if err != nil {
			return nil, bodyDTO{}, mapNotFound(err, "tool body")
		}
		var buf bytes.Buffer
		mediaType, err := st.WriteBlobPrefixTo(ctx, &buf, in.SHA256, limit)
		if err != nil {
			return nil, bodyDTO{}, mapNotFound(err, "tool body")
		}
		out := bodyDTO{
			SHA256:    in.SHA256,
			MediaType: mediaType,
			ByteLen:   meta.ByteLen,
			Truncated: meta.ByteLen > limit,
		}
		out.Encoding, out.Content = encodeBytes(buf.Bytes())
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
		out.Encoding, out.Content = encodeBytes(buf.Bytes())
		return jsonResult(out)
	})
}

// loadTranscript reads one bounded window of a session's transcript: the messages
// whose ordinal is greater than after (nil for the first window), and exactly the
// tool calls and attachments hanging on those messages. It pages by keyset on
// ordinal, so peak memory stays proportional to the window and reading a whole
// session window by window is linear, not quadratic. total is the session's full
// message count, reported for context. has_more is exact: the read peeks one row past
// the window rather than counting.
func loadTranscript(ctx context.Context, st *store.Store, sessionID int64, after *int, limit, total int) (*transcriptDTO, error) {
	if limit <= 0 {
		limit = defaultTranscriptLimit
	}
	if limit > maxTranscriptWindow {
		limit = maxTranscriptWindow
	}
	// Fetch one extra row to learn whether a further window exists without a count.
	msgs, err := st.MessagesAfter(ctx, sessionID, after, limit+1)
	if err != nil {
		return nil, err
	}
	hasMore := len(msgs) > limit
	if hasMore {
		msgs = msgs[:limit]
	}
	tr := &transcriptDTO{
		Limit:         limit,
		Returned:      len(msgs),
		TotalMessages: total,
		HasMore:       hasMore,
		Messages:      make([]messageDTO, 0, len(msgs)),
		ToolCalls:     []toolCallDTO{},
		Attachments:   []attachmentDTO{},
	}
	for _, m := range msgs {
		tr.Messages = append(tr.Messages, messageToDTO(m))
	}
	if len(msgs) == 0 {
		return tr, nil
	}

	// The returned messages are ordered by ordinal, so their span is the closed
	// range [first, last]; fetch only the calls and attachments inside it.
	minOrd, maxOrd := msgs[0].Ordinal, msgs[len(msgs)-1].Ordinal
	if hasMore {
		// The next window resumes strictly after the last ordinal in this one.
		next := maxOrd
		tr.NextAfter = &next
	}
	calls, err := st.ToolCallsInRange(ctx, sessionID, minOrd, maxOrd)
	if err != nil {
		return nil, err
	}
	for _, c := range calls {
		tr.ToolCalls = append(tr.ToolCalls, toolCallToDTO(c))
	}
	atts, err := st.AttachmentsInRange(ctx, sessionID, minOrd, maxOrd)
	if err != nil {
		return nil, err
	}
	for _, a := range atts {
		tr.Attachments = append(tr.Attachments, attachmentToDTO(a))
	}
	return tr, nil
}

// encodeCursor renders a feed cursor as an opaque, URL-safe token. nil (the last
// page) renders to the empty string. The cursor is the last row's immutable id, so a
// resumed walk cannot skip rows that were re-activated since the prior page.
func encodeCursor(c *store.SessionFeedCursor) string {
	if c == nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.ID, 10)))
}

// decodeCursor parses a cursor produced by encodeCursor. An empty string is the
// first page (nil cursor); anything malformed is a caller error.
func decodeCursor(s string) (*store.SessionFeedCursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("invalid cursor")
	}
	id, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return nil, errors.New("invalid cursor")
	}
	return &store.SessionFeedCursor{ID: id}, nil
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

// encodeBytes renders body bytes as UTF-8 text when they are valid text, and
// base64 otherwise, so a textual body reads directly while a binary one still
// round-trips. Both byte-returning tools (tool body and session raw) share it.
func encodeBytes(b []byte) (encoding, content string) {
	if utf8.Valid(b) {
		return "text", string(b)
	}
	return "base64", base64.StdEncoding.EncodeToString(b)
}
