package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ProjectSummary is one row of the projects index: a project plus rolled-up
// session counts and token/cost totals.
type ProjectSummary struct {
	ID           int64
	RemoteKey    string
	Host         string
	Owner        string
	Repo         string
	DisplayName  string
	SessionCount int
	TotalCostUSD float64
	TotalInput   int64
	TotalOutput  int64
	LastActivity *time.Time
}

// SessionSummary is one row of a session list (project view, search results).
type SessionSummary struct {
	ID               int64
	Agent            string
	Machine          string
	GitBranch        string
	Username         string
	MessageCount     int
	UserMessageCount int
	TotalInput       int64
	TotalOutput      int64
	TotalCacheWrite  int64
	TotalCacheRead   int64
	TotalCostUSD     float64
	CostIncomplete   bool
	Visibility       string
	PublicID         *string
	StartedAt        *time.Time
	EndedAt          *time.Time
	UpdatedAt        *time.Time
}

// SessionDetail adds the owning project to a session summary.
type SessionDetail struct {
	SessionSummary
	OwnerID       int64
	ProjectID     int64
	ProjectKey    string
	ProjectName   string
	Cwd           string
	ParentID      *int64
	ParserVersion int
}

// Message is one transcript row for rendering.
type Message struct {
	Ordinal      int
	Role         string
	Content      string
	ThinkingText string
	Model        string
	HasThinking  bool
	HasToolUse   bool
	Timestamp    *time.Time
}

// ToolCallView is one tool call rendered as metadata (the body lives in the CAS).
type ToolCallView struct {
	MessageOrdinal  int
	CallIndex       int
	ToolName        string
	Category        string
	FilePath        string
	InputBytes      int64
	InputMediaType  string
	ResultBytes     int64
	ResultMediaType string
	ResultStatus    string
}

// SearchHit is one message matching a search, with its session context.
type SearchHit struct {
	SessionID  int64
	ProjectKey string
	Agent      string
	Username   string
	Ordinal    int
	Role       string
	Snippet    string
}

// SessionFilter narrows a session list. Empty fields are ignored.
type SessionFilter struct {
	ProjectID int64
	Agent     string
	Machine   string
	Username  string
	Limit     int
	Offset    int
}

