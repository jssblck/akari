package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestRebuildSessionIsAtomic confirms a rebuild is all-or-nothing on an
// operational failure: an error partway through the rewrite (here a tool call
// referencing a blob the CAS never received) rolls back the clear, so the
// session keeps its prior projection and its bookkeeping rather than being left
// empty, and the next drain retries it. This is the property the worker relies
// on to treat operational failures as retryable without data loss. The other
// failure class, a deterministic reducer error, deliberately commits its stamp
// instead; TestRebuildSessionParserErrorStamps pins that one.
func TestRebuildSessionIsAtomic(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid.ID, Agent: "claude", SourceSessionID: "s-atomic", ProjectID: pid,
		GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	sid := ann.SessionID
	if _, err := st.AppendChunk(ctx, sid, 0, []byte("one transcript line\n")); err != nil {
		t.Fatalf("append: %v", err)
	}

	emit := func(content string) store.ProjectionDelta {
		return store.ProjectionDelta{
			Messages: []store.MessageDelta{{Ordinal: 0, Role: "user", Content: content}},
		}
	}
	readMessage := func() (count int, content string) {
		if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM messages WHERE session_id = $1", sid).Scan(&count); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		if count == 1 {
			if err := st.Pool.QueryRow(ctx, "SELECT content FROM messages WHERE session_id = $1", sid).Scan(&content); err != nil {
				t.Fatalf("read message: %v", err)
			}
		}
		return count, content
	}
	parsedLen := func() int64 {
		var n int64
		if err := st.Pool.QueryRow(ctx, "SELECT parsed_byte_len FROM session_raw WHERE session_id = $1", sid).Scan(&n); err != nil {
			t.Fatalf("read cursor: %v", err)
		}
		return n
	}

	// Build the original projection.
	rebuildWith(t, st, sid, emit("original"))
	if n, c := readMessage(); n != 1 || c != "original" {
		t.Fatalf("after initial rebuild got %d message(s) %q, want 1 %q", n, c, "original")
	}
	cursor := parsedLen()
	if cursor == 0 {
		t.Fatal("cursor should have advanced past zero after the initial rebuild")
	}

	// A rewrite that fails mid-transaction must leave the original projection
	// untouched. The failing delta parses fine (the reducer accepts it) but its
	// tool call references a lifted input body that was never uploaded, so
	// writeToolCalls fails after the old rows were already deleted; only the
	// rollback keeps that delete invisible.
	failing := emit("would replace")
	failing.ToolCalls = []store.ProjToolCall{{
		MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", Category: "read",
		InputSHA256: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		CallUID:     "missing-blob",
	}}
	if err := st.RebuildSession(ctx, sid, testEpoch, stubReducer{failing}); !errors.Is(err, store.ErrBlobNotUploaded) {
		t.Fatalf("failing rebuild error = %v, want ErrBlobNotUploaded", err)
	}
	if n, c := readMessage(); n != 1 || c != "original" {
		t.Fatalf("after a failed rebuild got %d message(s) %q, want the original 1 %q preserved", n, c, "original")
	}
	if got := parsedLen(); got != cursor {
		t.Fatalf("a failed rebuild moved the cursor to %d, want it left at %d", got, cursor)
	}

	// A successful rebuild replaces the projection.
	rebuildWith(t, st, sid, emit("rebuilt"))
	if n, c := readMessage(); n != 1 || c != "rebuilt" {
		t.Fatalf("after a successful rebuild got %d message(s) %q, want 1 %q", n, c, "rebuilt")
	}
}

