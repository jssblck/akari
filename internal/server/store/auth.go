package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// User is an akari account.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	IsAdmin      bool
	CreatedAt    time.Time
}

// APIToken is a stored client token (the secret itself is never stored, only its
// hash).
type APIToken struct {
	ID         int64
	UserID     int64
	Name       string
	Scope      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Register creates a user. The first account ever created becomes admin and
// needs no invite; every later account must present an unredeemed, unexpired
// invite token (by its hash), which is redeemed atomically with the insert.
func (s *Store) Register(ctx context.Context, username, passwordHash, inviteHash string) (User, error) {
	var u User
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Serialize registration so the first-user-is-admin decision and invite
		// redemption cannot race.
		if _, err := tx.Exec(ctx, "LOCK TABLE users IN EXCLUSIVE MODE"); err != nil {
			return err
		}

		var hasUsers bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM users)").Scan(&hasUsers); err != nil {
			return err
		}
		isAdmin := !hasUsers

		if hasUsers {
			// Redeem the invite first; the redeemed_by is patched after insert.
			var inviteID int64
			err := tx.QueryRow(ctx,
				`UPDATE invite_tokens
				   SET redeemed_at = now()
				 WHERE token_hash = $1
				   AND redeemed_at IS NULL
				   AND (expires_at IS NULL OR expires_at > now())
				 RETURNING id`, inviteHash).Scan(&inviteID)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrInvalidInvite
			} else if err != nil {
				return err
			}

			if err := tx.QueryRow(ctx,
				`INSERT INTO users (username, password_hash, is_admin)
				 VALUES ($1, $2, FALSE) RETURNING id, username, password_hash, is_admin, created_at`,
				username, passwordHash).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.CreatedAt); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, "UPDATE invite_tokens SET redeemed_by = $1 WHERE id = $2", u.ID, inviteID)
			return err
		}

		return tx.QueryRow(ctx,
			`INSERT INTO users (username, password_hash, is_admin)
			 VALUES ($1, $2, $3) RETURNING id, username, password_hash, is_admin, created_at`,
			username, passwordHash, isAdmin).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.CreatedAt)
	})
	return u, err
}

// UserByUsername looks up a user by username.
func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	var u User
	err := s.Pool.QueryRow(ctx,
		`SELECT id, username, password_hash, is_admin, created_at FROM users WHERE username = $1`,
		username).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// UserByID looks up a user by id.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	var u User
	err := s.Pool.QueryRow(ctx,
		`SELECT id, username, password_hash, is_admin, created_at FROM users WHERE id = $1`,
		id).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// ListUsers returns every account, id and username only, ordered by username, to
// populate the overview's per-user activity filter. The password hash is left
// zero: this list names identities for a scope control, it does not carry the
// credential.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, username FROM users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("query users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return out, nil
}

// CreateAPIToken stores a token's hash with a scope and returns its row id.
func (s *Store) CreateAPIToken(ctx context.Context, userID int64, name, scope, tokenHash string) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx,
		`INSERT INTO api_tokens (user_id, name, scope, token_hash)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		userID, name, scope, tokenHash).Scan(&id)
	return id, err
}

// TokenAuth resolves a presented token hash to its owner and scope, rejecting
// revoked tokens, and stamps last_used_at.
func (s *Store) TokenAuth(ctx context.Context, tokenHash string) (userID int64, scope string, err error) {
	err = s.Pool.QueryRow(ctx,
		`UPDATE api_tokens SET last_used_at = now()
		 WHERE token_hash = $1 AND revoked_at IS NULL
		 RETURNING user_id, scope`, tokenHash).Scan(&userID, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return userID, scope, err
}

// ListAPITokens returns a user's tokens, newest first.
func (s *Store) ListAPITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT id, user_id, name, scope, created_at, last_used_at, revoked_at
		   FROM api_tokens WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Scope, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken marks a user's token revoked. It is a no-op if the token does
// not belong to the user.
func (s *Store) RevokeAPIToken(ctx context.Context, userID, tokenID int64) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE api_tokens SET revoked_at = now()
		 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, tokenID, userID)
	return err
}

// CreateWebSession persists a browser session.
func (s *Store) CreateWebSession(ctx context.Context, id string, userID int64, expiresAt time.Time) error {
	_, err := s.Pool.Exec(ctx,
		`INSERT INTO web_sessions (id, user_id, expires_at) VALUES ($1, $2, $3)`,
		id, userID, expiresAt)
	return err
}

// WebSession resolves a session cookie id to its user, rejecting expired ones.
func (s *Store) WebSession(ctx context.Context, id string) (userID int64, err error) {
	err = s.Pool.QueryRow(ctx,
		`SELECT user_id FROM web_sessions WHERE id = $1 AND expires_at > now()`, id).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return userID, err
}

// DeleteWebSession removes a browser session (logout).
func (s *Store) DeleteWebSession(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx, "DELETE FROM web_sessions WHERE id = $1", id)
	return err
}

// CreateInvite stores an invite token hash issued by an admin.
func (s *Store) CreateInvite(ctx context.Context, tokenHash string, createdBy int64, note string, expiresAt *time.Time) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx,
		`INSERT INTO invite_tokens (token_hash, created_by, note, expires_at)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		tokenHash, createdBy, note, expiresAt).Scan(&id)
	return id, err
}
