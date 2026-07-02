package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jssblck/akari/internal/pricing"
	"github.com/jssblck/akari/internal/quality"
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
	Kind         string
	SessionCount int
	TotalCostUSD float64
	TotalInput   int64
	TotalOutput  int64
	// Cache token rollups back the projects-index tokens column, whose hover
	// detail breaks the total into in, out, cache read, and cache write (the same
	// four classes the overview heatmap surfaces per day).
	TotalCacheRead  int64
	TotalCacheWrite int64
	// CostIncomplete is true when any session folded into this project's totals
	// carries an unpriced usage event, so the rolled-up cost is a lower bound. It
	// is the OR of the per-session cost_incomplete flags, letting the index render
	// the same "$X+" marker the per-session rows show instead of an exact figure
	// that silently understates an aggregate built from incomplete sessions.
	//
	// Like every other figure on this index, it is rollup-scoped (every surviving
	// usage row), so it can read true for a project whose only unpriced usage is
	// undated while the all-time analytics panel, which drops undated rows off its
	// time axis, reads exact. That is the one documented rollup-vs-analytics gap,
	// the same one the token and cost totals carry (see, in package store's tests,
	// TestUndatedUsageIsTheOnlyRollupAnalyticsGap): the flag tracks each surface's
	// own displayed total rather than diverging from it.
	CostIncomplete bool
	LastActivity   *time.Time
}

// TotalTokens is the sum of every token class for a project: input, output, and
// both cache directions. It is the headline figure for the projects-index tokens
// column, matching how the overview heatmap totals a day.
func (p ProjectSummary) TotalTokens() int64 {
	return p.TotalInput + p.TotalOutput + p.TotalCacheRead + p.TotalCacheWrite
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
	// Title is the session's first user message, squashed to single-spaced and
	// capped for display, so a row is recognizable by what the run was about rather
	// than only its metadata. It is empty for a session with no user message. The
	// figure is fetched by a LEFT JOIN LATERAL over the first ordinal user message,
	// so it rides the list read rather than a per-row lookup.
	Title string
}

