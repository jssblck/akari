package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// seedForReads provisions a user, a remote project, and a session (with a known cwd so tool paths
// under it derive a relative form), returning the session id.
func seedForReads(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	return seedSession(t, st, u.ID, projectID, "sess-reads")
}

// msgByOrdinal indexes a message slice by ordinal for assertion.
func msgByOrdinal(msgs []store.Message) map[int]store.Message {
	m := map[int]store.Message{}
	for _, msg := range msgs {
		m[msg.Ordinal] = msg
	}
	return m
}

// TestMessagesTurnUsageFolds pins that each message's folded Usage sums a turn's streamed chunks,
// computes context occupancy as input + cache read + cache write (output excluded), leaves cost
// NULL only when every contributing row is unpriced (a lower-bound partial when some rows are
// priced), and that a NULL-ordinal usage row lands on no message's Usage.
func TestMessagesTurnUsageFolds(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st)

	ord0, ord1 := 0, 1
	cost := func(f float64) *float64 { return &f }
	// Two message turns, each with usage. Turn 0: two streamed chunks, both priced. Turn 1: one
	// priced row, one unpriced row (a mixed group, so the cost is a priced partial). A usage row with
	// no ordinal must not land on any message's Usage.
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "assistant", Content: "a", Model: "gpt-5"},
			{Ordinal: 1, Role: "assistant", Content: "b", Model: "gpt-5"},
		},
		Usage: []store.ProjUsage{
			{MessageOrdinal: &ord0, Model: "gpt-5", Input: 1000, Output: 200, CacheRead: 5000, CacheWrite: 300, Reasoning: 40, CostUSD: cost(0.10), SourceOffset: 1, SourceIndex: 0},
			{MessageOrdinal: &ord0, Model: "gpt-5", Input: 500, Output: 100, CacheRead: 2000, CacheWrite: 0, Reasoning: 10, CostUSD: cost(0.05), SourceOffset: 2, SourceIndex: 0},
			{MessageOrdinal: &ord1, Model: "gpt-5", Input: 800, Output: 400, CacheRead: 100000, CacheWrite: 0, CostUSD: cost(0.30), SourceOffset: 3, SourceIndex: 0},
			{MessageOrdinal: &ord1, Model: "mystery", Input: 100, Output: 50, CacheRead: 0, CacheWrite: 0, CostUSD: nil, SourceOffset: 4, SourceIndex: 0},
			{MessageOrdinal: nil, Model: "gpt-5", Input: 999, Output: 999, CostUSD: cost(9.99), SourceOffset: 5, SourceIndex: 0},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}

	msgs, err := st.Messages(ctx, sid)
	if err != nil {
		t.Fatalf("read messages: %v", err)
	}
	by := msgByOrdinal(msgs)

	m0 := by[0]
	if m0.Usage == nil {
		t.Fatalf("turn 0 carries no folded usage")
	}
	u0 := *m0.Usage
	if u0.Input != 1500 || u0.Output != 300 || u0.CacheRead != 7000 || u0.CacheWrite != 300 || u0.Reasoning != 50 {
		t.Errorf("turn 0 tokens = %+v, want summed across both chunks", u0)
	}
	// Context occupancy excludes output: 1500 + 7000 + 300.
	if u0.ContextTokens != 8800 {
		t.Errorf("turn 0 context = %d, want 8800 (input + cache read + cache write, output excluded)", u0.ContextTokens)
	}
	if u0.CostUSD == nil || *u0.CostUSD < 0.149 || *u0.CostUSD > 0.151 {
		t.Errorf("turn 0 cost = %v, want ~0.15", u0.CostUSD)
	}

	// Turn 0 is fully priced, so its cost is exact, not a lower bound.
	if u0.CostIncomplete {
		t.Error("turn 0 is fully priced and must not read cost-incomplete")
	}

	m1 := by[1]
	if m1.Usage == nil {
		t.Fatalf("turn 1 carries no folded usage")
	}
	u1 := *m1.Usage
	if u1.ContextTokens != 100900 { // 900 input + 100000 cache read + 0 cache write
		t.Errorf("turn 1 context = %d, want 100900", u1.ContextTokens)
	}
	// A mixed group returns the priced partial (0.30), not NULL and not a summed-with-zero figure.
	if u1.CostUSD == nil || *u1.CostUSD < 0.299 || *u1.CostUSD > 0.301 {
		t.Errorf("turn 1 cost = %v, want ~0.30 (the priced partial)", u1.CostUSD)
	}
	// The mixed group folded a token-bearing unpriced row, so its cost is a lower bound: the flag
	// must fire so the stamp reads "$0.30+" rather than an exact figure beside unpriced tokens.
	if !u1.CostIncomplete {
		t.Error("turn 1 mixes priced and token-bearing unpriced rows and must read cost-incomplete")
	}
}