// ListProjects returns every project with rolled-up stats, most recently active
// first.
func (s *Store) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT p.id, p.remote_key, p.host, p.owner, p.repo, p.display_name,
		        count(s.id),
		        coalesce(sum(s.total_cost_usd), 0),
		        coalesce(sum(s.total_input_tokens), 0),
		        coalesce(sum(s.total_output_tokens), 0),
		        max(s.updated_at)
		   FROM projects p
		   LEFT JOIN sessions s ON s.project_id = p.id
		  GROUP BY p.id
		  ORDER BY max(s.updated_at) DESC NULLS LAST, p.remote_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectSummary
	for rows.Next() {
		var p ProjectSummary
		if err := rows.Scan(&p.ID, &p.RemoteKey, &p.Host, &p.Owner, &p.Repo, &p.DisplayName,
			&p.SessionCount, &p.TotalCostUSD, &p.TotalInput, &p.TotalOutput, &p.LastActivity); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Project returns one project's identity (without rollups).
func (s *Store) Project(ctx context.Context, id int64) (ProjectSummary, error) {
	var p ProjectSummary
	err := s.Pool.QueryRow(ctx,
		`SELECT id, remote_key, host, owner, repo, display_name FROM projects WHERE id = $1`, id).
		Scan(&p.ID, &p.RemoteKey, &p.Host, &p.Owner, &p.Repo, &p.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectSummary{}, ErrNotFound
	}
	return p, err
}

// sessionSelect is the shared column list and joins for session summaries.
const sessionSelect = `
	SELECT s.id, s.agent, s.machine, s.git_branch, u.username,
	       s.message_count, s.user_message_count,
	       s.total_input_tokens, s.total_output_tokens,
	       s.total_cache_write_tokens, s.total_cache_read_tokens,
	       s.total_cost_usd, s.cost_incomplete, s.visibility, s.public_id,
	       s.started_at, s.ended_at, s.updated_at
	  FROM sessions s
	  JOIN users u ON u.id = s.user_id`

func scanSession(rows pgx.Rows) (SessionSummary, error) {
	var s SessionSummary
	err := rows.Scan(&s.ID, &s.Agent, &s.Machine, &s.GitBranch, &s.Username,
		&s.MessageCount, &s.UserMessageCount,
		&s.TotalInput, &s.TotalOutput, &s.TotalCacheWrite, &s.TotalCacheRead,
		&s.TotalCostUSD, &s.CostIncomplete, &s.Visibility, &s.PublicID,
		&s.StartedAt, &s.EndedAt, &s.UpdatedAt)
	return s, err
}

// ListSessions returns sessions matching the filter, newest first.
func (s *Store) ListSessions(ctx context.Context, f SessionFilter) ([]SessionSummary, error) {
	var conds []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, cond+" $"+itoa(len(args)))
	}
	if f.ProjectID != 0 {
		add("s.project_id =", f.ProjectID)
	}
	if f.Agent != "" {
		add("s.agent =", f.Agent)
	}
	if f.Machine != "" {
		add("s.machine =", f.Machine)
	}
	if f.Username != "" {
		add("u.username =", f.Username)
	}

	q := sessionSelect
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY s.updated_at DESC NULLS LAST, s.id DESC"
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)
	q += " LIMIT $" + itoa(len(args))
	if f.Offset > 0 {
		args = append(args, f.Offset)
		q += " OFFSET $" + itoa(len(args))
	}

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		sm, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// scanDetail loads one session with its project, by an arbitrary WHERE clause.
func (s *Store) scanDetail(ctx context.Context, where string, arg any) (SessionDetail, error) {
	var d SessionDetail
	err := s.Pool.QueryRow(ctx,
		`SELECT s.id, s.agent, s.machine, s.git_branch, u.username,
		        s.message_count, s.user_message_count,
		        s.total_input_tokens, s.total_output_tokens,
		        s.total_cache_write_tokens, s.total_cache_read_tokens,
		        s.total_cost_usd, s.cost_incomplete, s.visibility, s.public_id,
		        s.started_at, s.ended_at, s.updated_at,
		        s.user_id, s.project_id, p.remote_key, p.display_name, s.cwd, s.parent_session_id, s.parser_version
		   FROM sessions s
		   JOIN users u ON u.id = s.user_id
		   JOIN projects p ON p.id = s.project_id
		  WHERE `+where,
		arg).Scan(
		&d.ID, &d.Agent, &d.Machine, &d.GitBranch, &d.Username,
		&d.MessageCount, &d.UserMessageCount,
		&d.TotalInput, &d.TotalOutput, &d.TotalCacheWrite, &d.TotalCacheRead,
		&d.TotalCostUSD, &d.CostIncomplete, &d.Visibility, &d.PublicID,
		&d.StartedAt, &d.EndedAt, &d.UpdatedAt,
		&d.OwnerID, &d.ProjectID, &d.ProjectKey, &d.ProjectName, &d.Cwd, &d.ParentID, &d.ParserVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionDetail{}, ErrNotFound
	}
	return d, err
}

// SessionDetailByID loads a session by numeric id.
func (s *Store) SessionDetailByID(ctx context.Context, id int64) (SessionDetail, error) {
	return s.scanDetail(ctx, "s.id = $1", id)
}

// SessionDetailByPublicID loads a published session by its public id.
func (s *Store) SessionDetailByPublicID(ctx context.Context, publicID string) (SessionDetail, error) {
	return s.scanDetail(ctx, "s.public_id = $1 AND s.visibility = 'public'", publicID)
}

