package parse

// Epoch is the fleet-wide reparse signal: a binary constant the running server
// compares against parse_meta.reparsed_epoch to decide whether already-ingested
// data needs rebuilding. When Epoch != reparsed_epoch the server reparses every
// session in the background and, on success, writes Epoch back, so a deploy is all
// it takes to roll a parser improvement out to old data. Bump it in the same commit
// as any change that alters parser or reducer output: new or removed rows, changed
// field values, a different fold, or a pricing-table change that re-prices stored
// usage (the projection carries cost, so a reprice is an output change too).
//
// Why a constant and not a migration: parser behavior lives in the binary, not the
// schema. The most recent parser change (PR #18, "Lift Codex image payloads to the
// CAS; stop refusing bodyless big lines") shipped with no database migration at
// all, so a migration-versioned trigger would have missed it and the maintainer
// had to reparse by hand over SSH. The epoch travels with the binary, so the
// signal fires exactly when the code that produces the projection changes,
// migration or not.
//
// Relationship to Version: Version (see parse.go) is the per-session
// incremental-resume marker stored on each session_raw row; it stops two parser
// versions' output from blending on the live append path. Epoch is the global
// "has the whole corpus been reparsed since the last output change" marker. They
// move together in practice, so bump both when output changes. The golden-fixtures
// test (epoch_test.go) is the guardrail that makes the Epoch bump impossible to
// forget: it snapshots the projection for representative fixtures and fails, by
// name, when that output drifts.
//
// An Epoch bump is also a way to backfill a derived projection table that did not
// exist before. A caught-up session never re-enters AdvanceProjection, and the append
// path does not fill session_signals (the settle pass does, once a session settles),
// so on the first deploy of a new derived table an Epoch bump reparses the corpus to
// populate it in one pass, independent of whether the settle loop is enabled. The
// reparse also rebuilds session_signals at the running quality.Version, so a
// scoring-model change rides the same signal. Such a bump leaves the parser's projection
// delta byte-for-byte identical, so the golden fixtures do not move; the bump is
// intentional and stands on its own.
//
// Epoch 1 -> 2: introduce session_signals, the per-session derived behavioral signals
// (an outcome classification, a quality score and grade, tool-health counts,
// prompt-hygiene counts, and context-health figures). They are materialized by the settle
// pass as sessions settle and rebuilt on reparse; this one-time Epoch bump backfills the
// table across the existing (already-settled) corpus on first deploy and stamps every row
// at the running quality.Version. session_signals is a new derived table rather than a
// change to the parser's output, so this bump leaves the projection delta byte-for-byte
// identical and the golden fixtures do not move; it stands on its own.
//
// Epoch 2 -> 3: materialize per-message prompt-hygiene facts on the messages row
// (prompt_short, prompt_no_code, prompt_bare_greeting, prompt_digest; see migration
// 0022_prompt_hygiene_facts and the message insert in store/projection.go). They are derived at
// insert so the settle pass aggregates fixed-size columns instead of reading prompt bodies back.
// Migration 0022 adds the columns but, unlike a generated column, cannot backfill them; this bump
// reparses the corpus so every existing message re-inserts through the classifier and the columns
// fill in one pass. The facts are store-side derived columns, not parser output, so the reducer's
// projection delta is byte-for-byte identical and the golden fixtures do not move; the bump is the
// backfill signal and stands on its own.
//
// Epoch 3 -> 4: materialize tool_calls.file_rel_path, the session-relative form of a tool call's
// file_path (see migration 0028_tool_call_rel_path and the tool-call insert in store/projection.go).
// It exists so file churn can aggregate one repo file across the git worktrees it was edited from:
// file_path is absolute and fragments the same file into per-worktree rows, while the relative path
// paired with the project is worktree-invariant. Like the prompt-hygiene facts, migration 0028 adds
// the column but cannot backfill it; this bump reparses the corpus so every existing tool call
// re-inserts through sessionRelPath and the column fills in one pass. The column is a store-side
// derived value, not parser output (the reducer's delta is unchanged), so the projection delta is
// byte-for-byte identical and the golden fixtures do not move; the bump is the backfill signal and
// stands on its own.
const Epoch = 4
