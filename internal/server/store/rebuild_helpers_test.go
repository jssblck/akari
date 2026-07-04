package store_test

import (
	"context"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// testEpoch is the parser epoch tests stamp when they rebuild through a stub
// reducer. Its value is arbitrary: nothing in these tests compares it against
// the binary's parse.Epoch, they only need rebuilds to stamp something.
const testEpoch = 1

// stubReducer satisfies store.SessionReducer with a canned whole-session delta,
// ignoring whatever raw bytes the rebuild feeds it. It is the seam tests use to
// seed a projection without composing real transcript bytes.
type stubReducer struct{ delta store.ProjectionDelta }

func (r stubReducer) Feed([]byte, int64) error      { return nil }
func (r stubReducer) Finish() store.ProjectionDelta { return r.delta }

// rebuildWith replaces sid's whole projection with the canned delta, the way a
// real parse would. Announce already created the session_raw row the rebuild
// locks, so this works on any announced session even before a chunk lands.
//
// Two rebuild side effects matter to callers. The rollups and prompt facts are
// derived from the delta's rows, so counts never need pre-computing. And the
// rebuild re-grades signals: a session that is settled (delta.Ended past the
// abandoned window) or terminal gets a fresh session_signals row and
// signals_stale = false, while a live session has any existing signals row
// DELETED and signals_stale left true. A test that hand-inserts a signals row
// must therefore insert it after its last rebuild, and pair it with
// markSignalsFresh (signals_test.go) when the read it exercises gates on
// current signals.
func rebuildWith(t testing.TB, st *store.Store, sid int64, d store.ProjectionDelta) {
	t.Helper()
	if err := st.RebuildSession(context.Background(), sid, testEpoch, stubReducer{d}); err != nil {
		t.Fatalf("rebuild session %d: %v", sid, err)
	}
}
