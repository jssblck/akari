# Agent notes for akari

Short orientation for coding agents. The full story lives in
[docs/development.md](docs/development.md) and [DESIGN.md](DESIGN.md); this file
just front-loads the three things that bite if you skip them.

## Generate templ before you build

The web UI is templ. The `.templ` files under `internal/server/web/` are the
source of truth; the `*_templ.go` they compile to are gitignored and not present
on a fresh clone. A bare `go build ./...` or `go test ./...` fails with
`undefined: web.*` until the generated code exists. Run the generate step first:

```sh
make build          # go generate ./... then go build ./...
make test           # go generate ./... then go test -race ./...
go generate ./...   # just regenerate after editing a *.templ
```

Re-run `go generate ./...` (or `make generate`) after editing any `*.templ`.

## Integration tests gate on a database

Tests that touch Postgres skip cleanly unless `AKARI_TEST_DATABASE_URL` is set.
A green `go test ./...` with the variable unset has silently skipped the store,
parse, and web integration tests. Under eph the variable is already set, so the
one-liner is:

```sh
eph run go test ./...
```

Without eph, point it at any Postgres whose role may create databases (each test
provisions and drops its own database beside the one the URL names):

```sh
AKARI_TEST_DATABASE_URL=postgres://akari:akari@localhost:5432/akari go test ./...
```

## Running the app locally

`eph up` brings up Postgres plus the server and seeds demo data; sign in as
`grace` (admin) with password `akari-dev`. See
[docs/development.md](docs/development.md) "Worktree-based development with eph"
and "Example data for development".

## Signals and the reparse epoch

Per-session signals (outcome, quality score and grade, tool health, prompt
hygiene, context health, observed thinking) live in `session_signals`, derived
from the projection.
A row is materialized by the settle pass (`RefreshSettledSignals`, run on a timer
from `cmd/akari-server` and available as `akari-server settle`) once the session
has been idle past the abandoned threshold, and re-derived on reparse. The ingest
append path deliberately does NOT compute signals: doing it per message would be
quadratic in a session's turns, and a verdict taken mid-session would drift once
the session crossed the idle threshold, so signals are computed once, after the
session settles, off the hot path. They sit OUTSIDE the `sessions.total_* ==
sum(usage_events)` invariant: a new signal derives from the same rows without
touching the rollups. Five version constants gate the machinery, and forgetting
one leaves a half-migrated corpus:

- `parse.Version` and `parse.Epoch` (`internal/server/parse`): bump both when
  parser or reducer output changes (a new or removed row, a changed field, a
  reprice). The golden-fixtures test (`internal/server/parse/epoch_test.go`)
  fails by name and tells you to bump the epoch and refresh the snapshots with
  `go test ./internal/server/parse -run TestGoldenProjection -update`.
- `quality.Version` (`internal/quality`): bump when the signal set or the scoring
  changes, so the analytics count only rebuilt rows. The settle pass treats a
  stale-version row as due and re-stamps it once the session is settled, so a
  version bump backfills incrementally on its own. The `Epoch` bump forces a
  one-time full reparse that re-derives every row atomically, the belt-and-suspenders
  path when you want the whole corpus current on first deploy regardless of whether
  the settle loop is enabled.
- `quality.PromptFactsVersion` (`internal/quality`): bump when `ClassifyPrompt`'s
  output changes (a new or changed prompt flag, a different duplicate-digest
  normalization). Unlike `quality.Version`, these facts are cached on the messages
  row and can only be re-derived by re-inserting the message, so the settle pass
  cannot re-derive them: `gatherPromptHygiene` treats an older-version row like an
  unfilled one and leaves the session ungraded until a reparse fills it. Because the
  settle pass cannot fix such a session, `refreshSignalsTx` also clears its
  `signals_stale` flag so it drops out of the settle-due set rather than being
  re-scanned every wake (an unfixable transcript, one whose reparse fails
  deterministically, would otherwise be polled forever). The clear is guarded by
  `NOT EXISTS` a current-version `session_signals` row: a session that once graded
  cleanly and later had its facts superseded keeps `signals_stale = true` so its now
  pre-append row stays hidden instead of reading as current, and stays due until the
  paired epoch reparse re-derives it. The reparse that fills the facts re-marks a
  droppable session due through `applyAggregates` and grades it, so nothing is lost. The version
  is also stamped on the `session_signals` row (`prompt_facts_version`) and gated at
  the two hygiene read sites (`promptHygieneFrom` and the hygiene half of
  `SessionSignalsByID`), so a graded aggregate at a superseded classifier version
  reads as unmeasured until the reparse re-derives it, rather than mixing classifier
  versions into a fleet count while the current `signals_version` still passes. Pair
  the bump with a `parse.Epoch` bump so the corpus reparses and re-derives every
  message's facts at the new version (the reparse re-stamps both the messages and the
  `session_signals` versions).
