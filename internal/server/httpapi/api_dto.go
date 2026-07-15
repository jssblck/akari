package httpapi

import (
	"time"

	"github.com/jssblck/akari/internal/guide"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
)

// These DTOs keep the browser contract owned by the HTTP package. Store and
// presentation read models remain nested when their JSON shape is intentional;
// each browser endpoint still has one named response boundary.
type appViewer struct {
	Authenticated  bool   `json:"authenticated"`
	UserID         int64  `json:"user_id,omitempty"`
	Username       string `json:"username,omitempty"`
	IsAdmin        bool   `json:"is_admin"`
	OverviewPublic bool   `json:"overview_public"`
	CSRFToken      string `json:"csrf_token,omitempty"`
	// Version rides the bootstrap payload so the shell can show the running
	// server version before any authenticated request completes.
	Version string `json:"version"`
}

type overviewResponse struct {
	Range           string            `json:"range"`
	Ranges          []web.DateRange   `json:"ranges"`
	Users           []overviewUserDTO `json:"users"`
	SelectedUserIDs []int64           `json:"selected_user_ids"`
	Analytics       store.Analytics   `json:"analytics"`
}

type overviewUserDTO struct {
	ID       int64  `json:"ID"`
	Username string `json:"Username"`
	IsAdmin  bool   `json:"IsAdmin"`
}

type insightsResponse struct {
	Range       string          `json:"range"`
	Ranges      []web.DateRange `json:"ranges"`
	GeneratedAt time.Time       `json:"generated_at"`
	Insights    store.Insights  `json:"insights"`
}

type projectsResponse struct {
	Projects   []store.ProjectSummary `json:"projects"`
	Sparklines map[int64][]float64    `json:"sparklines"`
}

type projectResponse struct {
	Project   store.ProjectSummary   `json:"project"`
	Range     string                 `json:"range"`
	Ranges    []web.DateRange        `json:"ranges"`
	Filter    store.SessionFilter    `json:"filter"`
	Sessions  []store.SessionSummary `json:"sessions"`
	Remainder store.SessionRemainder `json:"remainder"`
	Facets    store.FacetValues      `json:"facets"`
	Analytics store.Analytics        `json:"analytics"`
	Insights  store.Insights         `json:"insights"`
}

type sessionsResponse struct {
	Sessions []store.SessionRow      `json:"sessions"`
	HasMore  bool                    `json:"has_more"`
	Filter   store.SessionFilter     `json:"filter"`
	Facets   store.GlobalFacetValues `json:"facets"`
}

type sessionResponse struct {
	Snapshot  store.SessionSnapshot `json:"snapshot"`
	Owner     bool                  `json:"owner"`
	CanDelete bool                  `json:"can_delete"`
}

type transcriptResponse struct {
	Page store.TranscriptPage `json:"page"`
}

type accountResponse struct {
	User        appViewer          `json:"user"`
	Tokens      []accountTokenDTO  `json:"tokens"`
	Connections []oauthGrantDTO    `json:"connections"`
	Invites     []accountInviteDTO `json:"invites"`
	Reparse     parse.Status       `json:"reparse"`
}

type accountTokenDTO struct {
	ID         int64      `json:"ID"`
	Name       string     `json:"Name"`
	Scope      string     `json:"Scope"`
	CreatedAt  time.Time  `json:"CreatedAt"`
	LastUsedAt *time.Time `json:"LastUsedAt"`
	RevokedAt  *time.Time `json:"RevokedAt"`
}

type oauthGrantDTO struct {
	ClientID    string    `json:"ClientID"`
	ClientName  string    `json:"ClientName"`
	Scope       string    `json:"Scope"`
	ConnectedAt time.Time `json:"ConnectedAt"`
	LastUsedAt  time.Time `json:"LastUsedAt"`
}

type accountInviteDTO struct {
	ID         int64      `json:"ID"`
	Note       string     `json:"Note"`
	CreatedBy  string     `json:"CreatedBy"`
	CreatedAt  time.Time  `json:"CreatedAt"`
	ExpiresAt  *time.Time `json:"ExpiresAt"`
	RedeemedBy *string    `json:"RedeemedBy"`
	RedeemedAt *time.Time `json:"RedeemedAt"`
}

type guideResponse struct {
	Slug        string          `json:"slug"`
	Title       string          `json:"title"`
	Summary     string          `json:"summary"`
	RawMarkdown string          `json:"raw_markdown"`
	Headings    []guide.Heading `json:"headings"`
	RawPath     string          `json:"raw_path"`
	GitHubURL   string          `json:"github_url"`
	Chapters    []guide.Chapter `json:"chapters"`
}

