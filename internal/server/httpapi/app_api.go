package httpapi

import (
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jssblck/akari/internal/guide"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/ogimage"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
	"github.com/jssblck/akari/internal/version"
)

type appViewer struct {
	Authenticated  bool   `json:"authenticated"`
	UserID         int64  `json:"user_id,omitempty"`
	Username       string `json:"username,omitempty"`
	IsAdmin        bool   `json:"is_admin"`
	OverviewPublic bool   `json:"overview_public"`
	CSRFToken      string `json:"csrf_token,omitempty"`
	// Version rides the bootstrap payload so the shell can show the running
	// server version the way the old templated sidebar did.
	Version string `json:"version"`
}

func (s *Server) handleAppBootstrap(w http.ResponseWriter, r *http.Request) {
	setPrivateNoStore(w)
	viewer := appViewer{Version: version.String()}
	if token, ok := csrfTokenFromRequest(r); ok {
		viewer.CSRFToken = token
	}
	p, ok := s.resolve(r)
	if !ok || p.Scope != scopeFull {
		writeJSON(w, http.StatusOK, viewer)
		return
	}
	u, err := s.Store.UserByID(r.Context(), p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load account")
		return
	}
	viewer.Authenticated = true
	viewer.UserID = u.ID
	viewer.Username = u.Username
	viewer.IsAdmin = u.IsAdmin
	viewer.OverviewPublic = u.OverviewPublic
	writeJSON(w, http.StatusOK, viewer)
}

func (s *Server) handleAPIOverview(w http.ResponseWriter, r *http.Request) {
	rng, ok := apiRange(w, r)
	if !ok {
		return
	}
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load users")
		return
	}
	selected := web.SelectedUserIDs(r.URL.Query()["user"], users)
	now := time.Now()
	analytics, err := s.Store.Analytics(r.Context(), store.AnalyticsFilter{
		Since: web.RangeSince(rng, now), Until: ogimage.DefaultUntil(now), UserIDs: selected,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load analytics")
		return
	}
	setDashboardCache(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"range": rng, "ranges": web.DateRanges, "users": users,
		"selected_user_ids": selected, "analytics": analytics,
	})
}

func (s *Server) handleAPIInsights(w http.ResponseWriter, r *http.Request) {
	rng, ok := apiRange(w, r)
	if !ok {
		return
	}
	insights, generatedAt, err := s.insights.get(r.Context(), rng)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load insights")
		return
	}
	setDashboardCache(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"range": rng, "ranges": web.DateRanges, "generated_at": generatedAt, "insights": insights,
	})
}

func (s *Server) handleAPIProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.Store.ListProjects(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load projects")
		return
	}
	sparklines, err := s.Store.ProjectSparklines(r.Context(), 30)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project trends")
		return
	}
	setDashboardCache(w)
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects, "sparklines": sparklines})
}

func (s *Server) handleAPIProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id", "project")
	if !ok {
		return
	}
	project, err := s.Store.Project(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project")
		return
	}
	rng, ok := apiRange(w, r)
	if !ok {
		return
	}
	now := time.Now()
	filter := store.SessionFilter{
		ProjectID: id, Agent: strings.TrimSpace(r.URL.Query().Get("agent")),
		Machine:  strings.TrimSpace(r.URL.Query().Get("machine")),
		Username: strings.TrimSpace(r.URL.Query().Get("user")),
		Since:    web.RangeSince(rng, now), IncludeEmpty: true,
	}
	page, err := s.Store.WindowSessionPage(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project sessions")
		return
	}
	facets, err := s.Store.SessionFacets(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project filters")
		return
	}
	analyticsFilter := store.AnalyticsFilter{
		ProjectID: id, Since: filter.Since, Until: ogimage.DefaultUntil(now),
		Username: filter.Username, Agent: filter.Agent, Machine: filter.Machine,
		Bucket: web.TrendBucket(rng),
	}
	analytics, err := s.Store.Analytics(r.Context(), analyticsFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project analytics")
		return
	}
	insights, err := s.Store.Insights(r.Context(), analyticsFilter, store.QualityBandPanels)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project insights")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": project, "range": rng, "ranges": web.DateRanges,
		"filter": filter, "sessions": page.Sessions, "remainder": page.Remainder,
		"facets": facets, "analytics": analytics, "insights": insights,
	})
}

