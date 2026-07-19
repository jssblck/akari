package store

import (
	"context"
	"errors"
	"fmt"
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
	// LastActivity is the most recent session activity in the project: max over its
	// sessions' last_active_at (last-event time), not their updated_at write time,
	// so a reparse of the project's sessions does not float it to the top of the
	// projects index. NULL for a project with no sessions.
	LastActivity *time.Time
	// OverviewPublic gates whether the project's usage overview resolves for
	// logged-out viewers at /p/<id>. Every read that returns a ProjectSummary
	// populates it from projects.overview_public (Project, PublicProjectOverview, and
	// the ListProjects rollup), so the flag reads the same for a given project across
	// surfaces rather than one projection carrying a stale false the others contradict.
	OverviewPublic bool
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
	// ModelFallbackCount is how many times this session's Claude Fable turns were declined by
	// the safety classifier and re-served on a lower model (see migration 0034). It is read from
	// the sessions.model_fallback_count rollup so every summary read surfaces it in O(1). Every
	// SessionSummary query loads it (the shared sessionSelect, the cross-project feed row, and the
	// single-session header) so the count is truthful wherever a summary is published, including a
	// subagent row: the MCP DTO reports it as an always-present field, so a summary that skipped the
	// column would falsely read zero on a child session that actually fell back.
	ModelFallbackCount int
	TotalInput         int64
	TotalOutput        int64
	TotalCacheWrite    int64
	TotalCacheRead     int64
	TotalCostUSD       float64
	Visibility         string
	PublicID           *string
	StartedAt          *time.Time
	EndedAt            *time.Time
	// LastActiveAt is when the session was last active: its last event timestamp
	// (ended_at), falling back to the row's creation time for a transcript that
	// carried no timestamps. It is the feed's "updated" recency, read from the
	// generated last_active_at column rather than the row's updated_at write time,
	// so a reparse (which restamps updated_at to now) never makes a days-old session
	// read as freshly updated. See migration 0033.
	LastActiveAt *time.Time
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
	OwnerID     int64
	ProjectID   int64
	ProjectKey  string
	ProjectName string
	ProjectKind string
	Cwd         string
	ParentID    *int64
	// TotalCacheSavingsUSD is the session's rolled-up prompt-cache saving (folded by the
	// rebuild beside total_cost_usd), so the Cache tile reads it in O(1) instead of
	// scanning usage_events on every live refresh.
	TotalCacheSavingsUSD float64
}

// Message is one transcript row for rendering. The prompt-hygiene facts (PromptShort,
// PromptNoCode, PromptDigest) are the fixed-size verdicts quality.ClassifyPrompt materialized
// on the row when the message was written; they carry no prompt body, so the full-transcript
// read stays fixed-size per row and never re-reads content to classify it.
//
// PromptFactsCurrent gates them: it is true only when the digest is non-NULL (a real
// classified human turn). Every row is rewritten by the epoch rebuild, so stored facts are
// always the running classifier's output; the flag only distinguishes a classified prompt
// from a non-prompt row (assistant turn, empty user turn), so the renderer badges exactly
// the turns the aggregate counted.
type Message struct {
	Ordinal      int
	Role         string
	Content      string
	ThinkingText string
	Model        string
	HasThinking  bool
	HasToolUse   bool
	// ThinkingBytes is the turn's reasoning-trace weight (plaintext length where the agent
	// logs it, else the encrypted payload length; see parser.Message.ThinkingBytes). The
	// transcript's per-message thinking band estimates the turn's reasoning tokens from it
	// when the agent reports no exact count (Usage.Reasoning), the same estimate the session
	// and fleet reads use.
	ThinkingBytes int
	Timestamp     *time.Time
	// Prompt-hygiene facts, meaningful only when PromptFactsCurrent is true.
	PromptShort        bool
	PromptNoCode       bool
	PromptDigest       int64
	PromptFactsCurrent bool
	// Usage is this turn's rolled-up token load and cost, folded per message ordinal from the
	// session's usage_events in the same read (a LEFT JOIN, not a second query), so the transcript
	// stamps a message with its own context and cost without holding a second session-sized map
	// beside the message slice. It is nil when the ordinal carried no attributed usage_events row
	// (an ordinal with no dated usage has no turn load to show), the same "no entry" the old
	// per-ordinal usage map expressed by a missing key.
	Usage *TurnUsage
	// DuplicatePrompt is true when this user turn repeats an earlier eligible prompt's digest
	// verbatim, computed in SQL over the same eligible set gatherPromptHygiene's duplicate count
	// uses (role user, content_length > 0, current prompt facts, a non-null digest, and NOT
	// prompt_short), so a transcript "repeat" badge and the stored duplicate_prompt_count read the
	// same set. The first occurrence of a digest is never marked (it is the original); every later
	// eligible occurrence is. An ineligible row is never marked.
	DuplicatePrompt bool
}

