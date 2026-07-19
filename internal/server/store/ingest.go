package store

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// AnnounceParams carries what a client reports when announcing a session. Kind
// is the session's classification ("remote", "standalone", or "orphaned"); it
// gates the downgrade guard in Announce.
type AnnounceParams struct {
	UserID          int64
	Agent           string
	SourceSessionID string
	ProjectID       int64
	Kind            string
	GitBranch       string
	Cwd             string
	Machine         string
	// Terminal is the client's assertion that this session is finished (an
	// `akari sync --finalize` on an ephemeral host). It is persisted sticky (OR'd
	// onto the stored flag) and OR'd into the server-side idle checks so the session
	// grades immediately rather than waiting out the abandoned-idle window. A normal
	// watch-loop announce leaves it false.
	Terminal bool
}

// ProjectParams is the project identity carried by an announce request before
// the server knows the row id. Keeping this with Announce lets the downgrade
// guard run before a local project is inserted, so an old client cannot recreate
// an unused orphaned project row for a session already stuck to a remote.
type ProjectParams struct {
	RemoteKey   string
	Host        string
	Owner       string
	Repo        string
	DisplayName string
	Kind        string
}

// AnnounceResult is the server's authoritative view of a session's raw store.
type AnnounceResult struct {
	SessionID    int64
	StoredBytes  int64
	PrefixSHA256 string
}

// emptySHA256 is the hex sha256 of zero bytes. A freshly announced session holds
// nothing, and the protocol still reports a valid hash for the empty prefix so a
// client can compare uniformly (its hash of zero local bytes equals this).
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// OffsetMismatchError reports that an append was attempted at the wrong offset;
// StoredBytes is the server's current cursor, which the client should resume at.
type OffsetMismatchError struct{ StoredBytes int64 }

func (e OffsetMismatchError) Error() string {
	return fmt.Sprintf("offset mismatch: server holds %d bytes", e.StoredBytes)
}

// UpsertProject inserts the project keyed by its remote/synthetic key, or
// refreshes last_seen on an existing one, returning the project id. The kind is
// updated on conflict so a standalone folder that is later deleted transitions to
// orphaned in place (its key, machine + path, is unchanged), and one that gains a
// remote is never re-resolved here: a remote session carries its own remote key.
func (s *Store) UpsertProject(ctx context.Context, remoteKey, host, owner, repo, displayName, kind string) (int64, error) {
	return upsertProjectTx(ctx, s.Pool, remoteKey, host, owner, repo, displayName, kind)
}

type projectUpserter interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func upsertProjectTx(ctx context.Context, q projectUpserter, remoteKey, host, owner, repo, displayName, kind string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx,
		`INSERT INTO projects (remote_key, host, owner, repo, display_name, kind)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (remote_key) DO UPDATE SET last_seen = now(), kind = EXCLUDED.kind
		 RETURNING id`,
		remoteKey, host, owner, repo, displayName, kind).Scan(&id)
	return id, err
}

// Announce upserts the session row (latest announce wins for mutable metadata),
// ensures its raw-store row exists, and returns the current cursor and hash.
//
// Remote attribution is sticky: once a session resolves to a git-remote project,
// a later announce that can no longer find a remote (standalone or orphaned,
// because the folder lost its origin or was deleted) does not move it to a local
// project. Backed-up work keeps its repo grouping rather than sliding into an
// orphaned bucket the moment its checkout is removed. An upgrade in the other
// direction (a local session that gains a remote) is allowed and re-homes it.
func (s *Store) Announce(ctx context.Context, p AnnounceParams) (AnnounceResult, error) {
	var r AnnounceResult
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockAnnounceIdentityTx(ctx, tx, p); err != nil {
			return err
		}
		var kept bool
		var err error
		r, kept, err = keepRemoteAttributionTx(ctx, tx, p)
		if err != nil || kept {
			return err
		}
		r, err = announceIntoProjectTx(ctx, tx, p)
		return err
	})
	if err == nil && r.StoredBytes == 0 {
		r.PrefixSHA256 = emptySHA256
	}
	return r, err
}

