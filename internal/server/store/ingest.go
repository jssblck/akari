package store

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"

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
func lockAnnounceIdentityTx(ctx context.Context, tx pgx.Tx, p AnnounceParams) error {
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

func keepRemoteAttributionTx(ctx context.Context, tx pgx.Tx, p AnnounceParams) (AnnounceResult, bool, error) {
	var r AnnounceResult
	if p.Kind == "" || p.Kind == "remote" {
		return r, false, nil
	}
	var existingID int64
	var existingKind string
	err := tx.QueryRow(ctx,
		`SELECT s.id, pr.kind
		   FROM sessions s JOIN projects pr ON pr.id = s.project_id
		  WHERE s.user_id = $1 AND s.agent = $2 AND s.source_session_id = $3`,
		p.UserID, p.Agent, p.SourceSessionID).Scan(&existingID, &existingKind)
	switch {
	case err == nil && existingKind == "remote":
		r.SessionID = existingID
		if _, err := tx.Exec(ctx,
			`INSERT INTO session_raw (session_id) VALUES ($1) ON CONFLICT DO NOTHING`, existingID); err != nil {
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
	if err := tx.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine, cwd, git_branch)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (user_id, agent, source_session_id) DO UPDATE
		   SET project_id = EXCLUDED.project_id,
		       machine    = EXCLUDED.machine,
		       cwd        = EXCLUDED.cwd,
		       git_branch = EXCLUDED.git_branch,
		       updated_at = now()
		 RETURNING id`,
		p.UserID, p.ProjectID, p.Agent, p.SourceSessionID, p.Machine, p.Cwd, p.GitBranch).Scan(&r.SessionID); err != nil {
		return AnnounceResult{}, fmt.Errorf("upsert session for announce: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO session_raw (session_id) VALUES ($1) ON CONFLICT DO NOTHING`, r.SessionID); err != nil {
		return AnnounceResult{}, err
	}
	return r, tx.QueryRow(ctx,
		`SELECT byte_len, content_sha256 FROM session_raw WHERE session_id = $1`, r.SessionID).
		Scan(&r.StoredBytes, &r.PrefixSHA256)
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
		if _, err := tx.Exec(ctx,
			`UPDATE session_raw
			    SET byte_len = $2, content_sha256 = $3, sha256_state = $4
			  WHERE session_id = $1`,
			sessionID, newStoredBytes, hex.EncodeToString(h.Sum(nil)), newState); err != nil {
			return fmt.Errorf("advance raw cursor and hash for session %d: %w", sessionID, err)
		}
		return nil
	})
	return newStoredBytes, err
}

// ResetRaw clears a session's raw store and its derived rows so the next chunk
// re-parses from zero. Dropping the tool_calls and attachments can orphan CAS
// blobs; like any deletion or re-parse, those are reclaimed by a later
// SweepBlobs rather than synchronously here, so a client reset stays cheap.
//
// It takes the parent session row lock and then the session_raw lock, the locks
// AppendChunk and AdvanceProjection serialize on, so a reset cannot interleave
// with an in-flight append or parse and leave behind a chunk row or projection
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
			"DELETE FROM attachments WHERE session_id = $1",
			// Derived signals clear with the projection they summarize; the next chunk's
			// catch-up rebuilds the row from the freshly re-parsed messages and tool calls.
			"DELETE FROM session_signals WHERE session_id = $1",
			"DELETE FROM session_raw_chunks WHERE session_id = $1",
		} {
			if _, err := tx.Exec(ctx, q, sessionID); err != nil {
				return fmt.Errorf("reset session %d (%s): %w", sessionID, q, err)
			}
		}
		_, err := tx.Exec(ctx,
			`UPDATE session_raw
			    SET byte_len = 0, content_sha256 = $2, sha256_state = NULL,
			        parsed_byte_len = 0, parse_state = '{}'::jsonb,
			        parse_state_version = 0, parse_error = ''
			  WHERE session_id = $1`, sessionID, emptySHA256)
		if err != nil {
			return fmt.Errorf("reset raw cursor for session %d: %w", sessionID, err)
		}
		return resetSessionAggregates(ctx, tx, sessionID)
	})
}