- `pricing.Version` (`internal/pricing`): bump on any rate change in the pricing
  table (a new or removed model, a different Input/Output/CacheWrite/CacheRead
  number, or a new or moved date-effective window). A model maps to a list of
  date-effective rates and `Cost`/`CacheSavings` select the window in effect at the
  usage event's time, so adding a window is still a reprice: a session logged inside
  the new window re-prices. It gates `reconcileCacheSavingsPricingIfNeeded`, which
  re-prices the per-session cache-savings rollup across the cache-bearing corpus once
  per bump, so a session whose reparse fails still re-prices from its usage_events at
  the new rates (per-row cost rides the `Epoch` reparse instead). It is separate from
  `parse.Epoch` on purpose: keying off Epoch would re-price the whole corpus on
  every unrelated parser change. Pair a `pricing.Version` bump with the
  `parse.Epoch` bump a reprice already requires.

A new signal should default to a value that reads as "unmeasured" (NULL, or a
zero the aggregate excludes) until the settle pass (or a backfill reparse) fills it.

Observed thinking bands on an absolute token scale, not at settle time. The
canonical unit is the per-turn estimated reasoning-token count, and a turn (or a
session's headline turn) sits in the band its token count reaches: low (0, 128],
medium (128, 512], high (512, 2048], xhigh above. The edges are baked constants
(`quality.ThinkingLowMaxTokens` and friends), applied at read time by
`quality.ThinkingBucketForTokens`, shared with the store's SQL aggregate as bound
parameters so the two cannot drift. An absolute scale is deliberate: the first cut
ranked each session against its model's cohort with a `cume_dist`, but quartiles are
25%-each by construction, so a fleet distribution over them was tautological. A fixed
token cut tracks the fleet's real distribution and shifts when behavior shifts.

The settle pass stores raw per-session scalars (`assistant_turns`, `thinking_turns`,
`thinking_tail_tokens`, `thinking_peak_tokens`; see `internal/quality/thinking.go`
and migration 0041), never a band. The tail is the session's headline volume: the
mean of the hardest tenth of its thinking turns (`ceil(thinking_turns / 10)`), a tail
statistic rather than an all-turn average, because most turns barely reason so a plain
mean collapses to the floor while a bare max lets one outlier define the session. The
session band reads off the tail; the peak (the single hardest turn) rides alongside.
All four are NULL together when the session had no assistant turns (nothing to
measure, so the UI reads absence, not "off"). These scalars back the session readout
(the Quality tooltip's Thinking group) and, per message, the transcript's thinking-band
chip.

Known limitation, and why the fleet view is deferred. The "turn" unit is wrong for
Claude today. A Claude Code assistant response is one API message, but the harness logs
each of its content blocks on its own JSONL line, so the thinking block, the reply text,
and every `tool_use` become separate `messages` rows that all share one `message.id`. The
parser maps one line to one row (`reduceClaude`), so a row-level distribution is dominated
by tool-call rows that structurally carry no thinking: on the real corpus ~51% of Claude
assistant rows are pure tool-call rows and only ~18% carry a reasoning block, so a per-row
"off" share reads ~82% while grouping the same rows by `message.id` puts it near ~15%
(Codex is unaffected: `reduceCodex` already folds a whole turn into one row). The
fleet/insights distribution over rows was therefore removed until the unit is fixed (group
rows by the API message id, or collapse the split at parse time; see issue #98). The per-session tail and
peak are volume figures per thinking block, so they are not distorted by the split and stay;
the coverage figure ("N of M turns") shares the row-count denominator and is a known
casualty folded into the same fix.

Each turn's tokens are its exact reasoning-token count where the agent reports one
(Codex logs it per turn in `message_turn_usage.reasoning_tokens`), else its
reasoning-trace bytes over an agent-calibrated bytes-per-token factor. `perTurnTokensExpr`
(store) builds that expression for the settle derivation (and the fleet aggregate when it
returns), single-sourcing the divisors from `quality.ThinkingBytesPerToken` so the SQL
matches the Go mapping. The byte estimate is trustworthy: measured against Codex's exact
counts and the rare Claude blocks that kept their plaintext, the per-turn medians agree
within ~2%, so a token figure is comparable across models without the per-model ranking
the first cut needed.

The trace bytes come from the reasoning the agents log, and the catch is that current
Claude Code and Codex redact it: the reasoning ships encrypted (Claude leaves a
`signature`, Codex an `encrypted_content` blob) with the plaintext dropped, so ~97% of
real Claude thinking blocks carry no text. The parser weighs each turn by its
reasoning-trace byte size (`messages.thinking_bytes`), plaintext where present and the
encrypted payload length otherwise; the ciphertext length tracks the hidden reasoning
volume closely (r=0.97 for Claude signatures, r=0.997 for Codex against the
reasoning-token count it reports), and pi keeps its thinking in the clear. `has_thinking`
is set on the presence of a reasoning block, not on non-empty text, so a redacted turn
still counts. Because `thinking_bytes` is a plain parser-filled column (not generated), it
is populated by a reparse: the `parse.Epoch 11 -> 12` bump reparses the corpus to fill it
(and each turn's `reasoning_tokens`) and re-derives every `session_signals` row at the
current `quality.Version` in the same pass.

`quality.ThinkingScaleVersion` marks the calibration: the bytes-per-token factors and the
band edges. Bump it on any change to either. A factor change moves the stored per-session
tokens, so pair it with a `quality.Version` bump (the settle pass re-derives the scalars);
an edge change only moves the read-time band and can ride the scale version alone. It is a
human-facing marker of "what "high" means today", not a gate: the stored scalars already
carry their derivation version through `signals_version`.