// AnnounceWithProject upserts the project and the session in one transaction.
// For non-remote announces it first applies the sticky remote guard; when the
// guard wins, the local project is never inserted. The HTTP ingest path uses
// this form because it receives a project identity rather than a project id.
func (s *Store) AnnounceWithProject(ctx context.Context, p AnnounceParams, project ProjectParams) (AnnounceResult, error) {
	var r AnnounceResult
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockAnnounceIdentityTx(ctx, tx, p); err != nil {
			return err
		}
		var kept bool
		var err error
		r, kept, err = keepRemoteAttributionTx(ctx, tx, p)
		if err != nil || kept {
			return err
		}
		p.ProjectID, err = upsertProjectTx(ctx, tx, project.RemoteKey, project.Host, project.Owner, project.Repo, project.DisplayName, project.Kind)
		if err != nil {
			return fmt.Errorf("upsert project for announce: %w", err)
		}
		r, err = announceIntoProjectTx(ctx, tx, p)
		return err
	})
	if err == nil && r.StoredBytes == 0 {
		r.PrefixSHA256 = emptySHA256
	}
	return r, err
}

// lockAnnounceIdentityTx serializes announces for one logical client session.
// The remote-attribution guard is read-before-write, so a local announce that
// started before a concurrent remote announce must wait and re-read the settled
// project before deciding whether to keep or move attribution.
//
// It first takes the coarser family lock (see lockAnnounceFamilyTx), always before the
// identity lock, so the two are acquired in one order and cannot deadlock.
func lockAnnounceIdentityTx(ctx context.Context, tx pgx.Tx, p AnnounceParams) error {
	if err := lockAnnounceFamilyTx(ctx, tx, p); err != nil {
		return err
	}
	_, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(
			hashtext(current_database() || ':announce-session'),
			hashtext($1::bigint::text || chr(31) || $2 || chr(31) || $3)
		)`,
		p.UserID, p.Agent, p.SourceSessionID)
	if err != nil {
		return fmt.Errorf("lock announce session identity: %w", err)
	}
	return nil
}

// lockAnnounceFamilyTx serializes a Claude session and all of its subagents on one key,
// the parent's source id, so linkSubagentParentTx never runs concurrently for a parent
// and one of its children. Without it the link is a check-then-act across two separately
// announced rows: under READ COMMITTED a parent and a child announcing at once can each
// run their link step before the other's session row commits, so neither sees the other,
// both commit, and the child's parent_session_id stays NULL with no later announce to
// retry it. A subagent's family key is its parent-source prefix, a top-level session's is
// its own source id, so a parent and every child hash to the same lock. The identity lock
// namespace differs ('announce-session'), so the two never false-share. Only Claude nests
// subagents, so nothing else pays for this.
func lockAnnounceFamilyTx(ctx context.Context, tx pgx.Tx, p AnnounceParams) error {
	if p.Agent != "claude" {
		return nil
	}
	family := p.SourceSessionID
	if parentSource, ok := subagentParentSource(p.SourceSessionID); ok {
		family = parentSource
	}
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(
			hashtext(current_database() || ':announce-family'),
			hashtext($1::bigint::text || chr(31) || $2)
		)`,
		p.UserID, family); err != nil {
		return fmt.Errorf("lock announce family: %w", err)
	}
	return nil
}

