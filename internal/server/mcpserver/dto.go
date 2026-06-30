package mcpserver

import (
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// The DTOs below are the stable, snake_case JSON shape of akari's data as agents
// see it. They deliberately do not reuse the store structs directly: the store
// types are tuned for the renderer (Go field names, four separate token columns),
// while these present one tokens object, omit empty context, and stay decoupled
// from internal renames. Each store type has a small mapper beside its DTO.

// tokens is the all-class token volume, the shape every token figure shares.
type tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Total      int64 `json:"total"`
}

func toks(in, out, cacheRead, cacheWrite int64) tokens {
	return tokens{Input: in, Output: out, CacheRead: cacheRead, CacheWrite: cacheWrite,
		Total: in + out + cacheRead + cacheWrite}
}

// --- whoami ---

type whoamiDTO struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
}

// --- overview ---

type overviewInput struct {
	Days    int     `json:"days,omitempty" jsonschema:"trailing window in days; 0 or omitted means all of history"`
	UserIDs []int64 `json:"user_ids,omitempty" jsonschema:"restrict to these account ids (from the users list); omitted means every user"`
}

type overviewDTO struct {
	Window    string       `json:"window"`
	Analytics analyticsDTO `json:"analytics"`
	Users     []userRefDTO `json:"users"`
}

type userRefDTO struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func usersToRefs(us []store.User) []userRefDTO {
	out := make([]userRefDTO, 0, len(us))
	for _, u := range us {
		out = append(out, userRefDTO{ID: u.ID, Username: u.Username})
	}
	return out
}

// --- analytics ---

type analyticsDTO struct {
	TotalCostUSD   float64        `json:"total_cost_usd"`
	CostIncomplete bool           `json:"cost_incomplete"`
	Tokens         tokens         `json:"tokens"`
	Sessions       int            `json:"sessions"`
	Series         []dayPointDTO  `json:"series"`
	Models         []breakdownDTO `json:"models"`
	Agents         []breakdownDTO `json:"agents"`
}

