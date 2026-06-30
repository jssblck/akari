package store

import (
	"strings"
	"testing"
)

// TestOrderClause locks the two properties that let the global session list walk
// an index instead of sorting the whole history: the ORDER BY carries no NULLS
// placement (which would defeat the index match, since every sortable expression
// is NOT NULL) and the id tiebreak follows the column's direction (so one (col,
// id) btree serves both the ascending and descending header clicks). It is a
// white-box guard against silently reintroducing "NULLS LAST", which once defeated
// even the default feed order.
func TestOrderClause(t *testing.T) {
	t.Parallel()

	// An empty or unknown Sort is the descending updated feed order, id descending.
	for _, sort := range []string{"", "nonsense", "; drop table sessions"} {
		got := SessionFilter{Sort: sort}.orderClause()
		if want := " ORDER BY s.updated_at DESC, s.id DESC"; got != want {
			t.Errorf("default orderClause(sort=%q) = %q, want %q", sort, got, want)
		}
	}

	for key, expr := range sessionSortColumns {
		for _, desc := range []bool{false, true} {
			got := SessionFilter{Sort: key, Desc: desc}.orderClause()
			if strings.Contains(got, "NULLS") {
				t.Errorf("orderClause(%q, desc=%v) = %q: must not place NULLS (it defeats the index)", key, desc, got)
			}
			dir := "ASC"
			if desc {
				dir = "DESC"
			}
			if want := " ORDER BY " + expr + " " + dir + ", s.id " + dir; got != want {
				t.Errorf("orderClause(%q, desc=%v) = %q, want %q", key, desc, got, want)
			}
		}
	}
}
