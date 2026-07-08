# Signals and the parse epoch

This is the full reference for the per-session signals subsystem. AGENTS.md
carries the short version; read this before changing signals, scoring, prompt
classification, pricing, or the observed-thinking calibration.

Per-session signals (outcome, quality score and grade, tool health, prompt
hygiene, context health, observed thinking) live in `session_signals`, derived
from the projection. The ingest path never computes them: a verdict taken
mid-session drifts (abandoned versus unknown depends on how long the session has
been idle), so a session is graded once it is stable. Two paths materialize the
row, both owned by the parse worker (see "Server-side parsing pipeline" in
docs/DESIGN.md):

- A rebuild of a settled or terminal session recomputes signals inside the
  rebuild transaction, so an epoch rollout re-grades the corpus as it reparses.
- The worker's maintenance tick (`AKARI_SIGNALS_SETTLE_INTERVAL`, one-shot form
  `akari-server settle`) grades sessions whose projection is current but
  ungraded: a session that took its last chunk while live and has since idled
  past the abandoned threshold (30 minutes), or one the client declared terminal
  (`akari sync --finalize`, which also triggers the grade inline so an ephemeral
  host does not wait for the tick).

Both standalone grading paths (the tick and the finalize request) grade only a
session whose parse state is settled at the running epoch: the raw bytes are
covered by the last rebuild (or by a deterministic failure pinned at the running
epoch, whose surviving projection is what gets graded), and neither the stamp
nor a recorded failure sits ahead of the running epoch. Anything else skips,
leaving `signals_stale` set: a pending rebuild grades the session itself when it
commits, and a state stamped ahead belongs to a newer binary during a rolling
deploy, whose own tick grades it. Without the skip, a finalize racing the parse
worker could grade a projection that does not cover the bytes, and an older
binary could overwrite a newer binary's work with old scoring and clear the one
flag that would trigger a redo.

`sessions.signals_stale` tracks the second case: a rebuild of a still-live
session sets it and drops the now-stale signals row; grading clears it. Signals
sit outside the rollup/ledger invariant (docs/data-aggregation.md): a new signal
derives from the same projection rows without touching the rollups.

## The one version constant

`parse.Epoch` (`internal/server/parse`) is the only version gate for derived
data. Bump it in the same commit as any change that alters what is derived from
the raw bytes:

- parser or reducer output (a new or removed row, a changed field, a different
  fold),
- a column the rebuild derives (prompt facts, `duplicate_prompt`,
  `file_rel_path`, the per-turn usage rollup),
- an insights rollup derivation (`internal/server/store/rollups.go`: the five
  session-grain tables rebuildTx writes; the reparse the bump forces is what
  re-derives the corpus),
- the signal set, the scoring, or the outcome rules,
- prompt classification (`quality.ClassifyPrompt`),
- the pricing table (a reprice changes stored per-row cost and the
  cache-savings rollup),
- the observed-thinking calibration where it changes stored scalars (the
  bytes-per-token factors; a band-edge change is applied at read time and needs
  no bump).

A bumped epoch makes every session read as due, and the parse worker rebuilds
the corpus in the background on the next deploy: projection rows, derived
columns, signals, and pricing all re-derive in one pass. There is no such thing
as a half-migrated corpus, and no second constant to forget. The
golden-fixtures test (`internal/server/parse/epoch_test.go`) fails by name when
projection output drifts without a bump; refresh the snapshots with
`go test ./internal/server/parse -run TestGoldenProjection -update`.

A new signal should default to a value that reads as "unmeasured" (NULL, or a
zero the aggregate excludes), so a session graded before the signal existed
reads as absent rather than zero until its rebuild or settle grade fills it.

## Observed thinking

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

A "turn" is a semantic assistant turn: one API response. The reducer folds
Claude's split content-block lines (which share one `message.id`) into a single
`messages` row, the same shape Codex and pi already have, so a turn-level count
is never diluted by tool-call rows that structurally carry no reasoning (issue
#98). The per-session scalars (`assistant_turns`, `thinking_turns`,
`thinking_tail_tokens`, `thinking_peak_tokens`; see
`internal/quality/thinking.go` and migration 0041) are derived over those turns
by the grading pass, never a band. The tail is the session's headline volume:
the mean of the hardest tenth of its thinking turns
(`ceil(thinking_turns / 10)`), a tail statistic rather than an all-turn average,
because most turns barely reason so a plain mean collapses to the floor while a
bare max lets one outlier define the session. The session band reads off the
tail; the peak (the single hardest turn) rides alongside. All four are NULL
together when the session had no assistant turns (nothing to measure, so the UI
reads absence, not "off"). These scalars back the session readout (the Quality
tooltip's Thinking group), the per-turn transcript band chip, the coverage line
("N of M turns"), and the fleet distribution on the insights page.

Each turn's tokens are its exact reasoning-token count where the agent reports
one (Codex logs it per turn in `message_turn_usage.reasoning_tokens`), else its
reasoning-trace bytes over an agent-calibrated bytes-per-token factor.
`perTurnTokensExpr` (store) builds that expression for the grading derivation and
the fleet aggregate, single-sourcing the divisors from
`quality.ThinkingBytesPerToken` so the SQL matches the Go mapping. The byte
estimate is trustworthy: measured against Codex's exact counts and the rare
Claude blocks that kept their plaintext, the per-turn medians agree within ~2%,
so a token figure is comparable across models.

The trace bytes come from the reasoning the agents log, and the catch is that
current Claude Code and Codex redact it: the reasoning ships encrypted (Claude
leaves a `signature`, Codex an `encrypted_content` blob) with the plaintext
dropped, so ~97% of real Claude thinking blocks carry no text. The parser weighs
each turn by its reasoning-trace byte size (`messages.thinking_bytes`),
plaintext where present and the encrypted payload length otherwise; the
ciphertext length tracks the hidden reasoning volume closely (r=0.97 for Claude
signatures, r=0.997 for Codex against the reasoning-token count it reports), and
pi keeps its thinking in the clear. `has_thinking` is set on the presence of a
reasoning block, not on non-empty text, so a redacted turn still counts.
