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
// Epoch 3 -> 4: add a per-tool-call detail (a shell command, a search pattern, a fetched URL, or an
// agent's description) so the UI can show what a call did when it has no file_path. The reparse
// re-derives it from the raw transcript wherever the input body is inline, so an inline-bodied session
// backfills its details in one pass. A session whose inputs were CAS-stripped before this change carries
// no detail on its sentinels, so it keeps an empty detail (the UI degrades for that); re-uploading is the
// only way to fill it. This is a parser output change (the projection delta now carries the field), so it
// pairs with the parse.Version bump and the golden fixtures move.
//
// Epoch 4 -> 5: materialize tool_calls.file_rel_path, the session-relative form of a tool call's
// file_path (see migration 0030_tool_call_rel_path and the tool-call insert in store/projection.go).
// It exists so file churn can aggregate one repo file across the git worktrees it was edited from:
// file_path is absolute and fragments the same file into per-worktree rows, while the relative path
// paired with the project is worktree-invariant. Like the prompt-hygiene facts, migration 0030 adds
// the column but cannot backfill it; this bump reparses the corpus so every existing tool call
// re-inserts through sessionRelPath and the column fills in one pass. The column is a store-side
// derived value, not parser output (the reducer's delta is unchanged), so the projection delta is
// byte-for-byte identical and the golden fixtures do not move; the bump is the backfill signal and
// stands on its own.
//
// Epoch 5 -> 6: materialize messages.duplicate_prompt, the per-user-turn verdict that this prompt
// repeats an earlier eligible prompt's digest (see migration 0031_message_duplicate_prompt and the
// message insert in store/projection.go). It exists so the web transcript reads a stored boolean
// instead of folding a whole-session window on every SSE-driven body refresh. Because the verdict
// depends on the ordered prefix of earlier messages, it can only be filled as messages re-insert in
// order; migration 0031 adds the column but cannot backfill it, so this bump reparses the corpus and
// every existing user turn re-derives its flag in one ordered pass. The flag is a store-side derived
// column, not parser output (the reducer's delta is unchanged), so the projection delta is
// byte-for-byte identical and the golden fixtures do not move; the bump is the backfill signal and
// stands on its own.
//
// Epoch 6 -> 7: materialize the message_turn_usage rollup, one row per (session, message_ordinal)
// holding that turn's summed token classes and cost (see migration 0032_message_turn_usage and the
// usage insert loop in store/projection.go). It exists so the web transcript joins one indexed row
// per message instead of re-grouping the session's whole usage_events table on every SSE-driven body
// refresh. The rollup is accumulated as each surviving usage row inserts; migration 0032 creates the
// table but cannot backfill it, so this bump reparses the corpus and every existing session's usage
// re-folds into its per-turn rows in one pass. The rollup is a store-side fold OF usage_events, not
// parser output (the reducer's delta is unchanged), so the projection delta is byte-for-byte identical
// and the golden fixtures do not move; the bump is the backfill signal and stands on its own.
//
// Epoch 7 -> 8: reclassify a Codex session's injected framing (the AGENTS.md project
// instructions and the environment_context block prepended before the first prompt, and
// re-injected after a compaction) from the user role to the "context" role (see
// internal/parser/codex.go isCodexContext and parser.RoleContext). Before this change the
// framing was the session's first user message, so it became the session title everywhere,
// inflated user_message_count, and was judged as the opening human prompt by prompt hygiene.
// Re-roling it drops it from every role='user' reader for free (the title lateral, the count,
// the hygiene aggregate) and moves it into the transcript's own Context section. This is a
// parser output change (a message row's role differs), so it pairs with the parse.Version bump
// and the golden fixtures move. The reparse also re-derives session_signals at the running
// quality.Version, so the shifted prompt-hygiene grades (the opener is now the real prompt) roll
// out in the same pass.
//
// Epoch 8 -> 9: add the model_fallbacks projection row and the sessions.model_fallback_count rollup
// (see migration 0034_model_fallbacks and the fallback upsert in store/projection.go). A model fallback
// is a Claude Fable turn the safety classifier declined and re-served on a lower model, detected only
// from the transcript's explicit markers (a "fallback" content block, a usage.iterations
// "fallback_message" entry, or a "model_refusal_fallback" system entry), never from a bare model-string
// change. Like Epoch 8 this is a genuine parser output change: the reducer emits a new op type, so the
// projection delta carries new rows and the golden fixtures move. It pairs with the parse.Version bump,
// and the reparse it forces detects fallbacks across the already-ingested corpus in one pass, folding
// model_fallback_count from the surviving inserts on each session.
const Epoch = 9
