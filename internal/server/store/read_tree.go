package store

import (
	"context"
	"fmt"
)

// TreeRollup is the whole-work-item view of a top-level session: its own cost plus
// every subagent it fanned out, folded together. A feed row shows its session's own
// tokens and cost, but a single prompt can spawn dozens of subagents whose spend is
// invisible at the row. The rollup names that hidden fan-out so a reader auditing
// where the money went sees the work item's true footprint, not just its root turn.
//
// The zero value (no subagents, zero cost) is the correct reading for a session that
// spawned nothing, so a row with no fan-out simply carries no rollup chip.
type TreeRollup struct {
	// SubagentCount is how many subagent sessions the root fanned out, counted over
	// the whole descendant subtree (a subagent that itself spawns subagents adds all
	// of them), not just the root's direct children.
	SubagentCount int
	// CostUSD is the summed cost of the root and every subagent in its subtree: the
	// price of the whole work item, which the row's own cost understates whenever the
	// session delegated work to subagents.
	CostUSD float64
	// CostIncomplete is true when any session folded into the subtree could not be
	// fully priced (an unknown model, a missing rate), so the rolled-up cost is a
	// floor rather than an exact figure and the UI can mark it approximate.
	CostIncomplete bool
}

// treeRollupSQL walks each root's subagent subtree in one recursive pass and folds
// the subtree into a per-root rollup. The recursion follows only 'subagent' edges,
// never 'continuation': a continuation is its own work item with its own feed row,
// so folding it into the session it continued would double-count it across two rows.
// Each session has exactly one parent, so a subagent belongs to exactly one root's
// subtree and no cost is counted twice across the page's roots.
const treeRollupSQL = `
WITH RECURSIVE tree AS (
    SELECT id AS root, id AS node, 0 AS depth
      FROM sessions
     WHERE id = ANY($1)
    UNION ALL
    SELECT t.root, c.id, t.depth + 1
      FROM sessions c
      JOIN tree t ON c.parent_session_id = t.node
     WHERE c.relationship_type = 'subagent'
)
SELECT t.root,
       count(*) FILTER (WHERE t.depth > 0)  AS subagents,
       coalesce(sum(s.total_cost_usd), 0)   AS tree_cost,
       coalesce(bool_or(s.cost_incomplete), false) AS tree_incomplete
  FROM tree t
  JOIN sessions s ON s.id = t.node
 GROUP BY t.root`

// treeRollups computes the subagent-subtree rollup for each of the given root session
// ids in a single query. A root with no subagents is still returned (its rollup is
// just its own cost with a zero count), so the caller can look up every id it asked
// for. Ids that name no session are absent from the map. It is a no-op on an empty
// slice, so a caller need not guard the empty page.
func (s *Store) treeRollups(ctx context.Context, q querier, ids []int64) (map[int64]TreeRollup, error) {
	if len(ids) == 0 {
		return map[int64]TreeRollup{}, nil
	}
	rows, err := q.Query(ctx, treeRollupSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("query tree rollups: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]TreeRollup, len(ids))
	for rows.Next() {
		var root int64
		var tr TreeRollup
		if err := rows.Scan(&root, &tr.SubagentCount, &tr.CostUSD, &tr.CostIncomplete); err != nil {
			return nil, fmt.Errorf("scan tree rollup: %w", err)
		}
		out[root] = tr
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tree rollups: %w", err)
	}
	return out, nil
}

// attachTreeRollups fills each row's Tree field with its subagent-subtree rollup,
// computed for the whole page in one query on the given querier. It mutates the rows in
// place. A row whose id has no rollup (it named no live session) keeps the zero-value
// rollup, which reads as a session that fanned out nothing. It is a no-op on an empty
// page. The caller passes the same transaction the row summaries were read on, so a row's
// own cost and its fan-out rollup describe one snapshot even if a rebuild lands mid-read.
func (s *Store) attachTreeRollups(ctx context.Context, q querier, rows []SessionRow) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]int64, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	roll, err := s.treeRollups(ctx, q, ids)
	if err != nil {
		return err
	}
	for i := range rows {
		rows[i].Tree = roll[rows[i].ID]
	}
	return nil
}