// maxSearchQueryLen caps the content-search string before it becomes an ILIKE
// pattern, so a pasted multi-kilobyte query cannot drive a pathological scan. It is
// generous for any real search term.
const maxSearchQueryLen = 200

var sessionFilterQueryKeys = map[string]struct{}{
	"after": {}, "after_value": {}, "agent": {}, "dir": {}, "empty": {},
	"grade": {}, "limit": {}, "machine": {}, "outcome": {}, "project": {},
	"q": {}, "range": {}, "sort": {}, "spanned": {}, "subagents": {}, "user": {},
}

var apiSessionSortKeys = map[string]struct{}{
	"updated": {}, "tokens": {}, "messages": {}, "cost": {},
}

var apiGradeKeys = map[string]struct{}{
	"A": {}, "B": {}, "C": {}, "D": {}, "F": {}, "unscored": {},
}

var apiOutcomeKeys = map[string]struct{}{
	"completed": {}, "abandoned": {}, "errored": {}, "unknown": {},
}

var apiRangeKeys = func() map[string]struct{} {
	keys := make(map[string]struct{}, len(web.DateRanges))
	for _, dateRange := range web.DateRanges {
		keys[dateRange.Key] = struct{}{}
	}
	return keys
}()

func apiRange(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("range"))
	if raw == "" {
		return web.DefaultRange, true
	}
	if _, ok := apiRangeKeys[raw]; !ok {
		writeError(w, http.StatusBadRequest, "invalid range")
		return "", false
	}
	return raw, true
}

func apiSessionFilter(r *http.Request) (store.SessionFilter, error) {
	q := r.URL.Query()
	for key := range q {
		if _, ok := sessionFilterQueryKeys[key]; !ok {
			return store.SessionFilter{}, errors.New("unknown query parameter: " + key)
		}
	}
	includeEmpty, err := queryFlag(q.Get("empty"), "empty")
	if err != nil {
		return store.SessionFilter{}, err
	}
	includeSubagents, err := queryFlag(q.Get("subagents"), "subagents")
	if err != nil {
		return store.SessionFilter{}, err
	}
	requireSpan, err := queryFlag(q.Get("spanned"), "spanned")
	if err != nil {
		return store.SessionFilter{}, err
	}
	desc := true
	switch q.Get("dir") {
	case "", "desc":
	case "asc":
		desc = false
	default:
		return store.SessionFilter{}, errors.New("dir must be asc or desc")
	}
	f := store.SessionFilter{
		Agent: strings.TrimSpace(q.Get("agent")), Machine: strings.TrimSpace(q.Get("machine")),
		Username: strings.TrimSpace(q.Get("user")), IncludeEmpty: includeEmpty,
		IncludeSubagents: includeSubagents, RequireSpan: requireSpan,
		Grade: strings.TrimSpace(q.Get("grade")), Outcome: strings.TrimSpace(q.Get("outcome")),
		Sort: strings.TrimSpace(q.Get("sort")), Desc: desc,
	}
	if raw := strings.TrimSpace(q.Get("project")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			return f, errors.New("invalid project filter")
		}
		f.ProjectID = id
	}
	if query := strings.TrimSpace(q.Get("q")); query != "" {
		if len(query) > maxSearchQueryLen {
			cut := maxSearchQueryLen
			for cut > 0 && !utf8.RuneStart(query[cut]) {
				cut--
			}
			query = query[:cut]
		}
		f.Query = query
	}
	if f.Grade != "" {
		if _, ok := apiGradeKeys[f.Grade]; !ok {
			return f, errors.New("invalid grade filter")
		}
	}
	if f.Outcome != "" {
		if _, ok := apiOutcomeKeys[f.Outcome]; !ok {
			return f, errors.New("invalid outcome filter")
		}
	}
	if rng := strings.TrimSpace(q.Get("range")); rng != "" {
		if _, ok := apiRangeKeys[rng]; !ok {
			return f, errors.New("invalid range")
		}
		f.Range = rng
		if web.RangeBounds(rng) {
			f.Since = web.RangeSince(rng, time.Now())
		}
	}
	limit := web.DefaultSessionLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			return f, errors.New("limit must be between 1 and 200")
		}
		limit = n
	}
	f.Limit = limit
	if f.Sort != "" {
		if _, ok := apiSessionSortKeys[f.Sort]; !ok {
			return f, errors.New("invalid sort")
		}
	}
	if q.Get("after") == "" && q.Get("after_value") != "" {
		return f, errors.New("after_value requires after")
	}
	if raw := q.Get("after"); raw != "" {
		after, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 1 {
			return f, errors.New("invalid after cursor")
		}
		f.After = after
		f.AfterVal = q.Get("after_value")
		if f.AfterVal == "" {
			return f, errors.New("after_value is required with after")
		}
		if !validAPIKeysetValue(f.Sort, f.AfterVal) {
			return f, errors.New("invalid after value")
		}
	}
	return f, nil
}