// SessionDetail adds the owning project to a session summary, plus the rolled-up
// prompt-cache saving the session header's Cache tile renders. The saving lives only on
// the detail (not the list summary): the session list shows no cache tile, so the extra
// per-model-priced rollup rides only the single-session read that needs it.
type SessionDetail struct {
	SessionSummary
	OwnerID       int64
	ProjectID     int64
	ProjectKey    string
	ProjectName   string
	ProjectKind   string
	Cwd           string
	ParentID      *int64
	ParserVersion int
	// TotalCacheSavingsUSD is the session's rolled-up prompt-cache saving (folded at parse
	// time beside total_cost_usd), so the Cache tile reads it in O(1) instead of scanning
	// usage_events on every live refresh. A session whose rollup is not yet backfilled is
	// priced and persisted once on read (see scanDetail), so the tile is never served the
	// seeded value and later reads stay O(1). CacheSavingsIncomplete flags that some cached
	// volume rode an unpriced model, so the figure is partial (and, unlike cost, not a clean
	// lower bound: an omitted model's saving can be either sign).
	TotalCacheSavingsUSD   float64
	CacheSavingsIncomplete bool
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

// ToolCallView is one tool call rendered as metadata (the body lives in the CAS,
// fetched on demand by its sha256).
type ToolCallView struct {
	MessageOrdinal  int
	CallIndex       int
	ToolName        string
	Category        string
	FilePath        string
	InputSHA        string
	InputBytes      int64
	InputMediaType  string
	ResultSHA       string
	ResultBytes     int64
	ResultMediaType string
	ResultStatus    string
}

// SessionFilter narrows a session list. Empty fields are ignored.
type SessionFilter struct {
	ProjectID int64
	Agent     string
	Machine   string
	Username  string
	// Query, when set, restricts the list to sessions with at least one message
	// whose content matches it case-insensitively (an ILIKE substring, with the LIKE
	// metacharacters escaped so a literal % or _ in the query matches itself). It
	// drives the match from the messages side so the content trigram index serves it
	// (see ListAllSessions), and it also windows a per-row snippet around the first
	// match. The empty string is no content filter.
	Query string
	// IncludeEmpty keeps sessions whose parse produced no readable message
	// (message_count = 0) in the list. The default excludes them: they clutter the
	// global feed without carrying anything to read. Setting it restores the old
	// behavior of listing every session regardless of message count.
	IncludeEmpty bool
	// Since bounds the list to sessions last active at or after this instant,
	// matching the analytics window so a project page's session list and its usage
	// panel cover the same range. The zero time means no lower bound.
	Since time.Time
	// Grade narrows by the Insights Grades panel's buckets: a letter "A".."F" matches
	// sessions with a usable current-version signals row carrying that grade, and the
	// sentinel "unscored" matches the panel's catch-all (explicit NULL-grade row, stale
	// or non-current row, or no row at all). The empty string is no grade filter. See
	// conds() for the exact match.
	Grade string
	// Outcome narrows by the Outcomes panel's buckets: "completed", "abandoned", and
	// "errored" match a usable current-version signals row with that outcome, and
	// "unknown" matches the panel's catch-all (an explicit unknown row or a missing/
	// stale one). The empty string is no outcome filter. See conds().
	Outcome string
	// Range is the trailing-window key (a web.DateRanges key like "30d") that produced
	// Since, carried so the URL builder can re-emit ?range= and the chip can label the
	// window. It is display-and-URL state only: the query narrows by Since, never by
	// this string, so the store ignores it. The empty string means no windowing.
	Range string
	// Sort names the column the global session list is ordered by (see
	// sessionSortColumns). The empty string means DefaultSort. Desc selects
	// descending order. Together they back the click-to-sort table headers; an
	// unknown Sort falls back to DefaultSort in the query builder.
	Sort   string
	Desc   bool
	Limit  int
	Offset int
}

// conds builds the WHERE additions for the filter's narrowing fields, shared by
// every session-list query so a filter field added here reaches all of them at
// once. sinceCol names the timestamp column the Since bound applies to:
// s.updated_at for the whole-session lists, ue.occurred_at for the windowed
// queries that scope by dated usage. Placeholders start at $1; the caller
// appends its own (cursor, limit, offset) after.
//
// The content-search condition (Query) is expressed as an EXISTS over the
// messages table rather than a join, so a session with many matching messages
// still contributes one row and Postgres can drive the match from the messages
// content trigram index (idx_messages_content_trgm) then confirm the session by
// its primary key, instead of joining every matching message back into the list.
// The query is matched as a case-insensitive substring (ILIKE), with the LIKE
// metacharacters escaped so a literal % or _ typed into the box matches itself.
func (f SessionFilter) conds(sinceCol string) (conds []string, args []any) {
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
	if !f.IncludeEmpty {
		// A parse that produced no readable message leaves message_count = 0; those
		// rows clutter the global feed without carrying anything to open, so the
		// default hides them. This is a plain scalar comparison, no placeholder.
		conds = append(conds, "s.message_count > 0")
	}
	if q := f.Query; q != "" {
		args = append(args, likePattern(q))
		conds = append(conds, "EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id AND m.content ILIKE $"+itoa(len(args))+")")
	}
	if !f.Since.IsZero() {
		add(sinceCol+" >=", f.Since)
	}
	// Grade and outcome match the Insights distributions' definition exactly, so a
	// drill-through from a panel bar opens precisely the sessions that bar counted
	// (pinned bucket by bucket in TestDrillFiltersMatchQualityDistribution). The panel's
	// scan (qualityDistributionFrom in analytics_quality.go) LEFT JOINs the current-version,
	// non-stale signals row and coalesces a missing one into the catch-all bucket (grade ''
	// / outcome 'unknown'), so the buckets split two ways:
	//
	//   - A letter grade or a concrete outcome is an EXISTS: the session has a usable row
	//     (signals_version = quality.Version AND NOT s.signals_stale) carrying that value.
	//     Only NULL grades and missing rows fold in the panel, so EXISTS matches its
	//     letter and concrete-outcome bars exactly.
	//   - The catch-all buckets are the complement, a NOT EXISTS: "unscored" is no usable
	//     row with a non-NULL grade, which folds in the explicit NULL-grade row, the stale
	//     or non-current-version row, and the session with no row at all, the same three
	//     cases the panel test names (analytics_insights_test.go, "unscored = a4
	//     (explicit) + a6stale (stale row) + a7none (no row)"). "unknown" is likewise no
	//     usable row with a different outcome, folding the explicit 'unknown' row together
	//     with the missing-row cases.
	if g := f.Grade; g != "" {
		args = append(args, quality.Version)
		ver := "$" + itoa(len(args))
		if g == "unscored" {
			conds = append(conds, "NOT EXISTS (SELECT 1 FROM session_signals sig WHERE sig.session_id = s.id"+
				" AND sig.signals_version = "+ver+" AND NOT s.signals_stale AND sig.grade IS NOT NULL)")
		} else {
			args = append(args, g)
			conds = append(conds, "EXISTS (SELECT 1 FROM session_signals sig WHERE sig.session_id = s.id"+
				" AND sig.signals_version = "+ver+" AND NOT s.signals_stale AND sig.grade = $"+itoa(len(args))+")")
		}
	}
	if o := f.Outcome; o != "" {
		args = append(args, quality.Version)
		ver := "$" + itoa(len(args))
		args = append(args, o)
		if o == "unknown" {
			conds = append(conds, "NOT EXISTS (SELECT 1 FROM session_signals sig WHERE sig.session_id = s.id"+
				" AND sig.signals_version = "+ver+" AND NOT s.signals_stale AND sig.outcome <> $"+itoa(len(args))+")")
		} else {
			conds = append(conds, "EXISTS (SELECT 1 FROM session_signals sig WHERE sig.session_id = s.id"+
				" AND sig.signals_version = "+ver+" AND NOT s.signals_stale AND sig.outcome = $"+itoa(len(args))+")")
		}
	}
	return conds, args
}

// likePattern wraps a user's search string in the wildcards that make it a
// case-insensitive substring match, escaping the LIKE metacharacters (%, _, and
// the escape character itself) so a literal one the user typed matches itself
// rather than acting as a wildcard. The default backslash escape applies, so a
// backslash in the query is doubled here.
func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// DefaultSort is the global session list's order when none is requested: most
// recently active first, matching the feed's "find a recent run" purpose.
const DefaultSort = "updated"

// sessionSortColumns whitelists each sortable column of the global session list,
// mapping the key that arrives on the query string to its ORDER BY expression.
// The map is the trust boundary: a key absent here never reaches the SQL, so the
// expression can be interpolated without risking injection.
//
// agent, branch, messages, tokens, and cost are session-local and index-walked
// (see migrations 0014 and 0028): each has a (col, id) btree that, paired with
// the direction-following id tiebreak in orderClause, satisfies the ORDER BY ...
// LIMIT with an index scan rather than sorting the whole history. tokens reads the
// stored generated total_tokens column for the same reason. project and user sort on
// joined tables (the projects CASE, users.username), which a session index cannot
// walk; they rank the working set, cheap when a facet filter narrows it and a full
// sort only on the unfiltered whole-history case.
var sessionSortColumns = map[string]string{
	"project":  "CASE WHEN p.kind IN ('standalone', 'orphaned') THEN p.display_name ELSE p.remote_key END",
	"agent":    "s.agent",
	"branch":   "s.git_branch",
	"user":     "u.username",
	"messages": "s.message_count",
	"tokens":   "s.total_tokens",
	"cost":     "s.total_cost_usd",
	"updated":  "s.updated_at",
}

// IsSortKey reports whether key names a sortable column of the global session
// list, so the handler can reject an unknown or tampered sort param before it
// reaches the query builder.
func IsSortKey(key string) bool {
	_, ok := sessionSortColumns[key]
	return ok
}

// orderClause builds the ORDER BY for the global session list from the filter's
// Sort and Desc. An empty or unknown Sort is the default order, most recent
// first: callers that pass a bare filter (the overview feed, the project page)
// expect newest-first, and a bool Desc cannot distinguish "explicitly ascending"
// from its zero value, so the default is forced descending rather than routed
// through Desc.
//
// The session id is always the tiebreaker, so ties order deterministically and
// pagination stays stable for a given sort and direction. The tiebreak follows
// the column's direction (id ASC under an ascending sort, id DESC under a
// descending one) so a single (col, id) btree serves both orders: a forward scan
// satisfies "col ASC, id ASC" and a backward scan "col DESC, id DESC". A fixed
// id DESC tiebreak would leave the ascending case unable to walk the index and
// force a sort. The default order is descending, so its tiebreak stays id DESC,
// matching the feed indexes (0004, 0006) exactly.
//
// No NULLS LAST: every sortable expression is NOT NULL (the session columns, the
// generated total_tokens, the username join, and the project CASE over two NOT
// NULL columns all are), so a nulls placement would never change a result. It
// would, however, defeat the index: Postgres only matches an ORDER BY to a btree
// when the nulls placement agrees, and a DESC btree defaults to NULLS FIRST, so a
// "DESC NULLS LAST" clause forces a full sort instead of an index scan. Omitting
// it lets the (col, id) and feed indexes satisfy the order directly.
func (f SessionFilter) orderClause() string {
	expr, ok := sessionSortColumns[f.Sort]
	desc := f.Desc
	if !ok {
		expr, desc = sessionSortColumns[DefaultSort], true
	}
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	return fmt.Sprintf(" ORDER BY %s %s, s.id %s", expr, dir, dir)
}

// SessionRow is one row of the global (cross-project) session list: a session
// summary plus the project it ran in, so a reader can scan and filter every
// session in one place without first choosing a project.
type SessionRow struct {
	SessionSummary
	ProjectID   int64
	ProjectKey  string
	ProjectName string
	ProjectKind string
	// Search is the content-match snippet for this row, populated only when the
	// list was run with a Query filter: a window of the first matching message's
	// content centered on the match, so the feed can show what the session said
	// around the search term. It is the zero value on an unfiltered list.
	Search SearchSnippet
}

// SearchSnippet is a window of a matching message's content around the first
// occurrence of the search query, with the match's offsets within the window so a
// renderer can wrap exactly the matched run in a highlight without re-scanning.
// The window is computed in Go from the raw matching content (fetched by a lateral
// join in the search query) rather than in SQL, so the offsets are byte positions
// into Text and stay exact for the template's three-part split (before, match,
// after). A zero-value snippet (empty Text) means no match was carried.
type SearchSnippet struct {
	// Text is the trimmed content window, with leading/trailing ellipses already
	// applied when the window was cut from a longer message.
	Text string
	// MatchStart and MatchEnd are the byte offsets of the matched run within Text,
	// so MatchStart <= MatchEnd <= len(Text). They are equal (both zero) when Text
	// is empty.
	MatchStart int
	MatchEnd   int
}

// Has reports whether the snippet carries a match, so a renderer can choose the
// snippet line over the title line only when a search actually produced one.
func (s SearchSnippet) Has() bool { return s.Text != "" }

// ListProjects returns every project with rolled-up stats, most recently active
// first.
func (s *Store) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT p.id, p.remote_key, p.host, p.owner, p.repo, p.display_name, p.kind,
		        count(s.id),
		        coalesce(sum(s.total_cost_usd), 0),
		        coalesce(sum(s.total_input_tokens), 0),
		        coalesce(sum(s.total_output_tokens), 0),
		        coalesce(sum(s.total_cache_read_tokens), 0),
		        coalesce(sum(s.total_cache_write_tokens), 0),
		        coalesce(bool_or(s.cost_incomplete), false),
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
		if err := rows.Scan(&p.ID, &p.RemoteKey, &p.Host, &p.Owner, &p.Repo, &p.DisplayName, &p.Kind,
			&p.SessionCount, &p.TotalCostUSD, &p.TotalInput, &p.TotalOutput,
			&p.TotalCacheRead, &p.TotalCacheWrite, &p.CostIncomplete, &p.LastActivity); err != nil {
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
		`SELECT id, remote_key, host, owner, repo, display_name, kind FROM projects WHERE id = $1`, id).
		Scan(&p.ID, &p.RemoteKey, &p.Host, &p.Owner, &p.Repo, &p.DisplayName, &p.Kind)
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
	conds, args := f.conds("s.updated_at")

	q := sessionSelect
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	// No NULLS LAST: updated_at is NOT NULL, so it cannot change the result, but it
	// would stop Postgres from matching this order to the feed indexes (0004, 0006)
	// and force a full sort instead of an index walk to the LIMIT. See orderClause.
	q += " ORDER BY s.updated_at DESC, s.id DESC"
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

// windowSessionLimit is the cap on how many session rows the project page renders.
// Past it the table would grow with a project's whole windowed history, so the rows
// stop here and SessionPage.Remainder accounts for the rest.
const windowSessionLimit = 100

// SessionPage is a project's windowed session table: the capped rows the page shows,
// newest-active first, plus the aggregate of every windowed session that did not fit
// (Remainder). Shown rows plus Remainder reproduce the usage panel's headline, since
// both the rows and the remainder derive from the one dated-usage base the panel sums.
type SessionPage struct {
	Sessions  []SessionSummary
	Remainder SessionRemainder
}

// SessionRemainder is the aggregate of the windowed sessions the capped table did not
// show: how many, their per-class token volume, their summed cost, and whether any of
// them carried unpriced usage. The project page renders it as a footer so the visible
// rows plus this line reconcile with the usage panel headline even when more sessions
// match than the table caps at. It carries all four token classes (not just a total)
// so the footer can show the same breakdown card every other token figure does, and
// its CostIncomplete is a bool_or over the hidden sessions alone, so the footer flags
// "$X+" only when a hidden session is the unpriced one, never because a visible row was.
type SessionRemainder struct {
	Sessions       int
	Input          int64
	Output         int64
	CacheRead      int64
	CacheWrite     int64
	CostUSD        float64
	CostIncomplete bool
}

// Has reports whether any windowed sessions fell outside the capped table, so the
// project page shows the reconciling footer only when rows were actually withheld.
func (r SessionRemainder) Has() bool { return r.Sessions > 0 }

// Tokens is the hidden tail's all-class token volume, the figure the footer's total
// shows with the per-class split behind its card, matching every other token readout.
func (r SessionRemainder) Tokens() int64 {
	return r.Input + r.Output + r.CacheRead + r.CacheWrite
}

// windowSessionConds builds the shared WHERE additions for the windowed-session
// queries (the capped rows and the remainder aggregate), so both narrow by the exact
// same project, window, agent, user, and machine scope the usage panel does: the
// standard filter conditions with the window bound applied to dated usage
// (ue.occurred_at) rather than session activity. The placeholders start at $1; the
// caller appends its own (limit, offset) after.
func windowSessionConds(f SessionFilter) ([]string, []any) {
	return f.conds("ue.occurred_at")
}

// WindowSessionPage returns the project page's session table: the capped rows that
// contributed dated usage inside the filter's window, each carrying its in-window
// token and cost sums rather than its all-time rollup, plus the aggregate of the
// windowed sessions beyond the cap. It shares the analytics base exactly (the same
// dated usage_events under the same project, window, agent, user, and machine scope,
// grouped per session instead of summed whole), which is what lets the rows be a
// partition of the usage panel's headline where the lifetime rollups ListSessions
// returns would overcount a session whose usage predates the window.
//
// The cap keeps a project with thousands of windowed sessions from rendering an
// unbounded table. So the visible rows alone need not sum to the headline; the
// remainder closes the gap. It is queried, not subtracted from the panel: a boolean
// OR like cost_incomplete cannot be undone by subtraction (a visible unpriced row
// would wrongly mark the priced tail incomplete), so the tail is aggregated directly
// over the hidden sessions, carrying its own per-class sums, cost, and bool_or flag.
// The remainder query runs only when the cap actually engaged.
func (s *Store) WindowSessionPage(ctx context.Context, f SessionFilter) (SessionPage, error) {
	conds, args := windowSessionConds(f)
	where := "ue.occurred_at IS NOT NULL"
	if len(conds) > 0 {
		where += " AND " + strings.Join(conds, " AND ")
	}

	// The Title lateral rides the same grouped read (a correlated LIMIT 1 over the
	// first user message), added to the GROUP BY so the aggregate stays well-formed.
	// It matches the global feed's title so a session reads the same on both surfaces.
	q := `
		SELECT s.id, s.agent, s.machine, s.git_branch, u.username,
		       s.message_count, s.user_message_count,
		       coalesce(sum(ue.input_tokens), 0), coalesce(sum(ue.output_tokens), 0),
		       coalesce(sum(ue.cache_write_tokens), 0), coalesce(sum(ue.cache_read_tokens), 0),
		       coalesce(sum(ue.cost_usd), 0), ` + costIncompleteExpr + `,
		       s.visibility, s.public_id, s.started_at, s.ended_at, s.updated_at,
		       coalesce(title.content, '')
		  FROM usage_events ue
		  JOIN sessions s ON s.id = ue.session_id
		  JOIN users u ON u.id = s.user_id
		  LEFT JOIN LATERAL (
		         SELECT left(m.content, ` + itoa(titleCap) + `) AS content
		           FROM messages m
		          WHERE m.session_id = s.id AND m.role = 'user'
		          ORDER BY m.ordinal LIMIT 1
		       ) title ON true
		 WHERE ` + where + `
		 GROUP BY s.id, u.username, title.content
		 ORDER BY s.updated_at DESC, s.id DESC
		 LIMIT $` + itoa(len(args)+1)
	rowArgs := append(append([]any{}, args...), windowSessionLimit)

	rows, err := s.Pool.Query(ctx, q, rowArgs...)
	if err != nil {
		return SessionPage{}, fmt.Errorf("query window sessions: %w", err)
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var sm SessionSummary
		if err := rows.Scan(&sm.ID, &sm.Agent, &sm.Machine, &sm.GitBranch, &sm.Username,
			&sm.MessageCount, &sm.UserMessageCount,
			&sm.TotalInput, &sm.TotalOutput, &sm.TotalCacheWrite, &sm.TotalCacheRead,
			&sm.TotalCostUSD, &sm.CostIncomplete, &sm.Visibility, &sm.PublicID,
			&sm.StartedAt, &sm.EndedAt, &sm.UpdatedAt, &sm.Title); err != nil {
			return SessionPage{}, fmt.Errorf("scan window session: %w", err)
		}
		sm.Title = squashSpaces(sm.Title)
		out = append(out, sm)
	}
	if err := rows.Err(); err != nil {
		return SessionPage{}, fmt.Errorf("iterate window sessions: %w", err)
	}

	page := SessionPage{Sessions: out}
	// The remainder exists only when the table filled to its cap; below it, every
	// windowed session already shows and the footer would be empty.
	if len(out) == windowSessionLimit {
		rem, err := s.windowSessionRemainder(ctx, where, args)
		if err != nil {
			return SessionPage{}, err
		}
		page.Remainder = rem
	}
	return page, nil
}

// windowSessionRemainder aggregates the windowed sessions past the cap into the
// footer's totals. It re-derives the same per-session grouping WindowSessionPage
// shows, orders it identically, skips the rows already shown (OFFSET the cap), and
// sums what is left: per-class tokens, cost, the count, and a bool_or over those
// sessions' own incompleteness so the marker reflects the hidden tail, not the panel.
func (s *Store) windowSessionRemainder(ctx context.Context, where string, args []any) (SessionRemainder, error) {
	q := `
		SELECT coalesce(count(*), 0),
		       coalesce(sum(t.input), 0), coalesce(sum(t.output), 0),
		       coalesce(sum(t.cache_read), 0), coalesce(sum(t.cache_write), 0),
		       coalesce(sum(t.cost), 0), coalesce(bool_or(t.incomplete), false)
		  FROM (
		       SELECT coalesce(sum(ue.input_tokens), 0) AS input,
		              coalesce(sum(ue.output_tokens), 0) AS output,
		              coalesce(sum(ue.cache_read_tokens), 0) AS cache_read,
		              coalesce(sum(ue.cache_write_tokens), 0) AS cache_write,
		              coalesce(sum(ue.cost_usd), 0) AS cost,
		              ` + costIncompleteExpr + ` AS incomplete
		         FROM usage_events ue
		         JOIN sessions s ON s.id = ue.session_id
		         JOIN users u ON u.id = s.user_id
		        WHERE ` + where + `
		        GROUP BY s.id
		        ORDER BY s.updated_at DESC, s.id DESC
		        OFFSET $` + itoa(len(args)+1) + `
		  ) t`
	remArgs := append(append([]any{}, args...), windowSessionLimit)
	var r SessionRemainder
	if err := s.Pool.QueryRow(ctx, q, remArgs...).Scan(
		&r.Sessions, &r.Input, &r.Output, &r.CacheRead, &r.CacheWrite, &r.CostUSD, &r.CostIncomplete,
	); err != nil {
		return SessionRemainder{}, fmt.Errorf("aggregate window session remainder: %w", err)
	}
	return r, nil
}

// globalSessionSelect is the column list and joins for cross-project session
// rows: the same session columns as sessionSelect, plus the owning project's
// identity so the list can show and link a project per row, and two lateral
// lookups off the messages table.
//
// The Title lateral is always present: the session's first user message, capped
// in SQL to a bounded prefix (left(...)) so a giant opening prompt never pulls its
// whole content into the row, then squashed to single-spaced in Go. It is a
// correlated LEFT JOIN LATERAL ordered by ordinal with LIMIT 1, so it walks the
// messages (session_id, ordinal) primary key straight to the first user turn
// rather than scanning the session's transcript.
//
// The match lateral (%[1]s) is spliced in only when a content search is active:
// it fetches the first matching message's content (again LIMIT 1 by ordinal) so
// SearchSnippet can window it in Go. It carries the same escaped ILIKE pattern the
// EXISTS filter in conds() uses, passed as the placeholder the caller records.
// When no search is active the format arg is empty and a NULL literal fills the
// column so the row shape stays fixed.
func globalSessionSelect(matchLateral, matchCol string) string {
	return `
	SELECT s.id, s.agent, s.machine, s.git_branch, u.username,
	       s.message_count, s.user_message_count,
	       s.total_input_tokens, s.total_output_tokens,
	       s.total_cache_write_tokens, s.total_cache_read_tokens,
	       s.total_cost_usd, s.cost_incomplete, s.visibility, s.public_id,
	       s.started_at, s.ended_at, s.updated_at,
	       p.id, p.remote_key, p.display_name, p.kind,
	       coalesce(title.content, ''), ` + matchCol + `
	  FROM sessions s
	  JOIN users u ON u.id = s.user_id
	  JOIN projects p ON p.id = s.project_id
	  LEFT JOIN LATERAL (
	         SELECT left(m.content, ` + itoa(titleCap) + `) AS content
	           FROM messages m
	          WHERE m.session_id = s.id AND m.role = 'user'
	          ORDER BY m.ordinal LIMIT 1
	       ) title ON true` + matchLateral
}

// titleCap bounds the first-user-message prefix the Title column carries. 240
// characters is well past what any row can display (CSS ellipsizes it far sooner),
// so the cap only guards against pulling a multi-kilobyte opening prompt into
// every row while leaving the visible portion intact.
const titleCap = 240

// scanSessionRow reads one cross-project row. matchArgActive reports whether the
// query selected the match-content column (a live content search), so the scan
// targets an extra nullable string only then and the row shape matches the SELECT.
// The raw match content stays local: the caller windows it into the row's snippet.
func scanSessionRow(rows pgx.Rows, matchActive bool) (SessionRow, string, error) {
	var r SessionRow
	var match *string
	dest := []any{&r.ID, &r.Agent, &r.Machine, &r.GitBranch, &r.Username,
		&r.MessageCount, &r.UserMessageCount,
		&r.TotalInput, &r.TotalOutput, &r.TotalCacheWrite, &r.TotalCacheRead,
		&r.TotalCostUSD, &r.CostIncomplete, &r.Visibility, &r.PublicID,
		&r.StartedAt, &r.EndedAt, &r.UpdatedAt,
		&r.ProjectID, &r.ProjectKey, &r.ProjectName, &r.ProjectKind,
		&r.Title, &match}
	if err := rows.Scan(dest...); err != nil {
		return r, "", fmt.Errorf("scan global session row: %w", err)
	}
	r.Title = squashSpaces(r.Title)
	var raw string
	if matchActive && match != nil {
		raw = *match
	}
	return r, raw, nil
}

// ListAllSessions returns sessions across every project matching the filter,
// newest first. A zero ProjectID means "all projects"; the other fields narrow
// the set exactly as ListSessions does. This backs the global Sessions view and
// the Overview's recent-activity feed.
//
// Every row carries its first-user-message Title. When Query is set, each row also
// carries a SearchSnippet windowed around the first match, computed in Go from the
// first matching message's content (fetched by the match lateral) so the offsets
// are exact byte positions the template can split on.
func (s *Store) ListAllSessions(ctx context.Context, f SessionFilter) ([]SessionRow, error) {
	conds, args := f.conds("s.updated_at")

	// The match lateral needs the same escaped ILIKE pattern the EXISTS filter in
	// conds() built. conds() already appended that pattern as the last arg when Query
	// is set, so reuse its placeholder rather than binding the pattern twice.
	matchLateral, matchCol := "", "NULL"
	searching := f.Query != ""
	if searching {
		patternPlaceholder := "$" + itoa(len(args))
		matchLateral = `
	  LEFT JOIN LATERAL (
	         SELECT m.content
	           FROM messages m
	          WHERE m.session_id = s.id AND m.content ILIKE ` + patternPlaceholder + `
	          ORDER BY m.ordinal LIMIT 1
	       ) match ON true`
		matchCol = "match.content"
	}

	q := globalSessionSelect(matchLateral, matchCol)
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += f.orderClause()
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
		return nil, fmt.Errorf("query global sessions: %w", err)
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		r, raw, err := scanSessionRow(rows, searching)
		if err != nil {
			return nil, err
		}
		if searching {
			r.Search = buildSnippet(raw, f.Query)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate global sessions: %w", err)
	}
	return out, nil
}

// CountAllSessions counts the sessions ListAllSessions would return for the same
// filter (ignoring Limit and Offset), plus how many empty sessions the default
// hides. It shares conds() with ListAllSessions so the two can never disagree
// about which rows match, and the footer's "N of M" and the empty-hidden toggle
// both read from this one round trip.
//
// The empty count is a FILTER aggregate over the same matched set, so it reflects
// the sessions hidden BY the empty filter within the current agent/project/query
// scope, not a fleet-wide zero-message count. When IncludeEmpty is set the empty
// rows are already in Total, and Empty reports how many of them are the
// zero-message ones (so the toggle can read "showing empty").
func (s *Store) CountAllSessions(ctx context.Context, f SessionFilter) (total, empty int, err error) {
	// Count over the same match set as the list, but neutralize the empty-hiding so
	// the FILTER can report the hidden count regardless of the current toggle: run
	// conds with IncludeEmpty forced on, then let the FILTER split the empty ones out.
	cf := f
	cf.IncludeEmpty = true
	conds, args := cf.conds("s.updated_at")

	// A content search still needs the messages EXISTS from conds(); no lateral or
	// ordering is needed for a count, so the query is just the filtered aggregate.
	q := `SELECT count(*), count(*) FILTER (WHERE s.message_count = 0)
	        FROM sessions s
	        JOIN users u ON u.id = s.user_id
	        JOIN projects p ON p.id = s.project_id`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	if err := s.Pool.QueryRow(ctx, q, args...).Scan(&total, &empty); err != nil {
		return 0, 0, fmt.Errorf("count global sessions: %w", err)
	}
	// When the caller hides empties (the default), the total it will show excludes
	// them, matching the list. Report that narrowed total so "N of M" reconciles.
	if !f.IncludeEmpty {
		total -= empty
	}
	return total, empty, nil
}

// SessionFeedCursor marks a position in the session feed: the id of the last row a
// page returned. The feed pages on the session id, which is immutable, rather than on
// updated_at. That matters for a client paging the whole feed across separate
// requests: a session re-activated mid-walk (its updated_at bumped by ingest or a
// re-parse) keeps the same id, so it never jumps past the cursor and drops out of a
// later page the way an updated_at keyset would silently skip it. Paging stays O(N),
// not the O(N^2) an OFFSET scan would cost.
type SessionFeedCursor struct {
	ID int64
}

// SessionFeed returns one page of the cross-project feed (newest session first) and
// the cursor for the page after it (nil when this page is the last). It applies the
// same filters as ListAllSessions but pages by keyset on the immutable session id
// descending rather than OFFSET: each page resumes from the prior page's last id and
// reads only the next `limit` rows, so a client paging the whole feed never re-skips
// the rows it already saw and a concurrent updated_at bump cannot drop a row from the
// walk. Each row still carries its updated_at, so a caller can order by recency within
// a page. limit is clamped to [1, 500] (default 100).
func (s *Store) SessionFeed(ctx context.Context, f SessionFilter, limit int, cursor *SessionFeedCursor) ([]SessionRow, *SessionFeedCursor, error) {
	conds, args := f.conds("s.updated_at")
	if cursor != nil {
		// The next page is everything below the cursor in id-descending order. id is the
		// primary key, so Postgres walks the PK index straight to the resume point rather
		// than sorting or skipping, and the bound is on an immutable column so no row can
		// slip past it between pages.
		args = append(args, cursor.ID)
		conds = append(conds, "s.id < $"+itoa(len(args)))
	}

	// The keyset feed does not carry a content search, so no match lateral: the
	// Title lateral rides along (it is cheap and lets a caller show a title) and the
	// match column is a NULL literal to keep the row shape fixed.
	q := globalSessionSelect("", "NULL")
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY s.id DESC"
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// Fetch one extra row to learn whether a further page exists without a COUNT.
	args = append(args, limit+1)
	q += " LIMIT $" + itoa(len(args))

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("query session feed: %w", err)
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		r, _, err := scanSessionRow(rows, false)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate session feed: %w", err)
	}

	var next *SessionFeedCursor
	if len(out) > limit {
		out = out[:limit]
		next = &SessionFeedCursor{ID: out[limit-1].ID}
	}
	return out, next, nil
}