func keepRemoteAttributionTx(ctx context.Context, tx pgx.Tx, p AnnounceParams) (AnnounceResult, bool, error) {
	var r AnnounceResult
	if p.Kind == "" || p.Kind == "remote" {
		return r, false, nil
	}
	var existingID int64
	var existingKind string
	var priorCwd string
	err := tx.QueryRow(ctx,
		`SELECT s.id, pr.kind, COALESCE(s.cwd, '')
		   FROM sessions s JOIN projects pr ON pr.id = s.project_id
		  WHERE s.user_id = $1 AND s.agent = $2 AND s.source_session_id = $3`,
		p.UserID, p.Agent, p.SourceSessionID).Scan(&existingID, &existingKind, &priorCwd)
	switch {
	case err == nil && existingKind == "remote":
		r.SessionID = existingID
		// Keep only the remote project attribution. Mutable announce metadata still
		// follows the latest client observation, and terminal remains sticky.
		parentSource := announceParentSource(p)
		if _, err := tx.Exec(ctx,
			`UPDATE sessions
			    SET machine = $2, cwd = $3, git_branch = $4,
			        terminal = sessions.terminal OR $5,
			        parent_source_id = CASE WHEN $6 <> '' THEN $6 ELSE parent_source_id END,
			        updated_at = now()
			  WHERE id = $1`,
			existingID, p.Machine, p.Cwd, p.GitBranch, p.Terminal, parentSource); err != nil {
			return AnnounceResult{}, false, fmt.Errorf("update kept remote session %d metadata: %w", existingID, err)
		}
		if err := refreshCwdDerivedStateTx(ctx, tx, existingID, priorCwd, p.Cwd); err != nil {
			return AnnounceResult{}, false, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO session_raw (session_id) VALUES ($1) ON CONFLICT DO NOTHING`, existingID); err != nil {
			return AnnounceResult{}, false, err
		}
		if err := linkSubagentParentTx(ctx, tx, p, existingID); err != nil {
			return AnnounceResult{}, false, err
		}
		err := tx.QueryRow(ctx,
			`SELECT byte_len, content_sha256 FROM session_raw WHERE session_id = $1`, existingID).
			Scan(&r.StoredBytes, &r.PrefixSHA256)
		return r, true, err
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return AnnounceResult{}, false, err
	default:
		return AnnounceResult{}, false, nil
	}
}

func announceIntoProjectTx(ctx context.Context, tx pgx.Tx, p AnnounceParams) (AnnounceResult, error) {
	var r AnnounceResult
	// Read the session's stored cwd (if it already exists) before the upsert overwrites it, so a
	// changed cwd can be detected and the dependent tool_calls.file_rel_path projection recomputed
	// below. The announce identity lock is held for the whole transaction, so this read-then-write
	// cannot interleave with a concurrent announce for the same logical session. A fresh session
	// (no row yet) reads the empty string and never triggers a recompute (it has no tool calls).
	var priorCwd string
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(cwd, '') FROM sessions WHERE user_id = $1 AND agent = $2 AND source_session_id = $3`,
		p.UserID, p.Agent, p.SourceSessionID).Scan(&priorCwd); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AnnounceResult{}, fmt.Errorf("read prior cwd for announce: %w", err)
	}
	// terminal is sticky: OR the incoming value onto the stored one so a --finalize
	// announce sets it and a later ordinary re-announce (watch loop) of the same
	// session never clears it. A once-terminal session stays terminal.
	parentSource := announceParentSource(p)
	if err := tx.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine, cwd, git_branch, terminal, parent_source_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (user_id, agent, source_session_id) DO UPDATE
		   SET project_id = EXCLUDED.project_id,
		       machine    = EXCLUDED.machine,
		       cwd        = EXCLUDED.cwd,
		       git_branch = EXCLUDED.git_branch,
		       terminal   = sessions.terminal OR EXCLUDED.terminal,
		       parent_source_id = CASE
		         WHEN EXCLUDED.parent_source_id <> '' THEN EXCLUDED.parent_source_id
		         ELSE sessions.parent_source_id
		       END,
		       updated_at = now()
		 RETURNING id`,
		p.UserID, p.ProjectID, p.Agent, p.SourceSessionID, p.Machine, p.Cwd, p.GitBranch, p.Terminal, parentSource).Scan(&r.SessionID); err != nil {
		return AnnounceResult{}, fmt.Errorf("upsert session for announce: %w", err)
	}
	// file_rel_path is a projection of the session's cwd and each tool call's file_path (derived at
	// insert in projection.go through sessionRelPath), so it must follow its inputs: the announce
	// upsert above can change cwd (cwd = EXCLUDED.cwd), which would strand every already-inserted
	// rel path at the old anchor and split one repo file's churn across two keys. When the cwd
	// actually changed, recompute the stored rel paths against the new cwd. A session's tool calls
	// are bounded (it is one session's edits, pushed to the CAS, not whole-session bytes), and a
	// cwd change is rare (a re-announce from a different checkout), so this is a rare, bounded write
	// rather than hot-path work.
	if err := refreshCwdDerivedStateTx(ctx, tx, r.SessionID, priorCwd, p.Cwd); err != nil {
		return AnnounceResult{}, err
	}
	if err := linkSubagentParentTx(ctx, tx, p, r.SessionID); err != nil {
		return AnnounceResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO session_raw (session_id) VALUES ($1) ON CONFLICT DO NOTHING`, r.SessionID); err != nil {
		return AnnounceResult{}, err
	}
	return r, tx.QueryRow(ctx,
		`SELECT byte_len, content_sha256 FROM session_raw WHERE session_id = $1`, r.SessionID).
		Scan(&r.StoredBytes, &r.PrefixSHA256)
}