func validAPIKeysetValue(sortKey, value string) bool {
	switch sortKey {
	case "", "updated":
		_, err := time.Parse(time.RFC3339Nano, value)
		return err == nil
	case "tokens", "messages":
		n, err := strconv.ParseInt(value, 10, 64)
		return err == nil && n >= 0
	case "cost":
		n, err := strconv.ParseFloat(value, 64)
		return err == nil && n >= 0 && !math.IsInf(n, 0) && !math.IsNaN(n)
	default:
		return false
	}
}

func queryFlag(value, name string) (bool, error) {
	switch value {
	case "":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, errors.New(name + " must be 1 when present")
	}
}

func (s *Server) handleAPISessions(w http.ResponseWriter, r *http.Request) {
	filter, err := apiSessionFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, hasMore, err := s.Store.ListAllSessions(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load sessions")
		return
	}
	facets, err := s.Store.GlobalFacets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load session filters")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": rows, "has_more": hasMore, "filter": filter, "facets": facets,
	})
}

func (s *Server) handleAPISession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id", "session")
	if !ok {
		return
	}
	snapshot, err := s.Store.SessionSnapshotByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load session")
		return
	}
	p, _ := principalFrom(r.Context())
	viewer, _ := s.Store.UserByID(r.Context(), p.UserID)
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot": snapshot, "owner": snapshot.Audit.Detail.OwnerID == p.UserID,
		"can_delete": snapshot.Audit.Detail.OwnerID == p.UserID || viewer.IsAdmin,
	})
}

func (s *Server) handleAPISessionEarlier(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id", "session")
	if !ok {
		return
	}
	before, err := strconv.Atoi(r.URL.Query().Get("before"))
	if err != nil || before < 0 {
		writeError(w, http.StatusBadRequest, "invalid before cursor")
		return
	}
	page, err := s.Store.TranscriptTail(r.Context(), id, &before)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load transcript")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"page": page})
}

func (s *Server) handleAPIAccount(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	user, err := s.Store.UserByID(r.Context(), p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load account")
		return
	}
	tokens, err := s.Store.ListAPITokens(r.Context(), p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load tokens")
		return
	}
	grants, err := s.Store.ListOAuthGrants(r.Context(), p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load connections")
		return
	}
	var invites []store.Invite
	if user.IsAdmin {
		invites, err = s.Store.ListInvites(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "load invites")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": appViewer{Authenticated: true, UserID: user.ID, Username: user.Username,
			IsAdmin: user.IsAdmin, OverviewPublic: user.OverviewPublic},
		"tokens": tokens, "connections": grants, "invites": invites,
		"reparse": s.worker.FleetStatus(r.Context()),
	})
}

