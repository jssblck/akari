package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AnnounceParams carries what a client reports when announcing a session.
type AnnounceParams struct {
	UserID          int64
	Agent           string
	SourceSessionID string
	ProjectID       int64
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

// UpsertProject inserts the project keyed by its canonical remote, or refreshes
// last_seen on an existing one, returning the project id.
func (s *Store) UpsertProject(ctx context.Context, remoteKey, host, owner, repo, displayName string) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx,
		`INSERT INTO projects (remote_key, host, owner, repo, display_name)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (remote_key) DO UPDATE SET last_seen = now()
		 RETURNING id`,
		remoteKey, host, owner, repo, displayName).Scan(&id)
	return id, err
}

// Announce upserts the session row (latest announce wins for mutable metadata),
// ensures its raw-store row exists, and returns the current cursor and hash.
func (s *Store) Announce(ctx context.Context, p AnnounceParams) (AnnounceResult, error) {
	var r AnnounceResult
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
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

// SessionOwner returns the owning user of a session, or ErrNotFound.
func (s *Store) SessionOwner(ctx context.Context, sessionID int64) (int64, error) {
	var userID int64
	err := s.Pool.QueryRow(ctx, "SELECT user_id FROM sessions WHERE id = $1", sessionID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return userID, err
}

// AppendChunk appends data at the given offset. If offset does not match the
// server's current byte_len it returns OffsetMismatchError with the truth and
// makes no change. The stored content hash is advanced to cover the new bytes.
func (s *Store) AppendChunk(ctx context.Context, sessionID, offset int64, data []byte) (newStoredBytes int64, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var current int64
		if err := tx.QueryRow(ctx,
			`SELECT byte_len FROM session_raw WHERE session_id = $1 FOR UPDATE`, sessionID).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if current != offset {
			return OffsetMismatchError{StoredBytes: current}
		}
		// The SET expressions read the pre-update content, so byte_len and the
		// hash both fold in exactly the appended bytes once. Hashing in-database
		// keeps the large content off the wire. content is BYTEA, so the bytes
		// (and their hash) round-trip exactly.
		return tx.QueryRow(ctx,
			`UPDATE session_raw
			   SET content        = content || $2,
			       byte_len       = byte_len + $3,
			       content_sha256 = encode(sha256(content || $2), 'hex')
			 WHERE session_id = $1
			 RETURNING byte_len`,
			sessionID, data, int64(len(data))).Scan(&newStoredBytes)
	})
	return newStoredBytes, err
}

// ResetRaw clears a session's raw store and its derived rows so the next chunk
// re-parses from zero.
func (s *Store) ResetRaw(ctx context.Context, sessionID int64) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for _, q := range []string{
			"DELETE FROM messages WHERE session_id = $1",
			"DELETE FROM tool_calls WHERE session_id = $1",
			"DELETE FROM usage_events WHERE session_id = $1",
			"DELETE FROM attachments WHERE session_id = $1",
		} {
			if _, err := tx.Exec(ctx, q, sessionID); err != nil {
				return err
			}
		}
		ct, err := tx.Exec(ctx,
			`UPDATE session_raw SET content = '\x', byte_len = 0, content_sha256 = '' WHERE session_id = $1`,
			sessionID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}