// refreshCwdDerivedStateTx updates the two projections that depend on a
// session's cwd. Announce can change that anchor without appending raw bytes, so
// no parse rebuild will arrive later to repair stale relative paths or churn
// keys.
func refreshCwdDerivedStateTx(ctx context.Context, tx pgx.Tx, sessionID int64, priorCwd, cwd string) error {
	if priorCwd == cwd {
		return nil
	}
	if err := recomputeToolCallRelPathsTx(ctx, tx, sessionID, cwd); err != nil {
		return err
	}
	if err := deriveSessionFileChurnTx(ctx, tx, sessionID); err != nil {
		return err
	}
	return nil
}

// relPathRecomputeBatch is how many tool-call rows recomputeToolCallRelPathsTx reads and writes per
// keyset page. It bounds peak memory to a fixed window (this many (ordinal, call_index, path)
// tuples) regardless of how many tool calls a session accumulated, so the recompute does not hold a
// per-session-sized slice resident. It is large enough that a typical session recomputes in one or
// two pages.
const relPathRecomputeBatch = 512

// recomputeToolCallRelPathsTx re-derives every tool_calls.file_rel_path for a session against a
// new cwd, so the stored projection follows its inputs when an announce changes cwd. file_rel_path
// is computed once at insert (projection.go) from the cwd known then; a later announce from a
// different checkout would otherwise leave the column pinned to the stale anchor, fragmenting one
// repo file's churn across the old and new relative keys. It recomputes the relative form with the
// same sessionRelPath the insert uses (so the recompute and the live path never diverge), setting
// NULL for a row with no stable relative form under the new cwd (a path outside the new workspace,
// or one that never had an absolute anchor), matching the insert's rule.
//
// It pages by keyset on the tool_calls primary key (message_ordinal, call_index) in fixed-size
// batches rather than buffering the whole session's rows: peak memory tracks relPathRecomputeBatch
// tuples, not the session's total tool-call count, so an arbitrarily large session recomputes in
// bounded memory. Each page reads its batch, closes its rows (pgx forbids a write on the same
// connection while a query's rows are open), applies the batch's updates, then resumes strictly
// after the last key it saw. A cwd change is rare (a re-announce from a different checkout), so this
// pass runs off the hot path.
func recomputeToolCallRelPathsTx(ctx context.Context, tx pgx.Tx, sessionID int64, cwd string) error {
	// update is one row's key and its recomputed relative path (nil for NULL), buffered only for the
	// current page.
	type update struct {
		ordinal, callIndex int
		rel                any
	}
	// The keyset cursor: the last (ordinal, call_index) written, so the next page resumes strictly
	// after it. Starts below any real key.
	lastOrdinal, lastCallIndex := -1, -1
	for {
		rows, err := tx.Query(ctx,
			`SELECT message_ordinal, call_index, COALESCE(file_path, '')
			   FROM tool_calls
			  WHERE session_id = $1 AND (message_ordinal, call_index) > ($2, $3)
			  ORDER BY message_ordinal, call_index
			  LIMIT $4`,
			sessionID, lastOrdinal, lastCallIndex, relPathRecomputeBatch)
		if err != nil {
			return fmt.Errorf("read tool calls to recompute rel paths for session %d: %w", sessionID, err)
		}
		batch := make([]update, 0, relPathRecomputeBatch)
		for rows.Next() {
			var u update
			var filePath string
			if err := rows.Scan(&u.ordinal, &u.callIndex, &filePath); err != nil {
				rows.Close()
				return fmt.Errorf("scan tool call for rel-path recompute in session %d: %w", sessionID, err)
			}
			if r, ok := sessionRelPath(cwd, filePath); ok {
				u.rel = r
			}
			batch = append(batch, u)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate tool calls for rel-path recompute in session %d: %w", sessionID, err)
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		for _, u := range batch {
			if _, err := tx.Exec(ctx,
				`UPDATE tool_calls SET file_rel_path = $4
				   WHERE session_id = $1 AND message_ordinal = $2 AND call_index = $3`,
				sessionID, u.ordinal, u.callIndex, u.rel); err != nil {
				return fmt.Errorf("update tool call rel path %d/%d for session %d: %w", u.ordinal, u.callIndex, sessionID, err)
			}
		}
		last := batch[len(batch)-1]
		lastOrdinal, lastCallIndex = last.ordinal, last.callIndex
		if len(batch) < relPathRecomputeBatch {
			return nil // a short page is the last page
		}
	}
}

// subagentMarker is the path segment the client's source id uses to nest a Claude
// subagent or workflow transcript under the session that spawned it: a child's source id
// is "<parent source id>/subagents/...". Splitting on it recovers the parent's source id.
const subagentMarker = "/subagents/"

// subagentParentSource returns the parent session's source id for a subagent transcript,
// or ok=false for a top-level session. Only Claude nests children this way (Codex and Pi
// write one flat id per session), so a non-Claude source id never matches. A marker at the
// very start has no parent before it and is rejected.
func subagentParentSource(sourceID string) (parentSource string, ok bool) {
	i := strings.Index(sourceID, subagentMarker)
	if i <= 0 {
		return "", false
	}
	return sourceID[:i], true
}

func announceParentSource(p AnnounceParams) string {
	if p.Agent != "claude" {
		return ""
	}
	parentSource, _ := subagentParentSource(p.SourceSessionID)
	return parentSource
}

// linkSubagentParentTx links from the stored parent-source key, whether announce
// derived it from a Claude source id or a rebuild parsed it from another agent's
// transcript. It also lets the announcing row adopt children that arrived first.
// Both writes are guarded so an established relationship is never rewritten.
func linkSubagentParentTx(ctx context.Context, tx pgx.Tx, p AnnounceParams, sessionID int64) error {
	if _, err := tx.Exec(ctx,
		`UPDATE sessions AS child
		    SET parent_session_id = parent.id, relationship_type = 'subagent'
		   FROM sessions AS parent
		  WHERE child.id = $1
		    AND child.parent_session_id IS NULL
		    AND child.parent_source_id <> ''
		    AND parent.user_id = $2
		    AND parent.agent = $3
		    AND parent.source_session_id = child.parent_source_id
		    AND parent.id <> child.id`,
		sessionID, p.UserID, p.Agent); err != nil {
		return fmt.Errorf("link subagent session %d to parent: %w", sessionID, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE sessions
		    SET parent_session_id = $1, relationship_type = 'subagent'
		  WHERE user_id = $2 AND agent = $3
		    AND parent_session_id IS NULL
		    AND parent_source_id <> ''
		    AND parent_source_id = $4
		    AND id <> $1`,
		sessionID, p.UserID, p.Agent, p.SourceSessionID); err != nil {
		return fmt.Errorf("adopt subagents of session %d: %w", sessionID, err)
	}
	return nil
}

// SessionMeta returns the owning user and agent of a session, or ErrNotFound.
func (s *Store) SessionMeta(ctx context.Context, sessionID int64) (userID int64, agent string, err error) {
	err = s.Pool.QueryRow(ctx, "SELECT user_id, agent FROM sessions WHERE id = $1", sessionID).Scan(&userID, &agent)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return userID, agent, err
}

// ErrChunkNotLineAligned reports a chunk that is empty or does not end on a
// newline. The ingest protocol requires every stored byte to rest on a JSONL line
// boundary so the server only ever parses complete lines; the server enforces it
// rather than trusting the client.
var ErrChunkNotLineAligned = errors.New("chunk must be non-empty and end on a newline")

// AppendChunk appends data at the given offset as a new raw chunk row. If offset
// does not match the server's current byte_len it returns OffsetMismatchError
// with the truth and makes no change. The prefix hash is advanced by resuming the
// stored sha256 state and folding in only the new bytes, so appending is work
// proportional to the chunk, not to the whole session.
func (s *Store) AppendChunk(ctx context.Context, sessionID, offset int64, data []byte) (newStoredBytes int64, err error) {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return 0, ErrChunkNotLineAligned
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var current int64
		var hashState []byte
		if err := tx.QueryRow(ctx,
			`SELECT byte_len, sha256_state FROM session_raw WHERE session_id = $1 FOR UPDATE`, sessionID).
			Scan(&current, &hashState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock session_raw to append to session %d at offset %d: %w", sessionID, offset, err)
		}
		if current != offset {
			return OffsetMismatchError{StoredBytes: current}
		}

		h := sha256.New()
		if len(hashState) > 0 {
			if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(hashState); err != nil {
				return fmt.Errorf("restore hash state for session %d: %w", sessionID, err)
			}
		}
		h.Write(data)
		newState, err := h.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil {
			return fmt.Errorf("marshal sha256 state for session %d: %w", sessionID, err)
		}
		newStoredBytes = current + int64(len(data))

		if _, err := tx.Exec(ctx,
			`INSERT INTO session_raw_chunks (session_id, byte_offset, byte_len, content)
			 VALUES ($1, $2, $3, $4)`,
			sessionID, offset, int64(len(data)), data); err != nil {
			return fmt.Errorf("insert raw chunk for session %d at offset %d: %w", sessionID, offset, err)
		}
		// New bytes clear any operational-failure backoff: the situation changed
		// (a re-sync that finally uploaded a missing CAS blob also appends), so
		// the wake this append triggers should retry immediately.
		if _, err := tx.Exec(ctx,
			`UPDATE session_raw
			    SET byte_len = $2, content_sha256 = $3, sha256_state = $4,
			        parse_retry_at = NULL, parse_retry_backoff_secs = 0
			  WHERE session_id = $1`,
			sessionID, newStoredBytes, hex.EncodeToString(h.Sum(nil)), newState); err != nil {
			return fmt.Errorf("advance raw cursor and hash for session %d: %w", sessionID, err)
		}
		return nil
	})
	return newStoredBytes, err
}

// ResetRaw clears a session's raw store and its derived rows so the next chunk
// starts from zero. Dropping the tool_calls and attachments can orphan CAS
// blobs; like any deletion or rebuild, those are reclaimed by a later
// SweepBlobs rather than synchronously here, so a client reset stays cheap.
//
// It takes the parent session row lock and then the session_raw lock, the locks
// AppendChunk and RebuildSession serialize on, so a reset cannot interleave
// with an in-flight append or rebuild and leave behind a chunk row or projection
// rows for a session it just zeroed. Taking the session row before session_raw
// matches DeleteSession's order, so the two cannot deadlock.
func (s *Store) ResetRaw(ctx context.Context, sessionID int64) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		var dummy int64
		if err := tx.QueryRow(ctx,
			`SELECT session_id FROM session_raw WHERE session_id = $1 FOR UPDATE`, sessionID).Scan(&dummy); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock session_raw for reset of session %d: %w", sessionID, err)
		}
		for _, q := range []string{
			"DELETE FROM messages WHERE session_id = $1",
			"DELETE FROM tool_calls WHERE session_id = $1",
			"DELETE FROM usage_events WHERE session_id = $1",
			// The per-turn usage rollup is derived from usage_events, so it clears with them.
			"DELETE FROM message_turn_usage WHERE session_id = $1",
			"DELETE FROM attachments WHERE session_id = $1",
			// The model-fallback rows are parser-owned projection state keyed by
			// (session_id, dedup_key), so they must clear with the rest of the projection.
			// resetSessionAggregates zeroes sessions.model_fallback_count below, so leaving
			// the rows would both diverge count(model_fallbacks) from the rollup and let a
			// re-upload of the same raw merge into the stale rows instead of inserting, so the
			// re-count never fires and the rollup stays 0.
			"DELETE FROM model_fallbacks WHERE session_id = $1",
			"DELETE FROM session_events WHERE session_id = $1",
			// Derived signals clear with the projection they summarize; the rebuild
			// that follows the re-uploaded bytes re-grades the session.
			"DELETE FROM session_signals WHERE session_id = $1",
			"DELETE FROM session_raw_chunks WHERE session_id = $1",
		} {
			if _, err := tx.Exec(ctx, q, sessionID); err != nil {
				return fmt.Errorf("reset session %d (%s): %w", sessionID, q, err)
			}
		}
		// The insights rollups summarize the projection rows just deleted, so they
		// clear with them; the rebuild the epoch reset below forces re-derives them
		// from whatever bytes arrive next.
		if err := clearSessionRollupsTx(ctx, tx, sessionID); err != nil {
			return err
		}
		// parser_epoch 0 sits behind every real epoch, so the session reads as due
		// and the worker rebuilds (to an empty projection if no bytes ever arrive,
		// re-stamping the epoch either way). The failure markers and any
		// operational backoff clear with it: the reset changed the situation, so
		// the next attempt is immediate.
		_, err := tx.Exec(ctx,
			`UPDATE session_raw
			    SET byte_len = 0, content_sha256 = $2, sha256_state = NULL,
			        parsed_byte_len = 0, parser_epoch = 0,
			        parse_error = '', parse_error_epoch = 0, parse_error_byte_len = 0,
			        parse_retry_at = NULL, parse_retry_backoff_secs = 0
			  WHERE session_id = $1`, sessionID, emptySHA256)
		if err != nil {
			return fmt.Errorf("reset raw cursor for session %d: %w", sessionID, err)
		}
		// Zero the rollups and transcript-owned identity so the session reads as empty
		// until the rebuild refills them. parent_source_id stays intact: announce owns
		// it for Claude, and a Codex rebuild overwrites any stale value after re-upload.
		// The projection moved, so signals_stale keeps the settle tick on the hook.
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET
			   message_count = 0, user_message_count = 0,
			   model_fallback_count = 0,
			   total_input_tokens = 0, total_output_tokens = 0,
			   total_cache_write_tokens = 0, total_cache_read_tokens = 0,
			   total_cost_usd = 0, total_cache_savings_usd = 0,
			   started_at = NULL, ended_at = NULL,
			   custom_title = '', slug = '', permission_mode = '',
			   reasoning_effort = '', subagent_name = '',
			   pr_number = 0, pr_url = '', pr_repo = '',
			   updated_at = now(),
			   signals_stale = true
			 WHERE id = $1`, sessionID); err != nil {
			return fmt.Errorf("reset aggregates for session %d: %w", sessionID, err)
		}
		return nil
	})
}