type dayPointDTO struct {
	Day        string  `json:"day"`
	CostUSD    float64 `json:"cost_usd"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
}

type breakdownDTO struct {
	Label          string  `json:"label"`
	CostUSD        float64 `json:"cost_usd"`
	CostIncomplete bool    `json:"cost_incomplete"`
	Tokens         tokens  `json:"tokens"`
	Sessions       int     `json:"sessions"`
}

func analyticsToDTO(a store.Analytics) analyticsDTO {
	d := analyticsDTO{
		TotalCostUSD:   a.TotalCost,
		CostIncomplete: a.CostIncomplete,
		Tokens:         toks(a.TotalIn, a.TotalOut, a.TotalCacheRead, a.TotalCacheWrite),
		Sessions:       a.Sessions,
		Series:         make([]dayPointDTO, 0, len(a.Series)),
		Models:         make([]breakdownDTO, 0, len(a.Models)),
		Agents:         make([]breakdownDTO, 0, len(a.Agents)),
	}
	for _, p := range a.Series {
		d.Series = append(d.Series, dayPointDTO{
			Day: p.Day.Format("2006-01-02"), CostUSD: p.CostUSD,
			Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite,
		})
	}
	for _, b := range a.Models {
		d.Models = append(d.Models, breakdownToDTO(b))
	}
	for _, b := range a.Agents {
		d.Agents = append(d.Agents, breakdownToDTO(b))
	}
	return d
}

func breakdownToDTO(b store.Breakdown) breakdownDTO {
	return breakdownDTO{
		Label: b.Label, CostUSD: b.CostUSD, CostIncomplete: b.CostIncomplete,
		Tokens: toks(b.Input, b.Output, b.CacheRead, b.CacheWrite), Sessions: b.Sessions,
	}
}

// --- projects ---

type projectsDTO struct {
	Projects []projectDTO `json:"projects"`
}

type projectDTO struct {
	ID             int64      `json:"id"`
	RemoteKey      string     `json:"remote_key"`
	Host           string     `json:"host,omitempty"`
	Owner          string     `json:"owner,omitempty"`
	Repo           string     `json:"repo,omitempty"`
	DisplayName    string     `json:"display_name"`
	Kind           string     `json:"kind"`
	SessionCount   int        `json:"session_count"`
	CostUSD        float64    `json:"cost_usd"`
	CostIncomplete bool       `json:"cost_incomplete"`
	Tokens         tokens     `json:"tokens"`
	LastActivity   *time.Time `json:"last_activity,omitempty"`
}

func projectToDTO(p store.ProjectSummary) projectDTO {
	return projectDTO{
		ID: p.ID, RemoteKey: p.RemoteKey, Host: p.Host, Owner: p.Owner, Repo: p.Repo,
		DisplayName: p.DisplayName, Kind: p.Kind, SessionCount: p.SessionCount,
		CostUSD: p.TotalCostUSD, CostIncomplete: p.CostIncomplete,
		Tokens:       toks(p.TotalInput, p.TotalOutput, p.TotalCacheRead, p.TotalCacheWrite),
		LastActivity: p.LastActivity,
	}
}

type getProjectInput struct {
	ProjectID int64  `json:"project_id" jsonschema:"the project id, from list_projects"`
	Days      int    `json:"days,omitempty" jsonschema:"trailing window in days for the analytics; 0 or omitted means all of history"`
	Agent     string `json:"agent,omitempty" jsonschema:"narrow the analytics to one agent"`
	Username  string `json:"username,omitempty" jsonschema:"narrow the analytics to one account name"`
	Machine   string `json:"machine,omitempty" jsonschema:"narrow the analytics to one machine"`
}

type projectDetailDTO struct {
	Project   projectDTO     `json:"project"`
	Window    string         `json:"window"`
	Analytics analyticsDTO   `json:"analytics"`
	Facets    facetValuesDTO `json:"facets"`
}

type facetValuesDTO struct {
	Agents   []string `json:"agents"`
	Machines []string `json:"machines"`
	Users    []string `json:"users"`
}

// --- sessions ---

type listSessionsInput struct {
	ProjectID int64  `json:"project_id,omitempty" jsonschema:"restrict to one project id"`
	Agent     string `json:"agent,omitempty" jsonschema:"restrict to one agent (claude, codex, pi)"`
	Username  string `json:"username,omitempty" jsonschema:"restrict to one account name"`
	Machine   string `json:"machine,omitempty" jsonschema:"restrict to one machine"`
	Days      int    `json:"days,omitempty" jsonschema:"restrict to sessions active within this many trailing days; 0 or omitted means all of history"`
	Sort      string `json:"sort,omitempty" jsonschema:"sort column: project, agent, branch, user, messages, tokens, or updated (default updated)"`
	Desc      bool   `json:"desc,omitempty" jsonschema:"sort descending"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max rows, up to 500 (default 100)"`
	Offset    int    `json:"offset,omitempty" jsonschema:"rows to skip, for paging"`
}

type sessionsDTO struct {
	Sessions []sessionDTO    `json:"sessions"`
	Facets   globalFacetsDTO `json:"facets"`
}