func (s *Server) handleAPIGuide(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		slug = "index"
	}
	chapter, ok := guide.Lookup(slug)
	if !ok {
		writeError(w, http.StatusNotFound, "guide page not found")
		return
	}
	rendered, err := chapter.Render()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "render guide")
		return
	}
	raw, err := chapter.Raw()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load guide")
		return
	}
	chapters := guide.Chapters()
	writeJSON(w, http.StatusOK, map[string]any{
		"slug": chapter.Slug, "title": chapter.Title, "summary": chapter.Summary,
		"raw_markdown": raw, "headings": rendered.Headings,
		"raw_path": chapter.RawRoute(), "github_url": chapter.GitHubURL(), "chapters": chapters,
	})
}

func (s *Server) handleAPIPublicOverview(w http.ResponseWriter, r *http.Request) {
	user, err := s.Store.PublicOverviewUser(r.Context(), r.PathValue("username"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "overview not published")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load overview")
		return
	}
	if s.writeAPIReparseGate(w, r) {
		return
	}
	rng, ok := apiRange(w, r)
	if !ok {
		return
	}
	started := time.Now()
	snapshot, meta, err := s.analyticsSnapshots.get(r.Context(), analyticsSnapshotKey{
		scope: analyticsScope{kind: analyticsUserScope, id: user.ID}, rangeKey: rng,
	})
	if err != nil {
		status, respond := analyticsSnapshotErrorStatus(w, r, err)
		if respond {
			writeError(w, status, "load overview analytics")
		}
		return
	}
	observeAnalyticsSnapshot(w, started, meta, s.analyticsSnapshots.freshFor, s.analyticsSnapshots.staleFor)
	writeJSON(w, http.StatusOK, map[string]any{"username": user.Username, "range": rng, "ranges": web.DateRanges, "analytics": snapshot.analytics})
}

func (s *Server) handleAPIPublicProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id", "project")
	if !ok {
		return
	}
	project, err := s.Store.PublicProjectOverview(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "project overview not published")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project overview")
		return
	}
	if s.writeAPIReparseGate(w, r) {
		return
	}
	rng, ok := apiRange(w, r)
	if !ok {
		return
	}
	started := time.Now()
	snapshot, meta, err := s.analyticsSnapshots.get(r.Context(), analyticsSnapshotKey{
		scope: analyticsScope{kind: analyticsProjectScope, id: id}, rangeKey: rng,
	})
	if err != nil {
		status, respond := analyticsSnapshotErrorStatus(w, r, err)
		if respond {
			writeError(w, status, "load project analytics")
		}
		return
	}
	observeAnalyticsSnapshot(w, started, meta, s.analyticsSnapshots.freshFor, s.analyticsSnapshots.staleFor)
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "range": rng, "ranges": web.DateRanges, "analytics": snapshot.analytics, "insights": snapshot.insights})
}

func (s *Server) handleAPIPublicSession(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.SessionDetailByPublicID(r.Context(), r.PathValue("public_id")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not published")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "load session")
		return
	}
	if s.writeAPIReparseGate(w, r) {
		return
	}
	snapshot, err := s.Store.PublicSessionByID(r.Context(), r.PathValue("public_id"), nil)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not published")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot})
}

func (s *Server) handleAPIPublicSessionEarlier(w http.ResponseWriter, r *http.Request) {
	before, err := strconv.Atoi(r.URL.Query().Get("before"))
	if err != nil || before < 0 {
		writeError(w, http.StatusBadRequest, "invalid before cursor")
		return
	}
	if _, err := s.Store.SessionDetailByPublicID(r.Context(), r.PathValue("public_id")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not published")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "load session")
		return
	}
	if s.writeAPIReparseGate(w, r) {
		return
	}
	snapshot, err := s.Store.PublicSessionByID(r.Context(), r.PathValue("public_id"), &before)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not published")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot})
}

