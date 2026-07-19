package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

func scanSession(rows pgx.Rows) (SessionSummary, error) {
	var s SessionSummary
	err := rows.Scan(&s.ID, &s.Agent, &s.Machine, &s.GitBranch, &s.Username,
		&s.MessageCount, &s.UserMessageCount, &s.ModelFallbackCount,
		&s.TotalInput, &s.TotalOutput, &s.TotalCacheWrite, &s.TotalCacheRead,
		&s.TotalCostUSD, &s.Visibility, &s.PublicID,
		&s.StartedAt, &s.EndedAt, &s.LastActiveAt)
	return s, err
}

// ListSessions returns sessions matching the filter, newest first. Its Since bound
// and its order both key on last_active_at (last-event time), so this is the
// "sessions last active in a window, most recently active first" list, immune to a
// reparse restamping updated_at. It differs deliberately from ListAllSessions,
// which binds Since to started_at to match the Insights quality window (see the
// note on CountAllSessions and TestQualityDrilldownWindowsOnStartedAt).
func (s *Store) ListSessions(ctx context.Context, f SessionFilter) ([]SessionSummary, error) {
	conds, args := f.conds("s.last_active_at")

	q := sessionSelect
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	// No NULLS LAST: last_active_at is NOT NULL, so it cannot change the result, but
	// it would stop Postgres from matching this order to the feed indexes (0033, 0006)
	// and force a full sort instead of an index walk to the LIMIT. See orderClause.
	q += " ORDER BY s.last_active_at DESC, s.id DESC"
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

// SessionPage is a project's windowed session table: the capped rows the page shows,
// newest-active first, plus the aggregate of every windowed session that did not fit
// (Remainder). Shown rows plus Remainder reproduce the usage panel's headline, since
// both the rows and the remainder derive from the one dated-usage base the panel sums.
type SessionPage struct {
	Sessions  []ProjectSessionSummary
	Remainder SessionRemainder
}

// ProjectSessionSummary adds the current quality signals used by the compact
// project-session list. Keeping it separate from SessionSummary avoids making
// every summary read pay for signals that only this surface renders.
type ProjectSessionSummary struct {
	SessionSummary
	Grade   *string
	Outcome string
}

// SessionRemainder is the aggregate of the windowed sessions the capped table did not
// show: how many, their per-class token volume, and their summed cost. The project page renders it as a footer so the visible
// rows plus this line reconcile with the usage panel headline even when more sessions
// match than the table caps at. It carries all four token classes (not just a total)
// so the footer can show the same breakdown card every other token figure does.
type SessionRemainder struct {
	Sessions   int
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	CostUSD    float64
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
// remainder closes the gap. It is queried rather than subtracted from the panel, so
// the tail is aggregated directly over the hidden sessions with its own token and cost sums.
// The remainder query runs only when the cap actually engaged.
func (s *Store) WindowSessionPage(ctx context.Context, f SessionFilter) (SessionPage, error) {
	var page SessionPage
	err := s.snapshotTx(ctx, func(tx pgx.Tx) error {
		var err error
		page, err = s.windowSessionPage(ctx, tx, f)
		return err
	})
	if err != nil {
		return SessionPage{}, err
	}
	return page, nil
}

func (s *Store) windowSessionPage(ctx context.Context, query querier, f SessionFilter) (SessionPage, error) {
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
		       s.message_count, s.user_message_count, s.model_fallback_count,
		       coalesce(sum(ue.input_tokens), 0), coalesce(sum(ue.output_tokens), 0),
		       coalesce(sum(ue.cache_write_tokens), 0), coalesce(sum(ue.cache_read_tokens), 0),
		       coalesce(sum(ue.cost_usd), 0),
		       s.visibility, s.public_id, s.started_at, s.ended_at, s.last_active_at,
		       coalesce(title.content, ''), sig.grade, coalesce(sig.outcome, '')
		  FROM usage_events ue
		  JOIN sessions s ON s.id = ue.session_id
		  JOIN users u ON u.id = s.user_id
		  LEFT JOIN session_signals sig ON sig.session_id = s.id AND ` + signalsCurrent() + `
		  ` + titleLateralSQL + `
		 WHERE ` + where + `
		 GROUP BY s.id, u.username, title.content, sig.grade, sig.outcome
		 ORDER BY s.last_active_at DESC, s.id DESC
		 LIMIT $` + itoa(len(args)+1)
	rowArgs := append(append([]any{}, args...), windowSessionLimit)

	rows, err := query.Query(ctx, q, rowArgs...)
	if err != nil {
		return SessionPage{}, fmt.Errorf("query window sessions: %w", err)
	}
	defer rows.Close()
	var out []ProjectSessionSummary
	for rows.Next() {
		var sm ProjectSessionSummary
		if err := rows.Scan(&sm.ID, &sm.Agent, &sm.Machine, &sm.GitBranch, &sm.Username,
			&sm.MessageCount, &sm.UserMessageCount, &sm.ModelFallbackCount,
			&sm.TotalInput, &sm.TotalOutput, &sm.TotalCacheWrite, &sm.TotalCacheRead,
			&sm.TotalCostUSD, &sm.Visibility, &sm.PublicID,
			&sm.StartedAt, &sm.EndedAt, &sm.LastActiveAt, &sm.Title, &sm.Grade, &sm.Outcome); err != nil {
			return SessionPage{}, fmt.Errorf("scan window session: %w", err)
		}
		sm.Title = squashSpaces(sm.Title)
		out = append(out, sm)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SessionPage{}, fmt.Errorf("iterate window sessions: %w", err)
	}
	rows.Close()

	page := SessionPage{Sessions: out}
	// The remainder exists only when the table filled to its cap; below it, every
	// windowed session already shows and the footer would be empty.
	if len(out) == windowSessionLimit {
		if s.windowSessionRowsReadHook != nil {
			s.windowSessionRowsReadHook()
		}
		rem, err := s.windowSessionRemainder(ctx, query, where, args)
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
// sums what is left: per-class tokens, cost, and the count.
func (s *Store) windowSessionRemainder(ctx context.Context, query querier, where string, args []any) (SessionRemainder, error) {
	q := `
		SELECT coalesce(count(*), 0),
		       coalesce(sum(t.input), 0), coalesce(sum(t.output), 0),
		       coalesce(sum(t.cache_read), 0), coalesce(sum(t.cache_write), 0),
		       coalesce(sum(t.cost), 0)
		  FROM (
		       SELECT coalesce(sum(ue.input_tokens), 0) AS input,
		              coalesce(sum(ue.output_tokens), 0) AS output,
		              coalesce(sum(ue.cache_read_tokens), 0) AS cache_read,
		              coalesce(sum(ue.cache_write_tokens), 0) AS cache_write,
		              coalesce(sum(ue.cost_usd), 0) AS cost
		         FROM usage_events ue
		         JOIN sessions s ON s.id = ue.session_id
		         JOIN users u ON u.id = s.user_id
		        WHERE ` + where + `
		        GROUP BY s.id
		        ORDER BY s.last_active_at DESC, s.id DESC
		        OFFSET $` + itoa(len(args)+1) + `
		  ) t`
	remArgs := append(append([]any{}, args...), windowSessionLimit)
	var r SessionRemainder
	if err := query.QueryRow(ctx, q, remArgs...).Scan(
		&r.Sessions, &r.Input, &r.Output, &r.CacheRead, &r.CacheWrite, &r.CostUSD,
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
func globalSessionSelect(matchLateral, matchCol, matchCutCol string) string {
	return `
	SELECT s.id, s.agent, s.machine, s.git_branch, u.username,
	       s.message_count, s.user_message_count, s.model_fallback_count,
	       s.total_input_tokens, s.total_output_tokens,
	       s.total_cache_write_tokens, s.total_cache_read_tokens,
	       s.total_cost_usd, s.visibility, s.public_id,
	       s.started_at, s.ended_at, s.last_active_at,
	       p.id, p.remote_key, p.display_name, p.kind,
	       sig.grade, sig.outcome,
	       coalesce(title.content, ''), ` + matchCol + `, ` + matchCutCol + `
	  FROM sessions s
	  JOIN users u ON u.id = s.user_id
	  JOIN projects p ON p.id = s.project_id
	  LEFT JOIN session_signals sig ON sig.session_id = s.id AND ` + signalsCurrent() + `
	  ` + titleLateralSQL + matchLateral
}

// scanSessionRow reads one cross-project row. matchActive reports whether the query
// selected the match-content and front-cut columns (a live content search), so the
// scan targets them and the row shape matches the SELECT. The raw (SQL-windowed)
// match content and the front-cut flag stay local: the caller windows them into the
// row's snippet. frontCut reports whether the SQL window dropped leading content, so
// the snippet builder prepends an ellipsis rather than reading the window's start as
// the message's start.
func scanSessionRow(rows pgx.Rows, matchActive bool) (r SessionRow, raw string, frontCut bool, err error) {
	var match *string
	var cut bool
	// outcome is nullable in the row: a session with no current signals row (unsettled,
	// or its signals gone stale under a newer epoch) LEFT JOINs to NULL. Scan through a
	// pointer and fold a missing outcome to the empty string the row field documents.
	var outcome *string
	dest := []any{&r.ID, &r.Agent, &r.Machine, &r.GitBranch, &r.Username,
		&r.MessageCount, &r.UserMessageCount, &r.ModelFallbackCount,
		&r.TotalInput, &r.TotalOutput, &r.TotalCacheWrite, &r.TotalCacheRead,
		&r.TotalCostUSD, &r.Visibility, &r.PublicID,
		&r.StartedAt, &r.EndedAt, &r.LastActiveAt,
		&r.ProjectID, &r.ProjectKey, &r.ProjectName, &r.ProjectKind,
		&r.Grade, &outcome,
		&r.Title, &match, &cut}
	if err := rows.Scan(dest...); err != nil {
		return r, "", false, fmt.Errorf("scan global session row: %w", err)
	}
	if outcome != nil {
		r.Outcome = *outcome
	}
	r.Title = squashSpaces(r.Title)
	if matchActive && match != nil {
		raw = *match
		frontCut = cut
	}
	return r, raw, frontCut, nil
}

// ListAllSessions returns one page of sessions across every project matching the
// filter, newest first, and whether more rows match beyond the page. A zero
// ProjectID means "all projects"; the other fields narrow the set exactly as
// ListSessions does. This backs the global Sessions view and the Overview's
// recent-activity feed.
//
// The page is bounded by the filter's Limit, and the hasMore return reports whether
// at least one more row matches past it: the query asks for limit+1 rows and, when it
// comes back full, trims the extra and flags hasMore. The handler thus learns "is
// there a next page" without a second count(*) over the whole matching history, so
// the render cost stays linear in the page rather than the corpus.
//
// Every row carries its first-user-message Title. When Query is set, each row also
// carries a SearchSnippet windowed around the first match. The match window is bounded
// in SQL (see snippetSQLWindowLen): the LATERAL locates the match with strpos and
// selects only a substring around it, so a huge message never rides back whole just to
// yield a short snippet. Go finishes the windowing so the offsets are exact byte
// positions the template can split on.
func (s *Store) ListAllSessions(ctx context.Context, f SessionFilter) (rows []SessionRow, hasMore bool, err error) {
	// Bound Since on s.started_at, the same column the Insights panels window their
	// cohorts by (qualityDistributionFrom in analytics_quality.go and the concurrency
	// scans in analytics_concurrency.go both clauseFor("s.started_at")). A drill-through
	// from a panel bar thus lands on exactly the sessions that bar counted: a session
	// started before the window but bumped inside it by a late reparse stays out of both,
	// where an updated_at bound would have pulled it into the feed but not the panel.
	conds, args := f.conds("s.started_at")

	if !f.IncludeSubagents {
		// Hide subagent sessions from the browse feed by default. This lives here, not in
		// the shared conds(), so CountAllSessions, the facet probes, the MCP SessionFeed,
		// and the Insights drill-through invariants keep counting every session; only this
		// list narrows to top-level work. relationship_type is NOT NULL (default ''), so a
		// plain inequality is null-safe and needs no placeholder. The top-level partial
		// indexes match this exact predicate so the page still walks an index rather than
		// scanning subagent rows: 0046's global sort orders for the unfaceted feed, and
		// 0047's per-facet twins for the default recency order under a project, user, agent,
		// or machine filter.
		conds = append(conds, "s.relationship_type <> 'subagent'")
	}

	// The match lateral reuses the escaped ILIKE pattern conds() already appended as
	// the last arg (so the row it snippets is the row the EXISTS filter matched), and
	// additionally binds the raw query for strpos, which wants the literal substring,
	// not the LIKE-escaped pattern. strpos over lower()'d content finds the match byte
	// offset so the substring can window around it; a 0 (the rare case where lower()
	// case-folding disagrees with the ILIKE match position on some Unicode) falls back
	// to the head of the message, and Go's fold-insensitive search finds or misses it
	// gracefully rather than anchoring on a wrong offset.
	matchLateral, matchCol, matchCutCol := "", "NULL", "false"
	searching := f.Query != ""
	if searching {
		patternPlaceholder := "$" + itoa(len(args))
		args = append(args, f.Query)
		rawPlaceholder := "$" + itoa(len(args))
		radius := itoa(snippetSQLWindowRadius)
		length := itoa(snippetSQLWindowLen)
		// pos is the 1-based match offset (0 when the fold disagrees); the window starts
		// radius bytes before it, floored at 1. front_cut reports whether the window
		// dropped leading content, so Go prepends an ellipsis and does not read the
		// window's start as the message's start.
		matchLateral = `
	  LEFT JOIN LATERAL (
	         SELECT substring(m.content from greatest(1, strpos(lower(m.content), lower(` + rawPlaceholder + `)) - ` + radius + `) for ` + length + `) AS content,
	                strpos(lower(m.content), lower(` + rawPlaceholder + `)) > ` + itoa(snippetSQLWindowRadius+1) + ` AS front_cut
	           FROM messages m
	          WHERE m.session_id = s.id AND m.content ILIKE ` + patternPlaceholder + `
	          ORDER BY m.ordinal LIMIT 1
	       ) match ON true`
		matchCol = "match.content"
		matchCutCol = "coalesce(match.front_cut, false)"
	}

	// Keyset pagination: "Show more" resumes strictly after the last row the reader saw
	// (f.After), so a deep page reads the next slice off the (col, id) index instead of
	// re-scanning rows 1..N under a doubled limit. The cursor id binds to the next arg
	// slot, so append it only when the predicate actually applies (a keyset-sortable order
	// with a cursor set).
	if cond, kargs, ok := f.keysetCond(len(args) + 1); ok {
		args = append(args, kargs...)
		conds = append(conds, cond)
	}

	q := globalSessionSelect(matchLateral, matchCol, matchCutCol)
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += f.orderClause()
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// Ask for one row past the page so a full extra row means "more match" without a
	// separate count over the history. The extra row is trimmed before returning.
	args = append(args, limit+1)
	q += " LIMIT $" + itoa(len(args))
	if f.Offset > 0 {
		args = append(args, f.Offset)
		q += " OFFSET $" + itoa(len(args))
	}

	// Read the page and its fan-out rollups in one read-only repeatable-read snapshot. The row
	// summaries carry each session's own cost, and attachTreeRollups reads the whole-work-item
	// cost (root plus subagent subtree) in a second query; a rebuild landing between the two
	// pool reads would let the row cost and the fan-out chip describe different snapshots of the
	// same subtree. One transaction pins both to a single MVCC view. Read-only, so it takes no
	// locks and never blocks ingest; the keyset cursor's live value lookup (when a legacy cursor
	// carries no observed value) also resolves against this same snapshot.
	err = pgx.BeginTxFunc(ctx, s.Pool,
		pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			qrows, qerr := tx.Query(ctx, q, args...)
			if qerr != nil {
				return fmt.Errorf("query global sessions: %w", qerr)
			}
			var page []SessionRow
			for qrows.Next() {
				r, raw, frontCut, serr := scanSessionRow(qrows, searching)
				if serr != nil {
					qrows.Close()
					return serr
				}
				if searching {
					r.Search = buildSnippet(raw, f.Query, frontCut)
				}
				page = append(page, r)
			}
			if ierr := qrows.Err(); ierr != nil {
				qrows.Close()
				return fmt.Errorf("iterate global sessions: %w", ierr)
			}
			// Free the connection for the rollup query on this same transaction: pgx allows one
			// active query per connection at a time, so the page must be fully drained first.
			qrows.Close()
			// A full extra row past the page is the "more match" signal: drop it and flag
			// hasMore so the footer can offer the next page without counting the corpus.
			if len(page) > limit {
				page = page[:limit]
				hasMore = true
			}
			// Fold each row's subagent subtree into a whole-work-item rollup so the feed can
			// show the fan-out a root turn hides. One batch query over the trimmed page, after
			// the hasMore probe row is dropped, so the extra row never costs a walk.
			if terr := s.attachTreeRollups(ctx, tx, page); terr != nil {
				return terr
			}
			rows = page
			return nil
		})
	if err != nil {
		return nil, false, err
	}
	return rows, hasMore, nil
}

// CountAllSessions counts the sessions ListAllSessions would return for the same
// filter (ignoring Limit and Offset), plus how many empty sessions the default
// hides. It shares conds() with ListAllSessions so the two can never disagree about
// which rows match.
//
// It exists for tests and verification only, NOT the render path: the /sessions
// footer no longer counts the corpus (that count(*) was O(total), the very cost the
// incremental-efficiency gate flagged), so nothing in a request calls this. The
// drift-guard tests keep it to pin the drill filters' bucket semantics against the
// Insights panel counts, and TestCountAllSessionsAgreement uses it to hold the
// list-vs-count invariant the shared conds() guarantees.
//
// The empty count is a FILTER aggregate over the same matched set, so it reflects
// the sessions hidden BY the empty filter within the current agent/project/query
// scope, not a fleet-wide zero-message count. When IncludeEmpty is set the empty
// rows are already in Total, and Empty reports how many of them are the
// zero-message ones.
func (s *Store) CountAllSessions(ctx context.Context, f SessionFilter) (total, empty int, err error) {
	// Count over the same match set as the list, but neutralize the empty-hiding so
	// the FILTER can report the hidden count regardless of the current toggle: run
	// conds with IncludeEmpty forced on, then let the FILTER split the empty ones out.
	cf := f
	cf.IncludeEmpty = true
	// Bound on s.started_at to match ListAllSessions (and the Insights panels), so the
	// drift-guard tests pin the same windowed cohort the list and the bars count.
	conds, args := cf.conds("s.started_at")

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

// HasEmptySessions reports whether the filter's scope holds at least one empty
// (zero-message) session, so the footer can decide whether the empty-hidden toggle
// would change anything without counting how many. It exists so the /sessions render
// path stays bounded: the old toggle carried a count K that was the same O(total)
// aggregate the footer no longer runs, and the toggle only needs the yes/no.
//
// The probe is EXISTS over the filter's other conditions with message_count = 0
// added, so Postgres stops at the first matching row (index-bounded, O(1)-ish)
// instead of scanning the scope. It forces IncludeEmpty on so the empty rows are in
// scope to be found regardless of the current toggle: the caller asks "are there
// empties here", not "does the current list include them".
func (s *Store) HasEmptySessions(ctx context.Context, f SessionFilter) (bool, error) {
	cf := f
	cf.IncludeEmpty = true
	// Bound on s.started_at to match ListAllSessions, so the toggle reflects empties in
	// the same windowed scope the list draws from.
	conds, args := cf.conds("s.started_at")
	conds = append(conds, "s.message_count = 0")
	// Mirror ListAllSessions' default subagent hiding (it lives outside conds() there, so it
	// must be added here too): the toggle drives that same list, so an empty row it could never
	// reveal must not make the toggle appear. Without this, a hidden empty subagent would offer
	// an empty=1 toggle that still shows nothing.
	if !f.IncludeSubagents {
		conds = append(conds, "s.relationship_type <> 'subagent'")
	}
	q := `SELECT EXISTS (SELECT 1
	        FROM sessions s
	        JOIN users u ON u.id = s.user_id
	        JOIN projects p ON p.id = s.project_id
	       WHERE ` + strings.Join(conds, " AND ") + `)`
	var exists bool
	if err := s.Pool.QueryRow(ctx, q, args...).Scan(&exists); err != nil {
		return false, fmt.Errorf("probe empty sessions: %w", err)
	}
	return exists, nil
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
//
// The Since bound windows on s.started_at, the same column ListAllSessions and
// CountAllSessions use, so SessionFilter.Since has ONE meaning across every query that
// shares the filter: "started in the window". A session started before the window but
// bumped inside it is out of all three consistently, rather than in the MCP feed but
// out of the web drill. The keyset still pages on id (immutable), so the window column
// choice does not affect paging stability.
func (s *Store) SessionFeed(ctx context.Context, f SessionFilter, limit int, cursor *SessionFeedCursor) ([]SessionRow, *SessionFeedCursor, error) {
	conds, args := f.conds("s.started_at")
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
	q := globalSessionSelect("", "NULL", "false")
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
		r, _, _, err := scanSessionRow(rows, false)
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

// GlobalFacets returns the busiest agents, machines, usernames, and projects
// across all sessions, each with its session count, ordered busiest first. The
// counts are read from the session_facets rollup (maintained incrementally by a
// trigger on the sessions table, see migration 0005), so each category is a
// bounded top-N index read rather than a GROUP BY over the whole sessions table.
// It backs the global Sessions view's filter rail.
//
// Reconciliation gap (intentional and pinned): the rollup counts EVERY session,
// including the empty (message_count = 0) parse-failures the default feed hides. So a
// facet's count is the whole-corpus count and can exceed the rows a default-feed click
// on that facet lists, by exactly the empty sessions carrying that value. Clicking the
// facet and then showing empties (the footer's toggle, empty=1) reconciles them: the
// facet count equals the IncludeEmpty feed count for that facet. The gap is not closed
// in the rollup on purpose: the facet trigger fires only on the facet columns (agent,
// machine, user_id, project_id), never on message_count (migration 0005's UPDATE OF
// clause deliberately excludes it so live ingest's per-message projection updates do
// not churn the rollup). A message_count-aware rollup would have to fire on every
// ingest append, reintroducing exactly that churn. The relationship
// (facet_count == IncludeEmpty feed count >= default feed count) is pinned by
// TestGlobalFacetsReconcileEmptyHidden so it cannot drift silently.
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