// FacetCount is one filter value and how many sessions carry it, for a faceted
// filter rail that shows counts beside each option.
type FacetCount struct {
	Value string
	Count int
}

// ProjectFacet is one project option in the global session filter: enough to
// label, color, and link it, plus its session count.
type ProjectFacet struct {
	ID    int64
	Key   string
	Name  string
	Kind  string
	Count int
}

// GlobalFacetValues holds the filter options for the cross-project session view,
// each with a count so the rail reads like an instrument.
type GlobalFacetValues struct {
	Agents   []FacetCount
	Machines []FacetCount
	Users    []FacetCount
	Projects []ProjectFacet
}

// facetLimit caps each facet category to its busiest values, so the rail (and
// the memory backing it) stays a fixed top-N rather than growing with the total
// distinct agents, machines, users, or projects ever ingested.
const facetLimit = 50

// GlobalFacets returns the busiest agents, machines, usernames, and projects
// across all sessions, each with its session count, ordered busiest first. The
// counts are read from the session_facets rollup (maintained incrementally by a
// trigger on the sessions table, see migration 0005), so each category is a
// bounded top-N index read rather than a GROUP BY over the whole sessions table.
// It backs the global Sessions view's filter rail.
func (s *Store) GlobalFacets(ctx context.Context) (GlobalFacetValues, error) {
	var f GlobalFacetValues

	// Agent and machine values are stored verbatim, so they read straight from
	// the rollup. Each kind is its own bounded subquery off the (kind, n DESC)
	// index, so one busy category cannot crowd out another.
	rows, err := s.Pool.Query(ctx,
		`(SELECT kind, key, n FROM session_facets WHERE kind = 'agent'   ORDER BY n DESC, key LIMIT $1)
		 UNION ALL
		 (SELECT kind, key, n FROM session_facets WHERE kind = 'machine' ORDER BY n DESC, key LIMIT $1)`,
		facetLimit)
	if err != nil {
		return f, fmt.Errorf("query session facets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, val string
		var n int
		if err := rows.Scan(&kind, &val, &n); err != nil {
			return f, fmt.Errorf("scan session facet row: %w", err)
		}
		fc := FacetCount{Value: val, Count: n}
		switch kind {
		case "agent":
			f.Agents = append(f.Agents, fc)
		case "machine":
			f.Machines = append(f.Machines, fc)
		}
	}
	if err := rows.Err(); err != nil {
		return f, fmt.Errorf("iterate session facets: %w", err)
	}
	// UNION ALL does not guarantee the final emission order, so re-establish the
	// busiest-first contract per category rather than trusting the scan order.
	sortFacets := func(fs []FacetCount) {
		sort.Slice(fs, func(i, j int) bool {
			if fs[i].Count != fs[j].Count {
				return fs[i].Count > fs[j].Count
			}
			return fs[i].Value < fs[j].Value
		})
	}
	sortFacets(f.Agents)
	sortFacets(f.Machines)

	// User and project facets store the id in the rollup; take the bounded top-N
	// off the (kind, n DESC, key) index first, then resolve the display label, so
	// the limit is applied by the index scan rather than after joining every row.
	urows, err := s.Pool.Query(ctx,
		`SELECT u.username, f.n
		   FROM (SELECT key, n FROM session_facets WHERE kind = 'user' ORDER BY n DESC, key LIMIT $1) f
		   JOIN users u ON u.id = f.key::bigint
		  ORDER BY f.n DESC, u.username`, facetLimit)
	if err != nil {
		return f, fmt.Errorf("query user facets: %w", err)
	}
	defer urows.Close()
	for urows.Next() {
		var fc FacetCount
		if err := urows.Scan(&fc.Value, &fc.Count); err != nil {
			return f, fmt.Errorf("scan user facet row: %w", err)
		}
		f.Users = append(f.Users, fc)
	}
	if err := urows.Err(); err != nil {
		return f, fmt.Errorf("iterate user facets: %w", err)
	}

	prows, err := s.Pool.Query(ctx,
		`SELECT p.id, p.remote_key, p.display_name, p.kind, f.n
		   FROM (SELECT key, n FROM session_facets WHERE kind = 'project' ORDER BY n DESC, key LIMIT $1) f
		   JOIN projects p ON p.id = f.key::bigint
		  ORDER BY f.n DESC, p.remote_key`, facetLimit)
	if err != nil {
		return f, fmt.Errorf("query project facets: %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var pf ProjectFacet
		if err := prows.Scan(&pf.ID, &pf.Key, &pf.Name, &pf.Kind, &pf.Count); err != nil {
			return f, fmt.Errorf("scan project facet row: %w", err)
		}
		f.Projects = append(f.Projects, pf)
	}
	if err := prows.Err(); err != nil {
		return f, fmt.Errorf("iterate project facets: %w", err)
	}
	// Surface git-remote projects above the standalone/orphaned folders: a real
	// project is what a reader scans for first, the looser local ones sit below.
	// A stable sort preserves the busiest-first order from the query within each
	// group.
	sort.SliceStable(f.Projects, func(i, j int) bool {
		return !isLocalKind(f.Projects[i].Kind) && isLocalKind(f.Projects[j].Kind)
	})
	return f, nil
}

// isLocalKind reports whether a project kind is one of the non-remote kinds
// (standalone or orphaned), which sort below git-remote projects in the facet
// rail.
func isLocalKind(kind string) bool { return kind == "standalone" || kind == "orphaned" }

// scanDetailRow loads one session with its project into a SessionDetail by an arbitrary WHERE
// clause, also reporting whether its cache-savings rollup is backfilled. It is the single-query
// core that scanDetail wraps with the read-side backfill; every displayed field comes from this
// one sessions-row read, so the token split and the saving are always one consistent snapshot.
func (s *Store) scanDetailRow(ctx context.Context, where string, arg any) (SessionDetail, bool, error) {
	var d SessionDetail
	var cacheSavingsBackfilled bool
	err := s.Pool.QueryRow(ctx,
		`SELECT s.id, s.agent, s.machine, s.git_branch, u.username,
		        s.message_count, s.user_message_count,
		        s.total_input_tokens, s.total_output_tokens,
		        s.total_cache_write_tokens, s.total_cache_read_tokens,
		        s.total_cost_usd, s.cost_incomplete, s.visibility, s.public_id,
		        s.started_at, s.ended_at, s.updated_at,
		        s.user_id, s.project_id, p.remote_key, p.display_name, p.kind, s.cwd, s.parent_session_id, s.parser_version,
		        s.total_cache_savings_usd, s.cache_savings_incomplete, s.cache_savings_backfilled,
		        coalesce(title.content, '')
		   FROM sessions s
		   JOIN users u ON u.id = s.user_id
		   JOIN projects p ON p.id = s.project_id
		   LEFT JOIN LATERAL (
		          SELECT left(m.content, `+itoa(titleCap)+`) AS content
		            FROM messages m
		           WHERE m.session_id = s.id AND m.role = 'user'
		           ORDER BY m.ordinal LIMIT 1
		        ) title ON true
		  WHERE `+where,
		arg).Scan(
		&d.ID, &d.Agent, &d.Machine, &d.GitBranch, &d.Username,
		&d.MessageCount, &d.UserMessageCount,
		&d.TotalInput, &d.TotalOutput, &d.TotalCacheWrite, &d.TotalCacheRead,
		&d.TotalCostUSD, &d.CostIncomplete, &d.Visibility, &d.PublicID,
		&d.StartedAt, &d.EndedAt, &d.UpdatedAt,
		&d.OwnerID, &d.ProjectID, &d.ProjectKey, &d.ProjectName, &d.ProjectKind, &d.Cwd, &d.ParentID, &d.ParserVersion,
		&d.TotalCacheSavingsUSD, &d.CacheSavingsIncomplete, &cacheSavingsBackfilled,
		&d.Title)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionDetail{}, false, ErrNotFound
	}
	if err != nil {
		return SessionDetail{}, false, err
	}
	d.Title = squashSpaces(d.Title)
	return d, cacheSavingsBackfilled, nil
}

// scanDetail loads one session with its project, by an arbitrary WHERE clause.
//
// The Cache tile reads the total_cache_savings_usd rollup rather than rescanning usage_events per
// refresh, but that rollup is authoritative only once cache_savings_backfilled is set. A session
// that predates the column is seeded at 0 and left unbackfilled until it is priced, and the startup
// BackfillCacheSavings runs asynchronously, so a detail read can arrive before it. Serving the
// seeded value would put a wrong saving on the very session a reader opened. Two cheaper-looking
// fixes are both wrong: recomputing the saving on every read makes a live session under SSE pay a
// full usage_events scan per refresh, O(K^2) over its rows; and reading the token split from the
// sessions rollup while recomputing only the saving from usage_events lets a concurrent append tear
// the tile, pairing an old split with a newer saving. So this backfills the row once on demand,
// under the same locked primitive the startup pass uses: backfillCacheSavingsForSession prices the
// saving, persists it, and flips the flag (safe against the live parse fold), after which this read
// and every later one serve the O(1) rollup from one consistent scanDetailRow snapshot.
//
// A stored rollup is authoritative only when the corpus has been priced at THIS binary's rate table,
// which the singleton pricing marker records: cache_savings_priced_version == pricing.Version. When
// the marker differs, a pricing rollout is in flight in one of two directions, and either way a
// backfilled=true row may hold a saving at a different rate table than a live recompute would produce,
// so the rollup is provisional. A newer binary is ahead (marker > pricing.Version): the stored value
// was priced at the newer rates and this older binary must not present it as its own exact figure. Or
// this newer binary's own reconcile has not run yet (marker < pricing.Version): existing rows are
// still at the OLD rates while a live recompute here uses the new ones. In both cases the read serves
// the stored value flagged partial rather than pay a per-read recompute: this is the session-body SSE
// path, so an O(K) usage_events scan per refresh would be O(K^2) over a live session, the exact cost
// the total_cache_savings_usd rollup exists to avoid. The marker check is one O(1) singleton read, and
// the periodic reconcile re-prices the corpus to pricing.Version within a settle tick, so the partial
// flag clears itself; until then it reads as provisional (the tile appends "partial") rather than
// asserting a figure a recompute would contradict.
//
// When the marker is current (the steady state), the rollup is authoritative once cache_savings_backfilled
// is set. A session that predates the column is seeded at 0 and left unbackfilled until it is priced,
// and the startup BackfillCacheSavings runs asynchronously, so a detail read can arrive before it.
// Serving the seeded value would put a wrong saving on the very session a reader opened, so this
// backfills the row once on demand, under the same locked primitive the startup pass uses:
// backfillCacheSavingsForSession prices the saving, persists it, and flips the flag (safe against the
// live parse fold), after which this read and every later one serve the O(1) rollup from one consistent
// scanDetailRow snapshot. Recomputing on every read, or reading the token split from the rollup while
// recomputing only the saving from usage_events, are both rejected for the same reasons: the first is
// O(K^2) under SSE, the second lets a concurrent append tear the tile by pairing an old split with a
// newer saving.
func (s *Store) scanDetail(ctx context.Context, where string, arg any) (SessionDetail, error) {
	marker, err := s.cacheSavingsPricedVersion(ctx)
	if err != nil {
		return SessionDetail{}, err
	}
	d, backfilled, err := s.scanDetailRow(ctx, where, arg)
	if err != nil {
		return SessionDetail{}, err
	}
	if marker != pricing.Version {
		// A pricing rollout is in flight (marker ahead or behind pricing.Version), so the stored rollup
		// may be at a different rate table than a live recompute. Serve it flagged partial, from the same
		// single row read as the token split so the two never tear, and do NOT recompute on this hot path.
		d.CacheSavingsIncomplete = true
		return d, nil
	}
	if backfilled {
		return d, nil
	}
	if _, err := s.backfillCacheSavingsForSession(ctx, d.ID); err != nil {
		return SessionDetail{}, fmt.Errorf("backfill cache savings for session %d on read: %w", d.ID, err)
	}
	// Re-read with the original predicate, not by id: the flag is now set (in the ordinary case), so
	// this returns the O(1) rollup from one post-backfill snapshot rather than a mix of the pre-backfill
	// scan and the new saving, and re-applying the same where clause rechecks any visibility gate (the
	// public path filters on visibility = 'public'), so a session unpublished between the two reads is
	// not rendered from the by-id re-read.
	d, backfilled, err = s.scanDetailRow(ctx, where, arg)
	if err != nil {
		return SessionDetail{}, err
	}
	if !backfilled {
		// The marker moved between the read above and the backfill (a concurrent reconcile winning the
		// marker), so backfillCacheSavingsForSession bowed out. Serve the stored value flagged partial for
		// the same reason as the marker-in-flight branch above rather than recompute on the SSE path.
		d.CacheSavingsIncomplete = true
	}
	return d, nil
}

// SessionDetailByID loads a session by numeric id.
func (s *Store) SessionDetailByID(ctx context.Context, id int64) (SessionDetail, error) {
	return s.scanDetail(ctx, "s.id = $1", id)
}

// SessionDetailByPublicID loads a published session by its public id.
func (s *Store) SessionDetailByPublicID(ctx context.Context, publicID string) (SessionDetail, error) {
	return s.scanDetail(ctx, "s.public_id = $1 AND s.visibility = 'public'", publicID)
}

// MessageCount returns a session's current message count from its rollup.
func (s *Store) MessageCount(ctx context.Context, sessionID int64) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, "SELECT message_count FROM sessions WHERE id = $1", sessionID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("read message count for session %d: %w", sessionID, err)
	}
	return n, nil
}