type publicOverviewResponse struct {
	Username  string          `json:"username"`
	Range     string          `json:"range"`
	Ranges    []web.DateRange `json:"ranges"`
	Analytics store.Analytics `json:"analytics"`
}

type publicProjectResponse struct {
	Project   store.ProjectSummary `json:"project"`
	Range     string               `json:"range"`
	Ranges    []web.DateRange      `json:"ranges"`
	Analytics store.Analytics      `json:"analytics"`
	Insights  store.Insights       `json:"insights"`
}

type publicSessionResponse struct {
	Snapshot store.PublicSessionSnapshot `json:"snapshot"`
}

type oauthConsentResponse struct {
	ClientName    string `json:"client_name"`
	Username      string `json:"username"`
	ClientID      string `json:"client_id"`
	RedirectURI   string `json:"redirect_uri"`
	State         string `json:"state"`
	CodeChallenge string `json:"code_challenge"`
	Resource      string `json:"resource"`
	CSRF          string `json:"csrf"`
	AppCSRF       string `json:"app_csrf"`
}

type publicationResponse struct {
	Published bool `json:"published"`
}

type sessionPublicationResponse struct {
	Published bool   `json:"published"`
	PublicID  string `json:"public_id,omitempty"`
}

type deletedSessionResponse struct {
	Deleted   bool  `json:"deleted"`
	ProjectID int64 `json:"project_id"`
}

type revokedResponse struct {
	Revoked bool `json:"revoked"`
}

type reparseStatusResponse parse.Status

type projectionRebuildError string
type projectionRebuildCode string

const (
	projectionRebuildInProgress projectionRebuildError = "projection rebuild in progress"
	projectionRebuildCodeValue  projectionRebuildCode  = "projection_rebuild"
)

type reparseGateResponse struct {
	Error   projectionRebuildError `json:"error"`
	Code    projectionRebuildCode  `json:"code"`
	Reparse reparseStatusResponse  `json:"reparse"`
}

func newReparseGateResponse(status parse.Status) reparseGateResponse {
	return reparseGateResponse{
		Error:   projectionRebuildInProgress,
		Code:    projectionRebuildCodeValue,
		Reparse: reparseStatusResponse(status),
	}
}

type registeredUserResponse struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
}

type loginResponse struct {
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
}

type statusResponse struct {
	Status string `json:"status"`
}

type createdTokenResponse struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Scope string `json:"scope"`
	Token string `json:"token"`
}

type tokenListItem struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

type tokensResponse struct {
	Tokens []tokenListItem `json:"tokens"`
}

type createdInviteResponse struct {
	ID          int64      `json:"id"`
	Note        string     `json:"note"`
	InviteToken string     `json:"invite_token"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

func overviewUserDTOs(users []store.User) []overviewUserDTO {
	out := make([]overviewUserDTO, len(users))
	for i, user := range users {
		out[i] = overviewUserDTO{ID: user.ID, Username: user.Username, IsAdmin: user.IsAdmin}
	}
	return out
}

func accountTokenDTOs(tokens []store.APIToken) []accountTokenDTO {
	out := make([]accountTokenDTO, len(tokens))
	for i, token := range tokens {
		out[i] = accountTokenDTO{
			ID: token.ID, Name: token.Name, Scope: token.Scope,
			CreatedAt: token.CreatedAt, LastUsedAt: token.LastUsedAt, RevokedAt: token.RevokedAt,
		}
	}
	return out
}

func oauthGrantDTOs(grants []store.OAuthGrant) []oauthGrantDTO {
	out := make([]oauthGrantDTO, len(grants))
	for i, grant := range grants {
		out[i] = oauthGrantDTO{
			ClientID: grant.ClientID, ClientName: grant.ClientName, Scope: grant.Scope,
			ConnectedAt: grant.ConnectedAt, LastUsedAt: grant.LastUsedAt,
		}
	}
	return out
}

func accountInviteDTOs(invites []store.Invite) []accountInviteDTO {
	out := make([]accountInviteDTO, len(invites))
	for i, invite := range invites {
		out[i] = accountInviteDTO{
			ID: invite.ID, Note: invite.Note, CreatedBy: invite.CreatedBy, CreatedAt: invite.CreatedAt,
			ExpiresAt: invite.ExpiresAt, RedeemedBy: invite.RedeemedBy, RedeemedAt: invite.RedeemedAt,
		}
	}
	return out
}