func (s *Server) writeAPIReparseGate(w http.ResponseWriter, r *http.Request) bool {
	status := s.worker.FleetStatus(r.Context())
	if !status.InProgress {
		return false
	}
	w.Header().Set("Retry-After", "2")
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": "projection rebuild in progress", "code": "projection_rebuild", "reparse": status,
	})
	return true
}

type publicationRequest struct {
	Published *bool `json:"published"`
}

func (s *Server) handleAPISessionPublication(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id", "session")
	if !ok {
		return
	}
	var req publicationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Published == nil {
		writeError(w, http.StatusBadRequest, "published is required")
		return
	}
	p, _ := principalFrom(r.Context())
	if *req.Published {
		candidate, err := auth.NewPublicID()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "generate public id")
			return
		}
		publicID, err := s.Store.PublishSession(r.Context(), id, p.UserID, candidate)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "publish session")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"published": true, "public_id": publicID})
		return
	}
	if err := s.Store.UnpublishSession(r.Context(), id, p.UserID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "unpublish session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"published": false})
}

func (s *Server) handleAPIDeleteSession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id", "session")
	if !ok {
		return
	}
	p, _ := principalFrom(r.Context())
	detail, err := s.Store.SessionDetailByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load session")
		return
	}
	if p.UserID != detail.OwnerID {
		user, err := s.Store.UserByID(r.Context(), p.UserID)
		if err != nil || !user.IsAdmin {
			writeError(w, http.StatusForbidden, "cannot delete this session")
			return
		}
	}
	if err := s.Store.DeleteSession(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "delete session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "project_id": detail.ProjectID})
}

func (s *Server) handleAPIProjectPublication(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id", "project")
	if !ok {
		return
	}
	var req publicationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Published == nil {
		writeError(w, http.StatusBadRequest, "published is required")
		return
	}
	var err error
	if *req.Published {
		err = s.Store.PublishProjectOverview(r.Context(), id)
	} else {
		err = s.Store.UnpublishProjectOverview(r.Context(), id)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update project publication")
		return
	}
	s.analyticsSnapshots.invalidate(analyticsScope{kind: analyticsProjectScope, id: id})
	writeJSON(w, http.StatusOK, map[string]bool{"published": *req.Published})
}

func (s *Server) handleAPIOverviewPublication(w http.ResponseWriter, r *http.Request) {
	var req publicationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Published == nil {
		writeError(w, http.StatusBadRequest, "published is required")
		return
	}
	p, _ := principalFrom(r.Context())
	var err error
	if *req.Published {
		err = s.Store.PublishOverview(r.Context(), p.UserID)
	} else {
		err = s.Store.UnpublishOverview(r.Context(), p.UserID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update overview publication")
		return
	}
	s.analyticsSnapshots.invalidate(analyticsScope{kind: analyticsUserScope, id: p.UserID})
	writeJSON(w, http.StatusOK, map[string]bool{"published": *req.Published})
}

func (s *Server) handleAPIRevokeConnection(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := s.Store.RevokeOAuthGrant(r.Context(), p.UserID, r.PathValue("client_id")); err != nil {
		writeError(w, http.StatusInternalServerError, "revoke connection")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (s *Server) handleAPIRevokeInvite(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id", "invite")
	if !ok {
		return
	}
	if err := s.Store.RevokeInvite(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "revoke invite")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (s *Server) handleAPIReparse(w http.ResponseWriter, r *http.Request) {
	if _, err := s.worker.Trigger(r.Context(), ""); err != nil {
		log.Printf("reparse trigger: %v", err)
		writeError(w, http.StatusInternalServerError, "trigger reparse")
		return
	}
	writeJSON(w, http.StatusAccepted, s.worker.FleetStatus(r.Context()))
}

func pathInt64(w http.ResponseWriter, r *http.Request, name, kind string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid "+kind+" id")
		return 0, false
	}
	return id, true
}