type sessionDTO struct {
	ID               int64      `json:"id"`
	Agent            string     `json:"agent"`
	Machine          string     `json:"machine,omitempty"`
	GitBranch        string     `json:"git_branch,omitempty"`
	Username         string     `json:"username"`
	MessageCount     int        `json:"message_count"`
	UserMessageCount int        `json:"user_message_count"`
	Tokens           tokens     `json:"tokens"`
	CostUSD          float64    `json:"cost_usd"`
	CostIncomplete   bool       `json:"cost_incomplete"`
	Visibility       string     `json:"visibility"`
	PublicID         *string    `json:"public_id,omitempty"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
	ProjectID        int64      `json:"project_id,omitempty"`
	ProjectKey       string     `json:"project_key,omitempty"`
	ProjectName      string     `json:"project_name,omitempty"`
	ProjectKind      string     `json:"project_kind,omitempty"`
}

func sessionSummaryToDTO(s store.SessionSummary) sessionDTO {
	return sessionDTO{
		ID: s.ID, Agent: s.Agent, Machine: s.Machine, GitBranch: s.GitBranch, Username: s.Username,
		MessageCount: s.MessageCount, UserMessageCount: s.UserMessageCount,
		Tokens:  toks(s.TotalInput, s.TotalOutput, s.TotalCacheRead, s.TotalCacheWrite),
		CostUSD: s.TotalCostUSD, CostIncomplete: s.CostIncomplete,
		Visibility: s.Visibility, PublicID: s.PublicID,
		StartedAt: s.StartedAt, EndedAt: s.EndedAt, UpdatedAt: s.UpdatedAt,
	}
}

func sessionRowToDTO(r store.SessionRow) sessionDTO {
	d := sessionSummaryToDTO(r.SessionSummary)
	d.ProjectID, d.ProjectKey, d.ProjectName, d.ProjectKind = r.ProjectID, r.ProjectKey, r.ProjectName, r.ProjectKind
	return d
}

type globalFacetsDTO struct {
	Agents   []facetCountDTO   `json:"agents"`
	Machines []facetCountDTO   `json:"machines"`
	Users    []facetCountDTO   `json:"users"`
	Projects []projectFacetDTO `json:"projects"`
}

type facetCountDTO struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type projectFacetDTO struct {
	ID    int64  `json:"id"`
	Key   string `json:"key"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

func globalFacetsToDTO(f store.GlobalFacetValues) globalFacetsDTO {
	d := globalFacetsDTO{
		Agents:   facetCounts(f.Agents),
		Machines: facetCounts(f.Machines),
		Users:    facetCounts(f.Users),
		Projects: make([]projectFacetDTO, 0, len(f.Projects)),
	}
	for _, p := range f.Projects {
		d.Projects = append(d.Projects, projectFacetDTO{ID: p.ID, Key: p.Key, Name: p.Name, Kind: p.Kind, Count: p.Count})
	}
	return d
}

func facetCounts(fs []store.FacetCount) []facetCountDTO {
	out := make([]facetCountDTO, 0, len(fs))
	for _, f := range fs {
		out = append(out, facetCountDTO{Value: f.Value, Count: f.Count})
	}
	return out
}

// --- session detail ---

type getSessionInput struct {
	SessionID         int64 `json:"session_id" jsonschema:"the session id, from list_sessions"`
	IncludeTranscript *bool `json:"include_transcript,omitempty" jsonschema:"include messages, tool calls, and attachments (default true); set false for just the header and counts"`
}

type sessionDetailDTO struct {
	sessionDTO
	OwnerID       int64  `json:"owner_id"`
	Cwd           string `json:"cwd,omitempty"`
	ParentID      *int64 `json:"parent_session_id,omitempty"`
	ParserVersion int    `json:"parser_version"`
	// DuplicateToolCallIDs counts tool-call ids that appear on more than one row, a
	// sign the transcript replayed a turn (a resumed or compacted run); normally 0.
	DuplicateToolCallIDs int             `json:"duplicate_tool_call_ids"`
	Messages             []messageDTO    `json:"messages,omitempty"`
	ToolCalls            []toolCallDTO   `json:"tool_calls,omitempty"`
	Attachments          []attachmentDTO `json:"attachments,omitempty"`
	Subagents            []sessionDTO    `json:"subagents,omitempty"`
}

func sessionDetailToDTO(d store.SessionDetail) sessionDetailDTO {
	out := sessionDetailDTO{
		sessionDTO:    sessionSummaryToDTO(d.SessionSummary),
		OwnerID:       d.OwnerID,
		Cwd:           d.Cwd,
		ParentID:      d.ParentID,
		ParserVersion: d.ParserVersion,
	}
	out.ProjectID, out.ProjectKey, out.ProjectName, out.ProjectKind = d.ProjectID, d.ProjectKey, d.ProjectName, d.ProjectKind
	return out
}

type messageDTO struct {
	Ordinal      int        `json:"ordinal"`
	Role         string     `json:"role"`
	Content      string     `json:"content"`
	ThinkingText string     `json:"thinking_text,omitempty"`
	Model        string     `json:"model,omitempty"`
	HasThinking  bool       `json:"has_thinking"`
	HasToolUse   bool       `json:"has_tool_use"`
	Timestamp    *time.Time `json:"timestamp,omitempty"`
}

func messageToDTO(m store.Message) messageDTO {
	return messageDTO{
		Ordinal: m.Ordinal, Role: m.Role, Content: m.Content, ThinkingText: m.ThinkingText,
		Model: m.Model, HasThinking: m.HasThinking, HasToolUse: m.HasToolUse, Timestamp: m.Timestamp,
	}
}

type toolCallDTO struct {
	MessageOrdinal  int    `json:"message_ordinal"`
	CallIndex       int    `json:"call_index"`
	ToolName        string `json:"tool_name"`
	Category        string `json:"category,omitempty"`
	FilePath        string `json:"file_path,omitempty"`
	InputSHA256     string `json:"input_sha256,omitempty"`
	InputBytes      int64  `json:"input_bytes,omitempty"`
	InputMediaType  string `json:"input_media_type,omitempty"`
	ResultSHA256    string `json:"result_sha256,omitempty"`
	ResultBytes     int64  `json:"result_bytes,omitempty"`
	ResultMediaType string `json:"result_media_type,omitempty"`
	ResultStatus    string `json:"result_status,omitempty"`
}

func toolCallToDTO(c store.ToolCallView) toolCallDTO {
	return toolCallDTO{
		MessageOrdinal: c.MessageOrdinal, CallIndex: c.CallIndex, ToolName: c.ToolName,
		Category: c.Category, FilePath: c.FilePath,
		InputSHA256: c.InputSHA, InputBytes: c.InputBytes, InputMediaType: c.InputMediaType,
		ResultSHA256: c.ResultSHA, ResultBytes: c.ResultBytes, ResultMediaType: c.ResultMediaType,
		ResultStatus: c.ResultStatus,
	}
}

type attachmentDTO struct {
	MessageOrdinal int    `json:"message_ordinal"`
	SHA256         string `json:"sha256"`
	MediaType      string `json:"media_type,omitempty"`
	ByteLen        int64  `json:"byte_len"`
	Filename       string `json:"filename,omitempty"`
}

func attachmentToDTO(a store.AttachmentView) attachmentDTO {
	return attachmentDTO{
		MessageOrdinal: a.MessageOrdinal, SHA256: a.SHA256, MediaType: a.MediaType,
		ByteLen: a.ByteLen, Filename: a.Filename,
	}
}

// --- bodies ---

type readBodyInput struct {
	SessionID int64  `json:"session_id" jsonschema:"a session that references the body (the gate the UI enforces)"`
	SHA256    string `json:"sha256" jsonschema:"the body's content hash, from a tool call's input_sha256 or result_sha256"`
	MaxBytes  int    `json:"max_bytes,omitempty" jsonschema:"cap on returned bytes (default 1048576, ceiling 8388608)"`
}

type bodyDTO struct {
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type,omitempty"`
	ByteLen   int64  `json:"byte_len"`
	Truncated bool   `json:"truncated"`
	Encoding  string `json:"encoding"`
	Content   string `json:"content"`
}

type rawInput struct {
	SessionID int64 `json:"session_id" jsonschema:"the session id"`
	MaxBytes  int   `json:"max_bytes,omitempty" jsonschema:"cap on returned bytes (default 1048576, ceiling 8388608)"`
}

type rawDTO struct {
	TotalBytes    int64  `json:"total_bytes"`
	BytesReturned int64  `json:"bytes_returned"`
	Truncated     bool   `json:"truncated"`
	Encoding      string `json:"encoding"`
	Content       string `json:"content"`
}