// TestMessagesTurnUsageUnpricedTurn pins that a turn whose every row is unpriced folds to a nil
// cost (unmeasured) on its Usage, never a summed zero that would read as free.
func TestMessagesTurnUsageUnpricedTurn(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st)

	ord := 0
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", Content: "a", Model: "mystery"}},
		Usage: []store.ProjUsage{
			{MessageOrdinal: &ord, Model: "mystery", Input: 100, Output: 50, CostUSD: nil, SourceOffset: 1, SourceIndex: 0},
		},
	}); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	msgs, err := st.Messages(ctx, sid)
	if err != nil {
		t.Fatalf("read messages: %v", err)
	}
	by := msgByOrdinal(msgs)
	if by[0].Usage == nil {
		t.Fatalf("turn 0 carries no folded usage")
	}
	if by[0].Usage.CostUSD != nil {
		t.Errorf("an all-unpriced turn should have nil cost, got %v", *by[0].Usage.CostUSD)
	}
}

// TestMessagesTurnUsageNullOrdinalInvisible pins that a usage row with no message ordinal never
// surfaces on any message's Usage: a message whose only usage is a NULL-ordinal row reads Usage nil
// (nothing to attribute to that turn).
func TestMessagesTurnUsageNullOrdinalInvisible(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st)

	cost := func(f float64) *float64 { return &f }
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", Content: "a", Model: "gpt-5"}},
		Usage: []store.ProjUsage{
			{MessageOrdinal: nil, Model: "gpt-5", Input: 999, Output: 999, CostUSD: cost(9.99), SourceOffset: 1, SourceIndex: 0},
		},
	}); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	msgs, err := st.Messages(ctx, sid)
	if err != nil {
		t.Fatalf("read messages: %v", err)
	}
	by := msgByOrdinal(msgs)
	if by[0].Usage != nil {
		t.Errorf("ordinal 0 should carry no usage (its only usage row has a NULL ordinal), got %+v", *by[0].Usage)
	}
}