// TestDueSessionsKeysetScan pins the worker's due scan: a fresh corpus is
// entirely due (parser_epoch 0 sits behind every real epoch), the scan pages
// by id with a strict keyset cursor, carries each session's agent, and a
// rebuilt session drops out while new bytes or a later running epoch bring
// it back.
func TestDueSessionsKeysetScan(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	announce := func(agent, src string) int64 {
		ann, err := st.Announce(ctx, store.AnnounceParams{
			UserID: uid.ID, Agent: agent, SourceSessionID: src, ProjectID: pid,
		})
		if err != nil {
			t.Fatalf("announce %s: %v", src, err)
		}
		return ann.SessionID
	}
	s1 := announce("claude", "due-1")
	s2 := announce("codex", "due-2")
	s3 := announce("claude", "due-3")

	// Page through with limit 1: every page advances strictly, and the union is
	// the whole corpus with the right agents.
	agents := map[int64]string{}
	var afterID int64
	for {
		page, err := st.DueSessions(ctx, testEpoch, afterID, 1)
		if err != nil {
			t.Fatalf("due page after %d: %v", afterID, err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) != 1 {
			t.Fatalf("limit 1 returned %d rows", len(page))
		}
		if page[0].ID <= afterID {
			t.Fatalf("keyset did not advance: got id %d after cursor %d", page[0].ID, afterID)
		}
		afterID = page[0].ID
		agents[page[0].ID] = page[0].Agent
	}
	want := map[int64]string{s1: "claude", s2: "codex", s3: "claude"}
	if len(agents) != len(want) {
		t.Fatalf("due scan found %d sessions, want %d: %v", len(agents), len(want), agents)
	}
	for id, agent := range want {
		if agents[id] != agent {
			t.Errorf("session %d agent = %q, want %q", id, agents[id], agent)
		}
	}

	// Rebuilding takes a session out of the due set; the other two remain.
	rebuildWith(t, st, s1, store.ProjectionDelta{})
	due, err := st.DueSessions(ctx, testEpoch, 0, 100)
	if err != nil {
		t.Fatalf("due scan: %v", err)
	}
	ids := map[int64]bool{}
	for _, d := range due {
		ids[d.ID] = true
	}
	if ids[s1] || !ids[s2] || !ids[s3] {
		t.Fatalf("after rebuilding s1, due = %v, want exactly s2 and s3", ids)
	}

	// New bytes bring a rebuilt session back (byte-dirtiness), and a later
	// running epoch makes even a byte-clean one due again.
	if _, err := st.AppendChunk(ctx, s1, 0, []byte("new line\n")); err != nil {
		t.Fatalf("append: %v", err)
	}
	due, err = st.DueSessions(ctx, testEpoch, 0, 100)
	if err != nil {
		t.Fatalf("due scan: %v", err)
	}
	found := false
	for _, d := range due {
		found = found || d.ID == s1
	}
	if !found {
		t.Fatal("byte-dirty session missing from the due set")
	}
}

// TestMarkEpochStaleAndCount pins the fleet-rebuild triggers: MarkEpochStale
// forces a scope (one agent, or everything) back into the due set by resetting
// its stored epoch, and EpochStaleCount reports exactly the sessions behind the
// running epoch (the figure the UI gate and the progress bar key on).
func TestMarkEpochStaleAndCount(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	announce := func(agent, src string) int64 {
		ann, err := st.Announce(ctx, store.AnnounceParams{
			UserID: uid.ID, Agent: agent, SourceSessionID: src, ProjectID: pid,
		})
		if err != nil {
			t.Fatalf("announce %s: %v", src, err)
		}
		return ann.SessionID
	}
	claude1 := announce("claude", "m-1")
	claude2 := announce("claude", "m-2")
	codex := announce("codex", "m-3")

	// A fresh corpus sits at epoch 0, so everything is stale relative to a real
	// epoch, and a rebuilt corpus counts zero.
	if n, err := st.EpochStaleCount(ctx, testEpoch); err != nil || n != 3 {
		t.Fatalf("fresh corpus stale count = %d (err %v), want 3", n, err)
	}
	for _, sid := range []int64{claude1, claude2, codex} {
		rebuildWith(t, st, sid, store.ProjectionDelta{})
	}
	if n, err := st.EpochStaleCount(ctx, testEpoch); err != nil || n != 0 {
		t.Fatalf("rebuilt corpus stale count = %d (err %v), want 0", n, err)
	}

	// Marking one agent stale touches only its sessions.
	marked, err := st.MarkEpochStale(ctx, "claude")
	if err != nil {
		t.Fatalf("mark claude stale: %v", err)
	}
	if marked != 2 {
		t.Fatalf("marked %d sessions, want the 2 claude ones", marked)
	}
	if n, _ := st.EpochStaleCount(ctx, testEpoch); n != 2 {
		t.Fatalf("stale count after agent mark = %d, want 2", n)
	}
	due, err := st.DueSessions(ctx, testEpoch, 0, 100)
	if err != nil {
		t.Fatalf("due scan: %v", err)
	}
	ids := map[int64]bool{}
	for _, d := range due {
		ids[d.ID] = true
	}
	if !ids[claude1] || !ids[claude2] || ids[codex] {
		t.Fatalf("agent-scoped mark made the wrong sessions due: %v", ids)
	}

	// Marking everything covers the remaining agent too, and re-marking an
	// already-stale session is harmless (the count does not double).
	marked, err = st.MarkEpochStale(ctx, "")
	if err != nil {
		t.Fatalf("mark all stale: %v", err)
	}
	if marked != 3 {
		t.Fatalf("marked %d sessions, want all 3", marked)
	}
	if n, _ := st.EpochStaleCount(ctx, testEpoch); n != 3 {
		t.Fatalf("stale count after fleet mark = %d, want 3", n)
	}
}

// TestEpochGatesAgreeWithDueScan pins the two properties the fleet gates
// (EpochStaleCount, EpochStaleExists, and the analytics snapshot gate that
// embeds the same predicate) share with DueSessions.
//
// Monotonicity: a session stamped ahead of the running epoch (the rolling
// deploy case, where an old binary looks at a newer binary's stamp) is neither
// due nor counted stale, even when byte-dirty. The old binary must not rebuild
// it (that would downgrade the projection to the older parser), and it must
// not gate on it either, since its drain will never touch the session.
//
// Failure pinning: a deterministic failure at the running epoch removes the
// session from the due set and the gates together, and new bytes readmit it to
// both at once. If the gates excluded failures on the epoch alone, a
// failed-then-appended session would be due (the worker has a concrete rebuild
// path at the running epoch) while the gates declared the corpus current.
func TestEpochGatesAgreeWithDueScan(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid.ID, Agent: "claude", SourceSessionID: "gates-agree", ProjectID: pid,
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	sid := ann.SessionID
	raw := "first line\n"
	if _, err := st.AppendChunk(ctx, sid, 0, []byte(raw)); err != nil {
		t.Fatalf("append: %v", err)
	}
	rebuildWith(t, st, sid, store.ProjectionDelta{})

	check := func(epoch int, wantDue, wantStale bool, when string) {
		t.Helper()
		due, err := st.DueSessions(ctx, epoch, 0, 100)
		if err != nil {
			t.Fatalf("%s: due scan: %v", when, err)
		}
		isDue := false
		for _, d := range due {
			isDue = isDue || d.ID == sid
		}
		if isDue != wantDue {
			t.Fatalf("%s: due at epoch %d = %v, want %v", when, epoch, isDue, wantDue)
		}
		n, err := st.EpochStaleCount(ctx, epoch)
		if err != nil {
			t.Fatalf("%s: stale count: %v", when, err)
		}
		exists, err := st.EpochStaleExists(ctx, epoch)
		if err != nil {
			t.Fatalf("%s: stale exists: %v", when, err)
		}
		if (n != 0) != wantStale || exists != wantStale {
			t.Fatalf("%s: stale gates at epoch %d = count %d, exists %v, want stale=%v", when, epoch, n, exists, wantStale)
		}
	}

	// Monotonicity: stamped at testEpoch, an earlier running epoch sees nothing,
	// byte-clean or byte-dirty.
	check(testEpoch-1, false, false, "byte-clean, stamped ahead")
	if _, err := st.AppendChunk(ctx, sid, int64(len(raw)), []byte("second line\n")); err != nil {
		t.Fatalf("append: %v", err)
	}
	check(testEpoch-1, false, false, "byte-dirty, stamped ahead")
	// At the stamping epoch the append is due, but byte-dirtiness alone never
	// counts as epoch-stale (a live session's pending append is steady state,
	// not a fleet rebuild).
	check(testEpoch, true, false, "byte-dirty at own epoch")
	rebuildWith(t, st, sid, store.ProjectionDelta{})

	// Failure pinning under a bumped epoch: the session is due and counted until
	// the failure pins it, and new bytes readmit it to the scan and the gates in
	// the same instant.
	check(testEpoch+1, true, true, "behind a bumped epoch")
	rerr := errors.New("malformed transcript")
	if err := st.RebuildSession(ctx, sid, testEpoch+1, failingReducer{rerr}); !errors.Is(err, rerr) {
		t.Fatalf("failing rebuild returned %v, want the reducer's error", err)
	}
	check(testEpoch+1, false, false, "failure pinned at the bumped epoch")
	var byteLen int64
	if err := st.Pool.QueryRow(ctx, `SELECT byte_len FROM session_raw WHERE session_id = $1`, sid).Scan(&byteLen); err != nil {
		t.Fatalf("read byte_len: %v", err)
	}
	if _, err := st.AppendChunk(ctx, sid, byteLen, []byte("third line\n")); err != nil {
		t.Fatalf("append: %v", err)
	}
	check(testEpoch+1, true, true, "new bytes after the pinned failure")
}