// Messages returns a session's whole transcript in order. The web renderer wants
// the full session in one pass; bounded readers (the MCP transcript window) use
// MessagesPage instead so peak memory does not scale with session size.
func (s *Store) Messages(ctx context.Context, sessionID int64) ([]Message, error) {
	return s.scanMessages(ctx,
		`SELECT ordinal, role, content, thinking_text, model, has_thinking, has_tool_use, timestamp
		   FROM messages WHERE session_id = $1 ORDER BY ordinal`, sessionID)
}

// MessagesAfter returns the next window of a session's transcript ordered by
// ordinal, starting strictly after the given ordinal (after == nil for the first
// window). It pages by keyset on ordinal rather than OFFSET: each call walks the
// messages primary key (session_id, ordinal) straight to the resume point and reads
// only the next `limit` rows, so reading a whole session window by window costs
// O(N), not the O(N^2/limit) an OFFSET walk would (Postgres re-skips the already
// returned prefix on every page). limit is clamped to [1, 2000].
func (s *Store) MessagesAfter(ctx context.Context, sessionID int64, after *int, limit int) ([]Message, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	if after == nil {
		return s.scanMessages(ctx,
			`SELECT ordinal, role, content, thinking_text, model, has_thinking, has_tool_use, timestamp
			   FROM messages WHERE session_id = $1 ORDER BY ordinal LIMIT $2`,
			sessionID, limit)
	}
	return s.scanMessages(ctx,
		`SELECT ordinal, role, content, thinking_text, model, has_thinking, has_tool_use, timestamp
		   FROM messages WHERE session_id = $1 AND ordinal > $2 ORDER BY ordinal LIMIT $3`,
		sessionID, *after, limit)
}