// TestMessagesTurnUsageDivergesFromContextFold pins the intentional difference between the
// transcript's per-turn fold (Message.Usage, grouped by ordinal, attributed rows only) and the
// stored context signal's raw fold (gatherContextHealth, exercised here through its tested
// reference quality.ContextHealth). The two agree whenever each ordinal carries exactly one dated
// usage row (the shape a real agent produces), and diverge only for a multi-row turn or an
// unattributed row, which the schema permits but the parser does not emit. See the usage_agg CTE
// doc in read.go.
//
// The divergent corpus is two usage rows on one ordinal (a big context then a sharp drop) plus a
// NULL-ordinal row. The transcript fold groups by ordinal and drops the NULL row, so the one
// message reports the summed context and shows no shed divider (a turn cannot shed against itself).
// The folder walks every raw row in order, so it sees the drop between the two same-turn rows as a
// context reset. Their reset views therefore disagree, on purpose.
func TestMessagesTurnUsageDivergesFromContextFold(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st)

	ord0 := 0
	// Row order in the raw sequence is (source_offset, source_index, id): a big context, then a
	// sharp drop past the reset threshold (100000 -> 1000 fires IsContextReset), then a NULL-ordinal
	// tail row. Context occupancy is input + cache read + cache write, so cache read carries the size.
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", Content: "a", Model: "gpt-5"}},
		Usage: []store.ProjUsage{
			{MessageOrdinal: &ord0, Model: "gpt-5", CacheRead: 100000, CostUSD: nil, SourceOffset: 1, SourceIndex: 0},
			{MessageOrdinal: &ord0, Model: "gpt-5", CacheRead: 1000, CostUSD: nil, SourceOffset: 2, SourceIndex: 0},
			{MessageOrdinal: nil, Model: "gpt-5", CacheRead: 1000, CostUSD: nil, SourceOffset: 3, SourceIndex: 0},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}

	// What the transcript folds: the two same-ordinal rows collapse into one turn's Usage, the
	// NULL-ordinal row is dropped, so there is one message with usage and no second turn to shed
	// against.
	msgs, err := st.Messages(ctx, sid)
	if err != nil {
		t.Fatalf("read messages: %v", err)
	}
	withUsage := 0
	var only store.TurnUsage
	for _, m := range msgs {
		if m.Usage != nil {
			withUsage++
			only = *m.Usage
		}
	}
	if withUsage != 1 {
		t.Fatalf("transcript sees %d turns with usage, want 1 (two rows collapse by ordinal, the NULL row is dropped)", withUsage)
	}
	if only.ContextTokens != 101000 {
		t.Errorf("collapsed turn context = %d, want 101000 (100000 + 1000 summed by ordinal)", only.ContextTokens)
	}

	// What the signal counts: the folder walks every raw row in transcript order (both same-ordinal
	// rows and the NULL-ordinal row), so it sees the drop as a reset. quality.ContextHealth is the
	// buffered reference gatherContextHealth's streaming folder mirrors, fed the same ordered sizes.
	peak, resets := quality.ContextHealth([]int64{100000, 1000, 1000})
	if peak != 100000 {
		t.Errorf("folder peak = %d, want 100000", peak)
	}
	if resets != 1 {
		t.Errorf("folder resets = %d, want 1 (the 100000 -> 1000 drop within one ordinal)", resets)
	}

	// The pin: the transcript can render no shed divider (it has one turn with usage), while the
	// signal counts one reset over the same rows. This divergence is intentional (see the usage_agg
	// CTE doc): the transcript attaches usage to rendered messages and groups by ordinal, the signal
	// measures the raw turn sequence and folds every row.
	if withUsage > 1 {
		t.Fatal("guard: the transcript must have a single turn with usage for this pin to hold")
	}
	if resets == 0 {
		t.Fatal("guard: the folder must count a reset for this pin to hold")
	}
}