// ToolCallView is one tool call rendered as metadata (the body lives in the CAS,
// fetched on demand by its sha256).
type ToolCallView struct {
	MessageOrdinal int
	CallIndex      int
	ToolName       string
	Category       string
	FilePath       string
	// FileRelPath is the worktree-relative form of FilePath (migration 0030), empty when the
	// path sits outside the session's workspace or no cwd was known. The transcript shows it in
	// preference to the absolute FilePath: the same repo file reads the same across worktrees.
	FileRelPath     string
	Detail          string
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
	// IncludeSubagents keeps subagent sessions (relationship_type = 'subagent') in the
	// global feed. The default excludes them: a fleet's spawned reviewers, fan-out
	// workers, and spec-extraction batches vastly outnumber the top-level runs a reader
	// is looking for, and each already rolls up under its parent's detail page. Setting
	// it shows the whole tree. Continuations (a resumed session) stay visible either way:
	// they are real work a reader started, not machinery a parent spun up. It is applied
	// in ListAllSessions, not the shared conds(), so it narrows only the browse feed and
	// leaves the count, facet, MCP-feed, and Insights drill-through queries counting every
	// session (see ListAllSessions).
	IncludeSubagents bool
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
	// RequireSpan narrows to sessions with a measured span (a parsed start and end and a
	// non-negative duration), the exact cohort the Insights Concurrency panel sweeps (see
	// spanFilter in analytics_concurrency.go). The busiest-user drill sets it so the
	// linked feed matches the panel's session set rather than a looser "all this user's
	// sessions in the window", which would list sessions the panel never counted. The
	// zero value applies no span constraint.
	RequireSpan bool
	// Sort names the column the global session list is ordered by (see
	// sessionSortColumns). The empty string means DefaultSort. Desc selects
	// descending order. Together they back the click-to-sort table headers; an
	// unknown Sort falls back to DefaultSort in the query builder.
	Sort   string
	Desc   bool
	Limit  int
	Offset int
	// After is a keyset cursor: the id of the last row the reader has already seen, so
	// "Show more" fetches the page strictly after it in the current sort order rather
	// than re-reading rows 1..N with a doubled limit. Zero means the first page. It applies
	// only to the four keyset-sortable feed orders (updated, tokens, messages, cost, in
	// sessionKeysetColumns); an After set under any other sort is ignored, since those
	// orders are not offered a "Show more" cursor. See ListAllSessions.
	After int64
	// AfterVal is the cursor row's sort value as the page observed it (the last visible row's
	// last_active_at, total_tokens, message_count, or total_cost_usd, formatted for its column
	// type). It is carried so the resume boundary stays fixed at what the reader already saw:
	// the sort columns are mutable projection fields (activity bumps last_active_at, a rebuild
	// moves the cost/token/message counts), so resolving the boundary live from the cursor row
	// on the next request would let it drift and duplicate or skip rows. Empty falls back to a
	// live scalar-subquery lookup of the cursor row's value, which is what a legacy cursor URL
	// (id only) still gets. It is meaningful only alongside a keyset-sortable After.
	AfterVal string
}