func (s *Store) scanMessages(ctx context.Context, query string, args ...any) ([]Message, error) {
	rows, err := s.Pool.Query(ctx, query, args...)
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

// ToolCalls returns all of a session's tool calls as metadata, for the web
// renderer. Bounded readers pass a message-ordinal range to ToolCallsInRange.
func (s *Store) ToolCalls(ctx context.Context, sessionID int64) ([]ToolCallView, error) {
	return s.scanToolCalls(ctx,
		`SELECT message_ordinal, call_index, tool_name, coalesce(category,''), coalesce(file_path,''),
		        coalesce(input_sha256,''), coalesce(input_bytes,0), coalesce(input_media_type,''),
		        coalesce(result_sha256,''), coalesce(result_bytes,0), coalesce(result_media_type,''), coalesce(result_status,'')
		   FROM tool_calls WHERE session_id = $1 ORDER BY message_ordinal, call_index`, sessionID)
}

// ToolCallsInRange returns the tool calls hanging on messages in the inclusive
// ordinal window [minOrdinal, maxOrdinal], so a bounded transcript read fetches
// only the calls for the messages it returned rather than the whole session.
func (s *Store) ToolCallsInRange(ctx context.Context, sessionID int64, minOrdinal, maxOrdinal int) ([]ToolCallView, error) {
	return s.scanToolCalls(ctx,
		`SELECT message_ordinal, call_index, tool_name, coalesce(category,''), coalesce(file_path,''),
		        coalesce(input_sha256,''), coalesce(input_bytes,0), coalesce(input_media_type,''),
		        coalesce(result_sha256,''), coalesce(result_bytes,0), coalesce(result_media_type,''), coalesce(result_status,'')
		   FROM tool_calls WHERE session_id = $1 AND message_ordinal BETWEEN $2 AND $3
		   ORDER BY message_ordinal, call_index`, sessionID, minOrdinal, maxOrdinal)
}

func (s *Store) scanToolCalls(ctx context.Context, query string, args ...any) ([]ToolCallView, error) {
	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolCallView
	for rows.Next() {
		var t ToolCallView
		if err := rows.Scan(&t.MessageOrdinal, &t.CallIndex, &t.ToolName, &t.Category, &t.FilePath,
			&t.InputSHA, &t.InputBytes, &t.InputMediaType,
			&t.ResultSHA, &t.ResultBytes, &t.ResultMediaType, &t.ResultStatus); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DuplicateCallUIDCount returns how many of a session's tool-call ids appear on more
// than one row. The GROUP BY runs in the database against the (session_id, call_uid)
// index, so the result is a bounded scalar and the session view can flag a repeated
// id without loading or grouping the calls in process memory. It is normally zero; a
// non-zero count means the transcript replayed a turn (a resumed or compacted Claude
// session repeats a tool_use id), which the view surfaces as a chip so a genuinely
// malformed id reuse is visible rather than silent.
func (s *Store) DuplicateCallUIDCount(ctx context.Context, sessionID int64) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM (
		   SELECT 1 FROM tool_calls
		    WHERE session_id = $1 AND call_uid IS NOT NULL
		    GROUP BY call_uid HAVING count(*) > 1
		 ) dups`, sessionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count duplicate call ids for session %d: %w", sessionID, err)
	}
	return n, nil
}

// AttachmentView is one attachment (today a lifted image) rendered under its
// message: the blob key plus enough metadata to show or link the image without
// fetching it. The bytes are served on demand through the session-scoped blob route.
type AttachmentView struct {
	MessageOrdinal int
	SHA256         string
	MediaType      string
	ByteLen        int64
	Filename       string
}

// Attachments returns all of a session's attachments, ordered by the message they
// hang on, for the web renderer. Bounded readers pass an ordinal range to
// AttachmentsInRange.
func (s *Store) Attachments(ctx context.Context, sessionID int64) ([]AttachmentView, error) {
	return s.scanAttachments(ctx,
		`SELECT coalesce(message_ordinal, 0), sha256, coalesce(media_type,''), coalesce(byte_len,0), coalesce(filename,'')
		   FROM attachments WHERE session_id = $1 ORDER BY message_ordinal, id`, sessionID)
}

// AttachmentsInRange returns the attachments hanging on messages in the inclusive
// ordinal window [minOrdinal, maxOrdinal], so a bounded transcript read fetches
// only the attachments for the messages it returned.
func (s *Store) AttachmentsInRange(ctx context.Context, sessionID int64, minOrdinal, maxOrdinal int) ([]AttachmentView, error) {
	return s.scanAttachments(ctx,
		`SELECT coalesce(message_ordinal, 0), sha256, coalesce(media_type,''), coalesce(byte_len,0), coalesce(filename,'')
		   FROM attachments WHERE session_id = $1 AND message_ordinal BETWEEN $2 AND $3
		   ORDER BY message_ordinal, id`, sessionID, minOrdinal, maxOrdinal)
}

func (s *Store) scanAttachments(ctx context.Context, query string, args ...any) ([]AttachmentView, error) {
	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query attachments: %w", err)
	}
	defer rows.Close()
	var out []AttachmentView
	for rows.Next() {
		var a AttachmentView
		if err := rows.Scan(&a.MessageOrdinal, &a.SHA256, &a.MediaType, &a.ByteLen, &a.Filename); err != nil {
			return nil, fmt.Errorf("scan attachment row: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachments: %w", err)
	}
	return out, nil
}

// SessionRawTo streams a session's raw uploaded bytes (the lossless JSONL the
// client sent, the source every projection is rebuilt from) to w in upload order,
// writing at most limit bytes. It returns the number of bytes written, whether the
// session held more than was written (so the caller can flag a truncated read), and
// the session's full raw length. A limit of zero or less means no cap. This is the
// raw underlying data behind the parsed transcript, exposed so an agent can inspect
// exactly what was ingested rather than only the projection. A missing session
// returns ErrNotFound.
func (s *Store) SessionRawTo(ctx context.Context, w io.Writer, sessionID, limit int64) (written int64, truncated bool, total int64, err error) {
	// total and the streamed content must come from one snapshot: an AppendChunk or
	// ResetRaw committing between the length read and the chunk stream would otherwise
	// let total_bytes describe one version of the raw while content is another. A
	// repeatable-read, read-only transaction pins both reads to the same MVCC snapshot,
	// so a concurrent writer is simply invisible to this reader rather than half-seen.
	txErr := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			// total is session_raw.byte_len, the running length AppendChunk and ResetRaw
			// maintain in the same transaction that writes the chunk rows (so the invariant
			// byte_len == sum(length(content)) holds at every committed state, pinned by
			// TestSessionRawByteLenMatchesChunks). Reading it is O(1) rather than scanning the
			// whole growing raw, and reading it inside this snapshot keeps it exactly
			// consistent with the chunks streamed below. A missing session_raw row (no upload
			// yet) is ErrNotFound.
			if err := tx.QueryRow(ctx,
				`SELECT byte_len FROM session_raw WHERE session_id = $1`, sessionID).Scan(&total); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotFound
				}
				return fmt.Errorf("read raw length for session %d: %w", sessionID, err)
			}

			rows, err := tx.Query(ctx,
				`SELECT byte_offset, byte_len, content
				   FROM session_raw_chunks WHERE session_id = $1 ORDER BY byte_offset`, sessionID)
			if err != nil {
				return fmt.Errorf("read raw chunks for session %d: %w", sessionID, err)
			}
			defer rows.Close()
			for rows.Next() {
				var off, length int64
				var content []byte
				if err := rows.Scan(&off, &length, &content); err != nil {
					return fmt.Errorf("scan raw chunk for session %d: %w", sessionID, err)
				}
				if limit > 0 && written+int64(len(content)) > limit {
					content = content[:limit-written]
					if _, err := w.Write(content); err != nil {
						return err
					}
					written += int64(len(content))
					truncated = true
					return nil
				}
				if _, err := w.Write(content); err != nil {
					return err
				}
				written += int64(len(content))
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("iterate raw chunks for session %d: %w", sessionID, err)
			}
			truncated = written < total
			return nil
		})
	if txErr != nil {
		return written, truncated, total, txErr
	}
	return written, truncated, total, nil
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