// TestMessagesDuplicatePromptMatchesStoredCount pins that the transcript's per-message
// DuplicatePrompt flag (folded in SQL in the Messages read) marks the same set the stored
// duplicate_prompt_count aggregates (gatherPromptHygiene), so a transcript "repeat" badge and the
// session's stored count never disagree. It seeds three verbatim copies of a substantial prompt
// (the first is the original, the next two are repeats), an unrelated substantial prompt, and a
// pair of terse acknowledgements that share a digest but are duplicate-ineligible (short prompts
// legitimately recur). The flag count from Messages must equal the stored duplicate_prompt_count,
// and only the two non-first substantial repeats carry the flag.
func TestMessagesDuplicatePromptMatchesStoredCount(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-dup")

	repeated := "please run the guarded rerun and refresh the golden snapshots thoroughly"
	other := "now add a reconciliation test that pins the invariant against the analytics panel"
	terse := "yes go"
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: repeated}, // original
			{Ordinal: 1, Role: "assistant", Content: "ok"},
			{Ordinal: 2, Role: "user", Content: repeated}, // repeat 1
			{Ordinal: 3, Role: "assistant", Content: "ok"},
			{Ordinal: 4, Role: "user", Content: other}, // distinct: not a repeat
			{Ordinal: 5, Role: "assistant", Content: "ok"},
			{Ordinal: 6, Role: "user", Content: repeated}, // repeat 2
			{Ordinal: 7, Role: "assistant", Content: "ok"},
			{Ordinal: 8, Role: "user", Content: terse}, // short, ineligible
			{Ordinal: 9, Role: "user", Content: terse}, // short repeat, still ineligible
		},
	}); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	// The settle pass needs the user-turn rollup and a settled session to grade and store the count.
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET user_message_count = 5 WHERE id = $1`, sid); err != nil {
		t.Fatalf("set user message count: %v", err)
	}
	settleSession(t, st, ctx, sid)
	if _, err := st.RefreshSettledSignals(ctx); err != nil {
		t.Fatalf("refresh settled signals: %v", err)
	}

	// The transcript's per-message flags: exactly the two non-first substantial repeats.
	msgs, err := st.Messages(ctx, sid)
	if err != nil {
		t.Fatalf("read messages: %v", err)
	}
	flagged := map[int]bool{}
	for _, m := range msgs {
		if m.DuplicatePrompt {
			flagged[m.Ordinal] = true
		}
	}
	if !flagged[2] || !flagged[6] {
		t.Errorf("expected ordinals 2 and 6 (the non-first substantial repeats) flagged, got %v", flagged)
	}
	if flagged[0] {
		t.Error("the first occurrence of a digest is the original and must not be flagged")
	}
	if flagged[8] || flagged[9] {
		t.Error("terse (short) prompts are duplicate-ineligible and must never be flagged")
	}
	if len(flagged) != 2 {
		t.Errorf("flagged %d messages, want 2", len(flagged))
	}

	// The stored aggregate must count the same set: two duplicates.
	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read session signals: %v", err)
	}
	if sig.DuplicatePromptCount != len(flagged) {
		t.Errorf("stored duplicate_prompt_count = %d, transcript flagged %d: the badge and the stored count must read the same set",
			sig.DuplicatePromptCount, len(flagged))
	}
}

// TestMessageTurnUsageMatchesUsageEvents pins the projection-consistency invariant for the
// materialized per-turn rollup: message_turn_usage must equal, row for row, a fresh GROUP BY over
// the surviving usage_events it summarizes. The rollup is accumulated at insert (projection.go) and
// read by the transcript in place of that GROUP BY, so if the two ever drifted the transcript would
// show a turn load the ledger does not back. It seeds a mixed corpus (a multi-chunk turn, a
// priced+unpriced mixed turn, an all-unpriced turn, and a NULL-ordinal row that belongs to no turn)
// and asserts the stored rollup matches the aggregate the old read computed, including the
// all-unpriced-is-nil cost rule and the cost_incomplete flag, and that the NULL-ordinal row folded
// into no rollup row.
func TestMessageTurnUsageMatchesUsageEvents(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st)

	ord0, ord1, ord2 := 0, 1, 2
	cost := func(f float64) *float64 { return &f }
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "assistant", Content: "a", Model: "gpt-5"},
			{Ordinal: 1, Role: "assistant", Content: "b", Model: "gpt-5"},
			{Ordinal: 2, Role: "assistant", Content: "c", Model: "mystery"},
		},
		Usage: []store.ProjUsage{
			// Turn 0: two streamed chunks, both priced (the rollup must sum them).
			{MessageOrdinal: &ord0, Model: "gpt-5", Input: 1000, Output: 200, CacheRead: 5000, CacheWrite: 300, Reasoning: 40, CostUSD: cost(0.10), SourceOffset: 1, SourceIndex: 0},
			{MessageOrdinal: &ord0, Model: "gpt-5", Input: 500, Output: 100, CacheRead: 2000, CacheWrite: 0, Reasoning: 10, CostUSD: cost(0.05), SourceOffset: 2, SourceIndex: 0},
			// Turn 1: one priced, one token-bearing unpriced (a mixed group: priced partial + flag).
			{MessageOrdinal: &ord1, Model: "gpt-5", Input: 800, Output: 400, CacheRead: 100000, CostUSD: cost(0.30), SourceOffset: 3, SourceIndex: 0},
			{MessageOrdinal: &ord1, Model: "mystery", Input: 100, Output: 50, CostUSD: nil, SourceOffset: 4, SourceIndex: 0},
			// Turn 2: all unpriced (cost reads nil, not a summed zero).
			{MessageOrdinal: &ord2, Model: "mystery", Input: 70, Output: 30, CostUSD: nil, SourceOffset: 5, SourceIndex: 0},
			// A NULL-ordinal row: attributable to no turn, so it folds into no rollup row.
			{MessageOrdinal: nil, Model: "gpt-5", Input: 999, Output: 999, CostUSD: cost(9.99), SourceOffset: 6, SourceIndex: 0},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}

	// A row is (tokens..., costSum, costCount, costIncomplete): the exact shape both the stored rollup
	// and a fresh aggregate over usage_events produce, so the two maps compare directly.
	type roll struct {
		in, out, cw, cr, rz int64
		costSum             float64
		costCount           int64
		costIncomplete      bool
	}
	scanRolls := func(q string) map[int]roll {
		rows, err := st.Pool.Query(ctx, q, sid)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		out := map[int]roll{}
		for rows.Next() {
			var ord int
			var r roll
			if err := rows.Scan(&ord, &r.in, &r.out, &r.cw, &r.cr, &r.rz, &r.costSum, &r.costCount, &r.costIncomplete); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out[ord] = r
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %q: %v", q, err)
		}
		return out
	}

	// The stored rollup.
	stored := scanRolls(`SELECT message_ordinal, input_tokens, output_tokens, cache_write_tokens,
	                             cache_read_tokens, reasoning_tokens, cost_sum, cost_count, cost_incomplete
	                        FROM message_turn_usage WHERE session_id = $1`)
	// A fresh aggregate over the surviving usage_events, the fold the transcript read before the
	// rollup replaced it. coalesce(sum(cost_usd),0) matches the rollup's NOT NULL cost_sum; the
	// cost_incomplete predicate mirrors the one projection.go folds per row.
	fresh := scanRolls(`SELECT message_ordinal,
	                            coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
	                            coalesce(sum(cache_write_tokens),0), coalesce(sum(cache_read_tokens),0),
	                            coalesce(sum(reasoning_tokens),0), coalesce(sum(cost_usd),0), count(cost_usd),
	                            bool_or(cost_usd IS NULL AND (input_tokens + output_tokens + cache_read_tokens + cache_write_tokens + reasoning_tokens) > 0)
	                       FROM usage_events
	                      WHERE session_id = $1 AND message_ordinal IS NOT NULL
	                      GROUP BY message_ordinal`)

	if len(stored) != 3 {
		t.Fatalf("stored rollup has %d rows, want 3 (turns 0, 1, 2; the NULL-ordinal row folds into none)", len(stored))
	}
	for ord, want := range fresh {
		got, ok := stored[ord]
		if !ok {
			t.Fatalf("ordinal %d present in usage_events aggregate but missing from the rollup", ord)
		}
		if got != want {
			t.Errorf("ordinal %d rollup %+v != usage_events aggregate %+v", ord, got, want)
		}
	}
	for ord := range stored {
		if _, ok := fresh[ord]; !ok {
			t.Errorf("ordinal %d in the rollup has no backing usage_events aggregate", ord)
		}
	}

	// The flag pins directly too: turn 0 fully priced (exact), turn 1 mixed (a lower bound), turn 2
	// all unpriced (also a token-bearing unpriced row present, so flagged).
	if stored[0].costIncomplete {
		t.Error("turn 0 is fully priced and must not read cost-incomplete")
	}
	if !stored[1].costIncomplete {
		t.Error("turn 1 mixes priced and token-bearing unpriced rows and must read cost-incomplete")
	}
}

// TestMessagesPromptFacts pins that the Messages read surfaces the per-prompt hygiene facts and
// gates them behind the current classifier version: a user prompt classified at
// quality.PromptFactsVersion reads PromptFactsCurrent true with its facts, an assistant message
// carries no facts, and a superseded-version row reads as not current (its facts render as
// nothing).
func TestMessagesPromptFacts(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st)

	ts := time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC)
	// A terse user prompt (classified live at the current version), then an assistant reply.
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "fix it", Timestamp: ts},
			{Ordinal: 1, Role: "assistant", Content: "On it.", Model: "gpt-5", Timestamp: ts},
		},
	}); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	// Stamp a third message directly at a superseded facts version to prove the version gate: the
	// row has a digest but an old version, so it must read as not current.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content, prompt_short, prompt_no_code, prompt_digest, prompt_facts_version)
		 VALUES ($1, 2, 'user', 'stale prompt', true, false, 424242, $2)`,
		sid, quality.PromptFactsVersion-1); err != nil {
		t.Fatalf("insert stale-version message: %v", err)
	}

	msgs, err := st.Messages(ctx, sid)
	if err != nil {
		t.Fatalf("read messages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("read %d messages, want 3", len(msgs))
	}
	// The live-classified terse prompt reads its facts as current.
	if !msgs[0].PromptFactsCurrent {
		t.Error("the live-classified user prompt should read PromptFactsCurrent = true")
	}
	if !msgs[0].PromptShort {
		t.Error("a two-word prompt should classify as short")
	}
	if msgs[0].PromptDigest == 0 {
		t.Error("a classified prompt should carry a non-zero digest")
	}
	// The assistant message carries no facts, so it reads as not current.
	if msgs[1].PromptFactsCurrent {
		t.Error("an assistant message should not read PromptFactsCurrent = true")
	}
	// The superseded-version row reads as not current despite carrying a digest.
	if msgs[2].PromptFactsCurrent {
		t.Error("a message at a superseded facts version should read PromptFactsCurrent = false")
	}
}