// conds builds the WHERE additions for the filter's narrowing fields, shared by
// every session-list query so a filter field added here reaches all of them at
// once. sinceCol names the timestamp column the Since bound applies to:
// s.last_active_at for the last-active list (ListSessions), s.started_at for the
// global feed that must match the Insights quality window (ListAllSessions,
// CountAllSessions), and ue.occurred_at for the windowed queries that scope by
// dated usage. Placeholders start at $1; the caller appends its own (cursor,
// limit, offset) after.
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
	if f.RequireSpan {
		// The exact predicate the concurrency sweep uses (spanFilter in
		// analytics_concurrency.go): a parsed start and end and a non-negative duration.
		// Kept in step by name so the busiest-user drill lists precisely the spanned
		// cohort the panel counted. No placeholder: it is a plain column comparison.
		conds = append(conds, "s.started_at IS NOT NULL AND s.ended_at IS NOT NULL AND s.ended_at >= s.started_at")
	}
	// Grade and outcome match the Insights distributions' definition exactly, so a
	// drill-through from a panel bar opens precisely the sessions that bar counted
	// (pinned bucket by bucket in TestDrillFiltersMatchQualityDistribution). The panel's
	// scan (qualityDistributionFrom in analytics_quality.go) LEFT JOINs the non-stale
	// signals row and coalesces a missing one into the catch-all bucket (grade ''
	// / outcome 'unknown'), so the buckets split two ways:
	//
	//   - A letter grade or a concrete outcome is an EXISTS: the session has a usable row
	//     (NOT s.signals_stale) carrying that value. Only NULL grades and missing rows
	//     fold in the panel, so EXISTS matches its letter and concrete-outcome bars
	//     exactly.
	//   - The catch-all buckets are the complement, a NOT EXISTS: "unscored" is no usable
	//     row with a non-NULL grade, which folds in the explicit NULL-grade row, the stale
	//     row, and the session with no row at all. "unknown" is likewise no usable row
	//     with a different outcome, folding the explicit 'unknown' row together with the
	//     missing-row cases.
	if g := f.Grade; g != "" {
		gate := signalsCurrent()
		if g == "unscored" {
			conds = append(conds, "NOT EXISTS (SELECT 1 FROM session_signals sig WHERE sig.session_id = s.id AND "+
				gate+" AND sig.grade IS NOT NULL)")
		} else {
			args = append(args, g)
			conds = append(conds, "EXISTS (SELECT 1 FROM session_signals sig WHERE sig.session_id = s.id AND "+
				gate+" AND sig.grade = $"+itoa(len(args))+")")
		}
	}
	if o := f.Outcome; o != "" {
		gate := signalsCurrent()
		args = append(args, o)
		if o == "unknown" {
			conds = append(conds, "NOT EXISTS (SELECT 1 FROM session_signals sig WHERE sig.session_id = s.id AND "+
				gate+" AND sig.outcome <> $"+itoa(len(args))+")")
		} else {
			conds = append(conds, "EXISTS (SELECT 1 FROM session_signals sig WHERE sig.session_id = s.id AND "+
				gate+" AND sig.outcome = $"+itoa(len(args))+")")
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
	// "updated" is the feed's recency order. It sorts on last_active_at (the
	// session's last-event time), NOT updated_at (the row's last-write time): a
	// reparse restamps updated_at but leaves last_active_at fixed, so an old
	// session stays where its activity puts it. The query-string key stays
	// "updated" so bookmarked feed URLs keep working; only the column it maps to
	// changed. Index-walked by idx_sessions_feed_active (migration 0033), with
	// top-level-only twins for the subagent-hiding default feed: the global order
	// (0046) and one per facet (0047), so a project, user, agent, or machine feed
	// walks only its own top-level rows rather than the facet's subagent history.
	"updated": "s.last_active_at",
}

// sessionKeysetColumns names the bare session column behind each keyset-paginable feed
// order, for the "Show more" cursor. It is the subset of sessionSortColumns whose sort
// expression is a single NOT NULL column on the sessions row, so a keyset predicate can
// compare (column, id) against the cursor row's (column, id) and walk the same (col, id)
// btree the order already uses. The other sort keys (project, agent, branch, user) rank a
// joined or text expression and are not offered a cursor, so they are absent here and an
// After under them is ignored. The four listed are exactly the feed's sort dropdown
// (SessionSortOptions), which is all a reader ever paginates.
var sessionKeysetColumns = map[string]string{
	"updated":  "last_active_at",
	"tokens":   "total_tokens",
	"messages": "message_count",
	"cost":     "total_cost_usd",
}

// IsSortKey reports whether key names a sortable column of the global session
// list, so the handler can reject an unknown or tampered sort param before it
// reaches the query builder.
func IsSortKey(key string) bool {
	_, ok := sessionSortColumns[key]
	return ok
}

// resolvedSort returns the effective sort key and direction the query builder uses: the
// filter's Sort and Desc when Sort is a known key, else the default (updated, descending).
// orderClause and the keyset predicate both read it, so the order they emit and the cursor
// comparison they build can never disagree on which column or direction the page walks.
func (f SessionFilter) resolvedSort() (key string, desc bool) {
	if _, ok := sessionSortColumns[f.Sort]; ok {
		return f.Sort, f.Desc
	}
	return DefaultSort, true
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
// matching the feed indexes (0033, 0006) exactly.
//
// No NULLS LAST: every sortable expression is NOT NULL (the session columns, the
// generated last_active_at and total_tokens, the username join, and the project
// CASE over two NOT NULL columns all are), so a nulls placement would never change
// a result. It
// would, however, defeat the index: Postgres only matches an ORDER BY to a btree
// when the nulls placement agrees, and a DESC btree defaults to NULLS FIRST, so a
// "DESC NULLS LAST" clause forces a full sort instead of an index scan. Omitting
// it lets the (col, id) and feed indexes satisfy the order directly.
func (f SessionFilter) orderClause() string {
	key, desc := f.resolvedSort()
	expr := sessionSortColumns[key]
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	return fmt.Sprintf(" ORDER BY %s %s, s.id %s", expr, dir, dir)
}

// keysetColType names the Postgres type of each keyset sort column, so a cursor value carried
// as text (SessionFilter.AfterVal) casts back to the exact value the column holds. The keys are
// the columns in sessionKeysetColumns: last_active_at is a timestamptz, the two counts are
// integers, and total_cost_usd is a double precision (not a numeric), so a shortest-round-trip
// float text casts back to the identical float64 and the equality tiebreak stays exact.
var keysetColType = map[string]string{
	"last_active_at": "timestamptz",
	"total_tokens":   "bigint",
	"message_count":  "integer",
	"total_cost_usd": "double precision",
}

// keysetCond returns the WHERE predicate that resumes the feed strictly after the After cursor
// in the current sort order, the args it binds, and whether a cursor applies. It applies only
// when After is set and the sort has a keyset column (sessionKeysetColumns); otherwise it
// returns ok=false and the caller pages from the top. firstArg is the number of the first bind
// slot it may claim: the cursor id, and (when the cursor carries an observed value) that value.
//
// The predicate mirrors orderClause's "(col, id)" ordering exactly: under the descending feed a
// later row has a smaller (col, id), so it keeps rows where col is below the cursor's or, on a
// tie, id is below it; an ascending sort flips both comparisons. The boundary value comes from
// AfterVal, the value the rendered page actually saw, cast back to the column's type, so a later
// change to the cursor row's own column (activity bumping last_active_at, a rebuild moving a
// count) cannot move the boundary and duplicate or skip rows the reader has or has not seen. A
// legacy cursor that carries no value falls back to a scalar subquery on the cursor id: exact
// while the row is unchanged, and a cursor id that names no row makes the subquery NULL so the
// predicate matches nothing and "Show more" ends cleanly rather than resuming from a wrong place.
func (f SessionFilter) keysetCond(firstArg int) (cond string, cargs []any, ok bool) {
	if f.After <= 0 {
		return "", nil, false
	}
	key, desc := f.resolvedSort()
	col, has := sessionKeysetColumns[key]
	if !has {
		return "", nil, false
	}
	op := "<"
	if !desc {
		op = ">"
	}
	idPH := "$" + itoa(firstArg)
	cargs = append(cargs, f.After)
	// The value expression appears twice below but binds one slot, so the cursor value (or the
	// subquery on the cursor id) is evaluated against the same boundary in both the col
	// comparison and the tie's equality.
	valExpr := "(SELECT " + col + " FROM sessions WHERE id = " + idPH + ")"
	if f.AfterVal != "" {
		cargs = append(cargs, f.AfterVal)
		valExpr = "$" + itoa(firstArg+1) + "::" + keysetColType[col]
	}
	cond = fmt.Sprintf("(s.%s %s %s OR (s.%s = %s AND s.id %s %s))",
		col, op, valExpr, col, valExpr, op, idPH)
	return cond, cargs, true
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
	// Grade is the session's letter grade (A..F) from its current, non-stale signals
	// row, nil when the session is unscored or has not settled yet. Outcome is that
	// row's outcome (completed / abandoned / errored / unknown), empty when no current
	// row exists. Both come from a LEFT JOIN in globalSessionSelect gated by
	// signalsCurrent(), so a feed row's grade and outcome match the drill filters and
	// the Insights panels rather than reading a stale verdict.
	Grade   *string
	Outcome string
	// Search is the content-match snippet for this row, populated only when the
	// list was run with a Query filter: a window of the first matching message's
	// content centered on the match, so the feed can show what the session said
	// around the search term. It is the zero value on an unfiltered list.
	Search SearchSnippet
	// Tree is the whole-work-item rollup for this row: its own cost plus every
	// subagent it fanned out, and the count of those subagents. It is filled only by
	// the feed path (ListAllSessions attaches it after the page is scanned), so a row
	// read outside the feed carries the zero-value rollup (no fan-out). The feed shows
	// the fan-out only when SubagentCount > 0, since a session that spawned nothing has
	// no work-item cost beyond the cost the row already shows.
	Tree TreeRollup
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
		`SELECT p.id, p.remote_key, p.host, p.owner, p.repo, p.display_name, p.kind, p.overview_public,
		        count(s.id),
		        coalesce(sum(s.total_cost_usd), 0),
		        coalesce(sum(s.total_input_tokens), 0),
		        coalesce(sum(s.total_output_tokens), 0),
		        coalesce(sum(s.total_cache_read_tokens), 0),
		        coalesce(sum(s.total_cache_write_tokens), 0),
		        max(s.last_active_at)
		   FROM projects p
		   LEFT JOIN sessions s ON s.project_id = p.id
		  GROUP BY p.id
		  ORDER BY max(s.last_active_at) DESC NULLS LAST, p.remote_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectSummary
	for rows.Next() {
		var p ProjectSummary
		if err := rows.Scan(&p.ID, &p.RemoteKey, &p.Host, &p.Owner, &p.Repo, &p.DisplayName, &p.Kind, &p.OverviewPublic,
			&p.SessionCount, &p.TotalCostUSD, &p.TotalInput, &p.TotalOutput,
			&p.TotalCacheRead, &p.TotalCacheWrite, &p.LastActivity); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Project returns one project's identity (without rollups), including whether its
// overview is published, so the signed-in project page can render the publicity
// control's current state without a second query.
func (s *Store) Project(ctx context.Context, id int64) (ProjectSummary, error) {
	var p ProjectSummary
	err := s.Pool.QueryRow(ctx,
		`SELECT id, remote_key, host, owner, repo, display_name, kind, overview_public FROM projects WHERE id = $1`, id).
		Scan(&p.ID, &p.RemoteKey, &p.Host, &p.Owner, &p.Repo, &p.DisplayName, &p.Kind, &p.OverviewPublic)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectSummary{}, ErrNotFound
	}
	return p, err
}

// sessionSelect is the shared column list and joins for session summaries.
const sessionSelect = `
	SELECT s.id, s.agent, s.machine, s.git_branch, u.username,
	       s.message_count, s.user_message_count, s.model_fallback_count,
	       s.total_input_tokens, s.total_output_tokens,
	       s.total_cache_write_tokens, s.total_cache_read_tokens,
	       s.total_cost_usd, s.visibility, s.public_id,
	       s.started_at, s.ended_at, s.last_active_at
	  FROM sessions s
	  JOIN users u ON u.id = s.user_id`

// windowSessionLimit is the cap on how many session rows the project page renders.
// Past it the table would grow with a project's whole windowed history, so the rows
// stop here and SessionPage.Remainder accounts for the rest.
const windowSessionLimit = 100

// titleCap bounds the first-user-message prefix the Title column carries. 240
// characters is well past what any row can display (CSS ellipsizes it far sooner),
// so the cap only guards against pulling a multi-kilobyte opening prompt into
// every row while leaving the visible portion intact.
const titleCap = 240

// titleLateralSQL derives a session's display title: the first user message's
// content, capped at titleCap. Every query that returns a session row splices in
// this one fragment so a session titles the same on the detail page, the project
// table, the global feed, and its OG card, and so the title rule changes in one
// place. It assumes the outer query aliases the sessions row `s`.
//
// Claude Code prepends a fixed <local-command-caveat>...</local-command-caveat>
// block (a couple hundred characters telling the model to ignore locally generated
// messages) ahead of the user's actual words when a session ran a local command. The
// block alone overruns titleCap, so a naive left() would title the whole session with
// the caveat and never reach the prompt. Strip that one known wrapper on the full
// content first, then cap: the visible title becomes the human's words, not the
// harness boilerplate. The strip is anchored and non-greedy so it removes exactly the
// leading caveat and nothing past its close; a message without one is unchanged.
var titleLateralSQL = `LEFT JOIN LATERAL (
	         SELECT left(regexp_replace(m.content, '^\s*<local-command-caveat>.*?</local-command-caveat>\s*', ''), ` + itoa(titleCap) + `) AS content
	           FROM messages m
	          WHERE m.session_id = s.id AND m.role = 'user'
	          ORDER BY m.ordinal LIMIT 1
	       ) title ON true`

// snippetSQLWindowRadius and snippetSQLWindowLen bound the matching-message content
// the search LATERAL pulls back per row. A page of results would otherwise
// materialize every matching message in full (kilobytes each) only to window down to
// a ~160-char snippet, so the window is cut in SQL: substring the content around the
// match position, keeping radius bytes of lead and enough trailing room that the Go
// snippet builder still has both sides of context to trim to word boundaries. The
// length is radius plus the visible window plus a matching trailing margin, so a match
// anywhere in the window has full context on either side.
const (
	snippetSQLWindowRadius = 240
	snippetSQLWindowLen    = 720
)

// facetLimit caps each facet category to its busiest values, so the rail (and
// the memory backing it) stays a fixed top-N rather than growing with the total
// distinct agents, machines, users, or projects ever ingested.
const facetLimit = 50

// messageReadColumns is the transcript-row column list shared by the full read and the bounded
// window read, so both scan into the same Message shape through scanMessages. It selects the
// message row's own columns, the prompt-hygiene facts gated by content_length > 0 and a real
// classified digest (PromptFactsCurrent), and the stored duplicate-prompt verdict. gatherPromptHygiene
// in signals.go counts only user turns with content_length > 0 (an empty or attachment-only turn is
// tool plumbing, not a prompt), so an empty user row must not render a transcript badge the aggregate
// excluded. duplicate_prompt is materialized over that same eligible set (the rebuild's in-memory
// fold), so the transcript "repeat" badge and the stored duplicate_prompt_count read one set; it is
// read here as a bounded column rather than folded from a whole-session window on every render. The
// remaining scanned columns (the per-turn usage fold) are supplied by whichever query wraps this
// list: the full read folds them from the session's usage_events (messagesFullQuery), the bounded
// window read leaves them empty (messagesWindowQuery), because its only caller, the MCP transcript
// window, renders no per-turn usage.
const messageReadColumns = `m.ordinal, m.role, m.content, m.thinking_text, m.model, m.has_thinking, m.has_tool_use,
	coalesce(m.thinking_bytes, 0), m.timestamp,
	coalesce(m.prompt_short, false), coalesce(m.prompt_no_code, false), coalesce(m.prompt_digest, 0),
	(m.prompt_digest IS NOT NULL AND m.content_length > 0),
	coalesce(m.duplicate_prompt, false)`

// messagesFullQuery is the whole-transcript read behind Messages. It LEFT JOINs the materialized
// per-turn usage rollup (message_turn_usage) so each returned row carries its own turn load without
// re-aggregating the session's usage_events on every render, and the render path holds no second
// session-sized structure beside the message slice it already renders:
//
//   - message_turn_usage holds one row per (session, message_ordinal) with the per-turn sums each
//     message carries on Message.Usage: input, output, cache read, cache write, reasoning, and the
//     cost. It is maintained at insert (projection.go) as each surviving usage row lands, so this
//     read joins one indexed row per message rather than scanning and grouping usage_events. The
//     context occupancy (input + cache read + cache write, output excluded) is computed here from
//     the joined sums. Unknown model prices are already stored as zero, so the cost is one scalar.
//     Only turn-attributed
//     usage contributes to the rollup: a NULL-ordinal usage row belongs to the session totals, not
//     to any one message, so it is never folded here (projection.go skips it). This is the deliberate
//     divergence from the stored context signal (gatherContextHealth), which folds every raw
//     usage_events row in source order (NULL-ordinal rows included) and never collapses two rows
//     sharing an ordinal: the two agree whenever each ordinal carries exactly one dated usage row
//     (the shape a real agent produces) and can differ only for a multi-row turn or an unattributed
//     row, cases the schema permits but the parser does not emit. TestMessagesTurnUsageDivergesFromContextFold
//     pins the difference; TestMessageTurnUsageMatchesUsageEvents pins that the rollup equals the
//     GROUP BY over usage_events it replaced.
//
// The duplicate-prompt verdict is read from the stored duplicate_prompt column (materialized once at
// insert, see projection.go) rather than folded from a whole-session window here. Both the per-turn
// usage and the duplicate flag are now stored per row, so the live body refresh (handleSessionBody
// re-fetching this on every SSE append) reads bounded indexed rows and does no growing whole-session
// usage scan or message window. $1 is the session id.
//
// messagesFullSelect is the shared SELECT..WHERE prefix; the windowed transcript-page reads
// (read_transcript_page.go) append their own ordinal predicates and LIMIT to it, so every
// full-fold read (whole session, tail window, forward append, walker seed) selects the same
// columns and scans through the same scanMessages.
const messagesFullSelect = `
	SELECT ` + messageReadColumns + `,
	       mtu.message_ordinal IS NOT NULL,
	       coalesce(mtu.input_tokens,0), coalesce(mtu.output_tokens,0), coalesce(mtu.cache_read_tokens,0), coalesce(mtu.cache_write_tokens,0),
	       coalesce(mtu.reasoning_tokens,0),
	       coalesce(mtu.input_tokens,0) + coalesce(mtu.cache_read_tokens,0) + coalesce(mtu.cache_write_tokens,0),
	       coalesce(mtu.cost_sum, 0)
	  FROM messages m
	  LEFT JOIN message_turn_usage mtu ON mtu.session_id = m.session_id AND mtu.message_ordinal = m.ordinal
	 WHERE m.session_id = $1`

const messagesFullQuery = messagesFullSelect + `
	 ORDER BY m.ordinal`

// messagesWindowQuery is the bounded transcript read behind MessagesAfter. It selects the message
// row's columns (including the stored duplicate_prompt, a bounded column read) and emits the usage
// columns as empty constants (no usage), so it does no whole-session usage scan: its only caller,
// the MCP transcript window, renders no per-turn usage stamps. The keyset predicate and LIMIT the
// caller appends keep each page bounded to the requested ordinal range. $1 is the session id; the
// caller's window args start at $2.
const messagesWindowQuery = `
	SELECT ` + messageReadColumns + `,
	       false, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::double precision
	  FROM messages m
	 WHERE m.session_id = $1`

// ModelFallbackListCap is the standard cap on how many fallback rows a caller reads for
// one session. sessions.model_fallback_count is the session-wide total and rides every
// view; this list is only the first N by occurrence, so a reader stays bounded no matter
// how pathological the session (a transcript that fell back thousands of times cannot blow
// up a tooltip, a transcript-notice map, or an MCP payload). Callers pass this to
// SessionModelFallbacks and lean on the count for the true total.
const ModelFallbackListCap = 100

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
