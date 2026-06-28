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
	var id int64
	err := s.Pool.QueryRow(ctx,
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
		if p.Kind != "" && p.Kind != "remote" {
			var existingID int64
			var existingKind string
			err := tx.QueryRow(ctx,
				`SELECT s.id, pr.kind
				   FROM sessions s JOIN projects pr ON pr.id = s.project_id
				  WHERE s.user_id = $1 AND s.agent = $2 AND s.source_session_id = $3`,
				p.UserID, p.Agent, p.SourceSessionID).Scan(&existingID, &existingKind)
			switch {
			case err == nil && existingKind == "remote":
				// Keep the remote attribution untouched; report the current cursor.
				r.SessionID = existingID
				if _, err := tx.Exec(ctx,
					`INSERT INTO session_raw (session_id) VALUES ($1) ON CONFLICT DO NOTHING`, existingID); err != nil {
					return err
				}
				return tx.QueryRow(ctx,
					`SELECT byte_len, content_sha256 FROM session_raw WHERE session_id = $1`, existingID).
					Scan(&r.StoredBytes, &r.PrefixSHA256)
			case err != nil && !errors.Is(err, pgx.ErrNoRows):
				return err
			}
		}
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
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO session_raw (session_id) VALUES ($1) ON CONFLICT DO NOTHING`, r.SessionID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT byte_len, content_sha256 FROM session_raw WHERE session_id = $1`, r.SessionID).
			Scan(&r.StoredBytes, &r.PrefixSHA256)
	})
	if err == nil && r.StoredBytes == 0 {
		r.PrefixSHA256 = emptySHA256
	}
	return r, err
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
			return err
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
			return err
		}
		newStoredBytes = current + int64(len(data))

		if _, err := tx.Exec(ctx,
			`INSERT INTO session_raw_chunks (session_id, byte_offset, byte_len, content)
			 VALUES ($1, $2, $3, $4)`,
			sessionID, offset, int64(len(data)), data); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`UPDATE session_raw
			    SET byte_len = $2, content_sha256 = $3, sha256_state = $4
			  WHERE session_id = $1`,
			sessionID, newStoredBytes, hex.EncodeToString(h.Sum(nil)), newState)
		return err
	})
	return newStoredBytes, err
}

// ResetRaw clears a session's raw store and its derived rows so the next chunk
// re-parses from zero. Dropping the tool_calls and attachments can orphan CAS
// blobs; like any deletion or re-parse, those are reclaimed by a later
// SweepBlobs rather than synchronously here, so a client reset stays cheap.
func (s *Store) ResetRaw(ctx context.Context, sessionID int64) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for _, q := range []string{
			"DELETE FROM messages WHERE session_id = $1",
			"DELETE FROM tool_calls WHERE session_id = $1",
			"DELETE FROM usage_events WHERE session_id = $1",
			"DELETE FROM attachments WHERE session_id = $1",
			"DELETE FROM session_raw_chunks WHERE session_id = $1",
		} {
			if _, err := tx.Exec(ctx, q, sessionID); err != nil {
				return err
			}
		}
		ct, err := tx.Exec(ctx,
			`UPDATE session_raw
			    SET byte_len = 0, content_sha256 = $2, sha256_state = NULL,
			        parsed_byte_len = 0, parse_state = '{}'::jsonb,
			        parse_state_version = 0, parse_error = ''
			  WHERE session_id = $1`, sessionID, emptySHA256)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return ErrNotFound
		}
		return resetSessionAggregates(ctx, tx, sessionID)
	})
}
