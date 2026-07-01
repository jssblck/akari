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
// An Epoch bump is also the way to backfill a derived projection table that did not
// exist before. A caught-up session never re-enters AdvanceProjection, so a table
// the live path now fills (session_signals) stays empty for old sessions until
// something reparses them. The reparse is also where session_signals is rebuilt to
// the running quality.Version, so a scoring-model change rides the same signal. Such
// a bump leaves the parser's projection delta byte-for-byte identical, so the golden
// fixtures do not move; the bump is intentional and stands on its own.
//
// Epoch 1 -> 2: introduce session_signals (per-session outcome, quality score and
// grade, and tool-health counts), computed on catch-up and reparse. The bump
// backfills the table across the existing corpus.
//
// Epoch 2 -> 3: add prompt-hygiene signals to session_signals (quality.Version 1 -> 2).
// The projection delta is unchanged (the new signals derive from stored messages), so
// the golden fixtures do not move; the bump exists only to rebuild every signals row at
// the current version, backfilling the hygiene counts across the existing corpus.
//
// Epoch 3 -> 4: capture is_sidechain on usage events (parse.Version 3 -> 4) and add
// context-health signals to session_signals (peak context tokens and inferred context
// resets; quality.Version 2 -> 3). Unlike the hygiene bump this one DOES move the golden
// fixtures: the usage rows in the projection delta gain the new field, so the snapshots
// are refreshed in the same commit. The reparse backfills the flag and recomputes every
// signals row at the current version across the corpus.
//
// Epoch 4 -> 5: remove is_sidechain again (parse.Version 4 -> 5). A subagent is a
// separate transcript file, ingested as its own session, so a main session's usage never
// carried subagent turns and the flag guarded a case that never occurred. The field
// leaves the projection delta, so this bump also moves the golden fixtures, and
// context-health analysis now reads a session's own turns with no carve-out
// (quality.Version 3 -> 4). The reparse rewrites the usage rows without the field and
// recomputes every signals row at the current version.
const Epoch = 5