// TestMessagesAfterPromptFactsVersionGate pins that the bounded keyset reader applies the same
// prompt-facts version gate the whole-transcript read does (TestMessagesPromptFacts): a message
// row stamped at a superseded quality.PromptFactsVersion must read PromptFactsCurrent = false
// through MessagesAfter, not just Messages, since both share messageReadColumns but the gate is
// re-evaluated per query against the running version. A row at the current version, by contrast,
// reads current.
func TestMessagesAfterPromptFactsVersionGate(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st)

	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "user", Content: "fix it"}},
	}); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	// Stamp a second message directly at a superseded facts version, mirroring
	// TestMessagesPromptFacts' stale row but read here through the bounded window instead.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content, prompt_short, prompt_no_code, prompt_digest, prompt_facts_version)
		 VALUES ($1, 1, 'user', 'stale prompt', true, false, 424242, $2)`,
		sid, quality.PromptFactsVersion-1); err != nil {
		t.Fatalf("insert stale-version message: %v", err)
	}

	msgs, err := st.MessagesAfter(ctx, sid, nil, 10)
	if err != nil {
		t.Fatalf("MessagesAfter: %v", err)
	}
	byOrd := msgByOrdinal(msgs)
	if !byOrd[0].PromptFactsCurrent {
		t.Error("a message classified at the current facts version should read PromptFactsCurrent = true through MessagesAfter")
	}
	if byOrd[1].PromptFactsCurrent {
		t.Error("a message at a superseded facts version should read PromptFactsCurrent = false through MessagesAfter, not the stale facts")
	}
}

// TestToolCallsFileRelPath pins that the ToolCalls read surfaces the worktree-relative path the
// projection derived from the session's cwd, alongside the absolute file_path.
func TestToolCallsFileRelPath(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st) // cwd is /home/grace/akari

	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Edit", Category: "edit",
			FilePath:  "/home/grace/akari/internal/auth.go",
			InputBody: `{"file_path":"internal/auth.go"}`, InputMediaType: "application/json", CallUID: "c1",
		}},
	}); err != nil {
		t.Fatalf("apply delta: %v", err)
	}

	calls, err := st.ToolCalls(ctx, sid)
	if err != nil {
		t.Fatalf("read tool calls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("read %d tool calls, want 1", len(calls))
	}
	if got, want := calls[0].FileRelPath, "internal/auth.go"; got != want {
		t.Errorf("file_rel_path = %q, want %q", got, want)
	}
	if got, want := calls[0].FilePath, "/home/grace/akari/internal/auth.go"; got != want {
		t.Errorf("file_path = %q, want %q (absolute path preserved)", got, want)
	}
}

// TestToolCallsInRangeFileRelPath pins that the bounded ToolCallsInRange read carries the same
// worktree-relative path ToolCalls does (TestToolCallsFileRelPath): a tool call inserted under a
// session with a known cwd yields its path relative to that cwd, and one inserted under a session
// with no cwd yields an empty FileRelPath (the projection had no anchor to derive a relative form
// against, so file_rel_path stored NULL and the read coalesces it to "").
func TestToolCallsInRangeFileRelPath(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	// A session with a known cwd: the edit under it derives a relative path.
	withCwd := seedForReads(t, st) // cwd is /home/grace/akari
	if err := st.ApplyProjectionDelta(ctx, withCwd, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Edit", Category: "edit",
			FilePath:  "/home/grace/akari/internal/auth.go",
			InputBody: `{"file_path":"internal/auth.go"}`, InputMediaType: "application/json", CallUID: "c1",
		}},
	}); err != nil {
		t.Fatalf("apply delta (with cwd): %v", err)
	}
	calls, err := st.ToolCallsInRange(ctx, withCwd, 0, 0)
	if err != nil {
		t.Fatalf("tool calls in range (with cwd): %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("read %d tool calls, want 1", len(calls))
	}
	if got, want := calls[0].FileRelPath, "internal/auth.go"; got != want {
		t.Errorf("with cwd: file_rel_path = %q, want %q", got, want)
	}

	// A session announced with no cwd: the same edit has no anchor to derive a relative
	// path against, so file_rel_path reads empty rather than a guessed value.
	uid := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid, Agent: "claude", SourceSessionID: "sess-no-cwd", ProjectID: pid, Machine: "box",
	})
	if err != nil {
		t.Fatalf("announce without cwd: %v", err)
	}
	noCwd := ann.SessionID
	if err := st.ApplyProjectionDelta(ctx, noCwd, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Edit", Category: "edit",
			FilePath: "/home/ada/somewhere/db.go", CallUID: "c2",
		}},
	}); err != nil {
		t.Fatalf("apply delta (no cwd): %v", err)
	}
	noCwdCalls, err := st.ToolCallsInRange(ctx, noCwd, 0, 0)
	if err != nil {
		t.Fatalf("tool calls in range (no cwd): %v", err)
	}
	if len(noCwdCalls) != 1 {
		t.Fatalf("read %d tool calls, want 1", len(noCwdCalls))
	}
	if got := noCwdCalls[0].FileRelPath; got != "" {
		t.Errorf("without cwd: file_rel_path = %q, want empty", got)
	}
}

// TestAnnounceRecomputesRelPathsOnCwdChange pins that a re-announce with a different cwd
// recomputes the stored tool_calls.file_rel_path against the new anchor. file_rel_path is a
// projection of cwd + file_path derived at insert, and announce updates cwd on conflict, so a
// re-announce from a different checkout must follow its input or old rel paths strand at the stale
// anchor and split one repo file's churn across two keys. It announces cwd A, ingests two edits
// (one under A, one outside it), re-announces the same session at cwd B (under which the first path
// no longer sits and the second now does), and asserts each rel path tracked the new cwd.
func TestAnnounceRecomputesRelPathsOnCwdChange(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	// Announce at cwd A, then ingest two edits: one under A (rel derives) and one under a sibling
	// path B that is NOT under A (stays absolute-only, rel NULL) but WILL sit under B after re-announce.
	annA, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-cwd",
		ProjectID: pid, GitBranch: "main", Cwd: "/home/grace/worktreeA", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce cwd A: %v", err)
	}
	sid := annA.SessionID
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 0, CallIndex: 0, ToolName: "Edit", Category: "edit",
				FilePath: "/home/grace/worktreeA/internal/auth.go", CallUID: "c1"},
			{MessageOrdinal: 0, CallIndex: 1, ToolName: "Edit", Category: "edit",
				FilePath: "/home/grace/worktreeB/internal/db.go", CallUID: "c2"},
		},
	}); err != nil {
		t.Fatalf("apply delta: %v", err)
	}

	// Sanity before the cwd change: the A-anchored path is relative, the B-anchored one is not.
	relByFile := func() map[string]string {
		calls, err := st.ToolCalls(ctx, sid)
		if err != nil {
			t.Fatalf("read tool calls: %v", err)
		}
		out := map[string]string{}
		for _, c := range calls {
			out[c.FilePath] = c.FileRelPath
		}
		return out
	}
	before := relByFile()
	if before["/home/grace/worktreeA/internal/auth.go"] != "internal/auth.go" {
		t.Errorf("before recompute: auth.go rel = %q, want internal/auth.go", before["/home/grace/worktreeA/internal/auth.go"])
	}
	if before["/home/grace/worktreeB/internal/db.go"] != "" {
		t.Errorf("before recompute: db.go rel = %q, want empty (outside cwd A)", before["/home/grace/worktreeB/internal/db.go"])
	}

	// Re-announce the same session at cwd B: now the db.go path is under the workspace and the
	// auth.go path is not. The stored rel paths must flip to match the new anchor.
	if _, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-cwd",
		ProjectID: pid, GitBranch: "main", Cwd: "/home/grace/worktreeB", Machine: "laptop",
	}); err != nil {
		t.Fatalf("re-announce cwd B: %v", err)
	}
	after := relByFile()
	if after["/home/grace/worktreeA/internal/auth.go"] != "" {
		t.Errorf("after recompute: auth.go rel = %q, want empty (now outside cwd B)", after["/home/grace/worktreeA/internal/auth.go"])
	}
	if after["/home/grace/worktreeB/internal/db.go"] != "internal/db.go" {
		t.Errorf("after recompute: db.go rel = %q, want internal/db.go (now under cwd B)", after["/home/grace/worktreeB/internal/db.go"])
	}
}
