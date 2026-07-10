package parse

// Epoch is the single version constant for everything derived from a session's
// raw bytes. Each session_raw row stores the epoch its last successful rebuild
// ran at; a session is due for a rebuild whenever that stamp differs from this
// constant (or its bytes moved past the last rebuilt length), so a deploy with
// a bumped Epoch makes the whole corpus due and the parse worker rebuilds it in
// the background. Bump it in the same commit as any change to what a rebuild
// produces: parser or reducer output, a rebuild-derived column, the signal set
// or scoring, prompt classification, or the pricing table (the projection
// carries cost, so a reprice is an output change too). Nothing else needs
// versioning, because nothing derived can stay behind for longer than one
// rebuild.
//
// Why a constant and not a migration: parser behavior lives in the binary, not
// the schema. Parser changes often ship with no database migration at all (PR
// #18, "Lift Codex image payloads to the CAS", was one), so a
// migration-versioned trigger would miss them. The epoch travels with the
// binary, so the signal fires exactly when the code that produces the
// projection changes, migration or not.
//
// The golden-fixtures test (epoch_test.go) is the guardrail that makes the
// Epoch bump impossible to forget: it snapshots the projection for
// representative fixtures and fails, by name, when that output drifts. A bump
// whose change lives outside the reducer (a store-side derived column, a
// scoring change, a reprice) leaves the fixtures unmoved; the bump is
// intentional and stands on its own.
//
// Entries below Epoch 13 predate the rebuild pipeline and describe the old
// incremental machinery (a parse.Version resume marker, a separate reparse
// service, per-table version stamps); they are kept as the history of what each
// epoch's data change was.
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
//
// Epoch 9 -> 10: price claude-sonnet-5 (Sonnet 5 at the standard $3/$15 Sonnet rate; see
// internal/pricing). Sonnet 5 usage was unknown to the pricing table before, so its usage_events rows
// carry a NULL per-row cost and its sessions read cost_incomplete. This bump reparses the corpus so
// every Sonnet 5 usage row re-prices through pricing.Cost in one pass, and pairs with the
// pricing.Version 1 -> 2 bump that re-prices the per-session cache-savings rollup. It is a pricing
// change, not a reducer-shape change, and no golden fixture uses Sonnet 5, so the projection delta for
// the fixtures is byte-for-byte identical and the golden snapshots do not move; the bump is the reprice
// signal and stands on its own.
//
// Epoch 10 -> 11: give claude-sonnet-5 its two-window introductory rate ($2/$10 per MTok through
// 2026-08-31, $3/$15 after; see internal/pricing). pricing.Cost now selects the date-effective window
// at each usage row's OccurredAt, so a Sonnet 5 row logged inside the intro window prices cheaper than
// the flat $3/$15 the previous epoch stored. This bump reparses the corpus so every Sonnet 5 usage row
// re-prices from the window in effect when it occurred, and pairs with the pricing.Version 2 -> 3 bump
// that re-prices the per-session cache-savings rollup at the windowed rates. Like Epoch 10 it is a
// pricing change, not a reducer-shape change, and no golden fixture uses Sonnet 5, so the projection
// delta for the fixtures is byte-for-byte identical and the golden snapshots do not move; the bump is
// the reprice signal and stands on its own.
//
// Epoch 11 -> 12: record the reasoning-trace weight per assistant turn (messages.thinking_bytes; see
// parser.Message.ThinkingBytes and migration 0041). This is the data source for the observed-thinking
// signal: how much a session's model actually deliberated, ranked per model at read time. It is measured
// from the reasoning the agents already log, but current Claude Code and Codex ship that reasoning
// encrypted (Claude a "signature", Codex an "encrypted_content" blob) and drop the plaintext, so the
// weight is the plaintext length where it survives and the encrypted payload length where it does not
// (which tracks the hidden reasoning volume at r=0.97 for Claude, r=0.997 for Codex). The reducer now
// emits ThinkingBytes on every assistant turn and sets HasThinking on the presence of a reasoning block
// rather than on non-empty text, so a redacted turn reads as thinking where before it read as none. That
// flips has_thinking true on the bulk of the real corpus and adds a message field, a genuine parser
// output change, so it moves the golden fixtures. Because thinking_bytes is a plain column the parser
// fills (not a generated one), the reparse this forces is what populates it across the already-ingested
// corpus; the reparse also re-derives session_signals at the running quality.Version, so the new
// observed-thinking scalars materialize in the same pass.
//
// Epoch 12 -> 13: the rebuild pipeline. Parsing becomes rebuild-on-dirty (see docs/DESIGN.md
// "Server-side parsing pipeline"): a background worker refolds a session's whole projection from
// byte zero whenever its bytes or this epoch move, and the incremental machinery goes away
// (parse.Version, serialized reducer state, the reparse service, and the quality.Version /
// quality.PromptFactsVersion / pricing.Version stamps all collapse into this one constant). Two
// output changes ride the same bump. First, the Claude reducer now folds the content-block lines
// that share one API message.id into a single assistant turn, so a messages row is one semantic
// turn for every agent, has_tool_use and thinking land on the turn that produced them, and the
// per-turn usage rollup keys one row per API response; this fixes the observed-thinking turn
// denominator (issue #98) and moves the golden fixtures. Second, usage dedup, tool-result
// patching, fallback merging, prompt facts, and the rollups are now computed by the rebuild's
// in-memory fold over complete information rather than by ON CONFLICT arithmetic; the fold is
// value-identical for well-formed transcripts, and migration 0042 makes every session read as due
// (parser_epoch DEFAULT 0), so the first boot rebuilds the corpus into the new shape.
//
// Epoch 13 -> 14: make tool-result-only user lines transparent to the Claude turn
// fold. A response with parallel tool calls logs each call's result between its own
// tool_use lines, and Epoch 13's fold treated any user line as ending the response,
// so the second and later calls of such a response landed as bare assistant rows
// with no usage attached. The fold now closes a turn only on a real user message or
// a different message.id, so every tool_use of one response folds into its one row.
// A parser output change: the golden fixtures move (the fixture carries the
// interleaved shape), and the rebuild re-folds the corpus.
//
// Epoch 14 -> 15: the insights rollup tables (migration 0048; see
// internal/server/store/rollups.go). rebuildTx now derives five per-session rollups
// (session_usage_daily, session_tool_rollup, session_file_churn, session_turns,
// session_activity_hourly) from the projection rows it writes, inside the same
// transaction, and the insights reads move onto them. The reducer's output is unchanged
// and the migration backfills the corpus from the existing projection, so nothing visible
// depends on the reparse this bump forces; the bump exists for the rolling-deploy race. An
// epoch-14 binary running beside this one rebuilds sessions without writing rollup rows,
// and nothing would ever mark such a session due again: with the bump, whatever the old
// binary rebuilt stays due (it stamps epoch 14) until a new binary re-derives it, so the
// rollups converge on every session regardless of deploy interleaving. Also the rule going
// forward: changing a rollup derivation is a rebuild-derived-output change and takes a
// bump, exactly like a reducer or scoring change.
//
// Epoch 15 -> 16: price the GPT-5.6 family (gpt-5.6-sol, gpt-5.6-terra,
// gpt-5.6-luna, and the gpt-5.6 alias that routes to sol; see internal/pricing).
// GPT-5.6 usage was unknown to the pricing table before, so its usage_events rows
// carry a NULL per-row cost and its sessions read cost_incomplete. This bump
// rebuilds the corpus so every GPT-5.6 usage row re-prices through pricing.Cost in
// one pass. It is a pricing change, not a reducer-shape change, and no golden
// fixture uses a GPT-5.6 model, so the projection delta for the fixtures is
// byte-for-byte identical and the golden snapshots do not move; the bump is the
// reprice signal and stands on its own.
const Epoch = 16