// Messages returns a session's transcript in order.
func (s *Store) Messages(ctx context.Context, sessionID int64) ([]Message, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT ordinal, role, content, thinking_text, model, has_thinking, has_tool_use, timestamp
		   FROM messages WHERE session_id = $1 ORDER BY ordinal`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Ordinal, &m.Role, &m.Content, &m.ThinkingText, &m.Model,
			&m.HasThinking, &m.HasToolUse, &m.Timestamp); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ToolCalls returns a session's tool calls as metadata.
func (s *Store) ToolCalls(ctx context.Context, sessionID int64) ([]ToolCallView, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT message_ordinal, call_index, tool_name, coalesce(category,''), coalesce(file_path,''),
		        coalesce(input_bytes,0), coalesce(input_media_type,''),
		        coalesce(result_bytes,0), coalesce(result_media_type,''), coalesce(result_status,'')
		   FROM tool_calls WHERE session_id = $1 ORDER BY message_ordinal, call_index`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolCallView
	for rows.Next() {
		var t ToolCallView
		if err := rows.Scan(&t.MessageOrdinal, &t.CallIndex, &t.ToolName, &t.Category, &t.FilePath,
			&t.InputBytes, &t.InputMediaType, &t.ResultBytes, &t.ResultMediaType, &t.ResultStatus); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Subagents returns sessions whose parent is the given session.
func (s *Store) Subagents(ctx context.Context, parentID int64) ([]SessionSummary, error) {
	rows, err := s.Pool.Query(ctx, sessionSelect+" WHERE s.parent_session_id = $1 ORDER BY s.id", parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		sm, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// FacetValues holds the distinct filter values available within a project's
// sessions, for populating the session-list filter dropdowns.
type FacetValues struct {
	Agents   []string
	Machines []string
	Users    []string
}

// SessionFacets returns the distinct agents, machines, and usernames present in a
// project's sessions, each sorted, for the project view's filter controls.
func (s *Store) SessionFacets(ctx context.Context, projectID int64) (FacetValues, error) {
	var f FacetValues
	rows, err := s.Pool.Query(ctx,
		`SELECT DISTINCT 'agent' AS kind, s.agent AS val FROM sessions s WHERE s.project_id = $1 AND s.agent <> ''
		 UNION
		 SELECT DISTINCT 'machine', s.machine FROM sessions s WHERE s.project_id = $1 AND s.machine <> ''
		 UNION
		 SELECT DISTINCT 'user', u.username FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.project_id = $1
		 ORDER BY kind, val`, projectID)
	if err != nil {
		return f, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, val string
		if err := rows.Scan(&kind, &val); err != nil {
			return f, err
		}
		switch kind {
		case "agent":
			f.Agents = append(f.Agents, val)
		case "machine":
			f.Machines = append(f.Machines, val)
		case "user":
			f.Users = append(f.Users, val)
		}
	}
	return f, rows.Err()
}

// Search finds messages whose content matches the query (trigram-accelerated
// substring match), optionally scoped to one project.
func (s *Store) Search(ctx context.Context, query string, projectID int64, limit int) ([]SearchHit, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args := []any{query}
	scope := ""
	if projectID != 0 {
		args = append(args, projectID)
		scope = " AND s.project_id = $2"
	}
	args = append(args, limit)
	rows, err := s.Pool.Query(ctx,
		`SELECT m.session_id, p.remote_key, s.agent, u.username, m.ordinal, m.role,
		        left(m.content, 240)
		   FROM messages m
		   JOIN sessions s ON s.id = m.session_id
		   JOIN projects p ON p.id = s.project_id
		   JOIN users u ON u.id = s.user_id
		  WHERE m.content ILIKE '%' || $1 || '%'`+scope+`
		  ORDER BY s.updated_at DESC NULLS LAST
		  LIMIT $`+itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.SessionID, &h.ProjectKey, &h.Agent, &h.Username, &h.Ordinal, &h.Role, &h.Snippet); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// itoa avoids strconv noise in query building.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
