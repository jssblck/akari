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
const Epoch = 1
