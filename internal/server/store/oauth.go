package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrInvalidGrant is returned when an authorization code or refresh token is
// unknown, already used, expired, or revoked. It maps to the OAuth
// "invalid_grant" error at the token endpoint.
var ErrInvalidGrant = errors.New("invalid or expired grant")

// OAuthClient is a dynamically registered MCP client (a coding agent). Clients are
// public: they authenticate with PKCE and hold no secret, so this is identity and
// the redirect allowlist only.
type OAuthClient struct {
	ID           string
	ClientName   string
	RedirectURIs []string
	CreatedAt    time.Time
}

// AuthCode is the data an authorization code carries from the authorize step to the
// token exchange: who approved it, for which client and redirect, the PKCE
// challenge the exchange must answer, and the scope and audience it grants.
type AuthCode struct {
	ClientID      string
	UserID        int64
	RedirectURI   string
	CodeChallenge string
	Scope         string
	Resource      string
}

// OAuthGrant summarizes a client a user has connected, for the account page's
// "connected apps" list. One row per client the user has live (unrevoked) tokens
// for, with when it was first connected and when it last authenticated a request
// or redeemed a refresh token.
type OAuthGrant struct {
	ClientID    string
	ClientName  string
	Scope       string
	ConnectedAt time.Time
	LastUsedAt  time.Time
}

// CreateOAuthClient stores a dynamic client registration and returns nothing but an
// error: the caller already holds the generated id.
func (s *Store) CreateOAuthClient(ctx context.Context, id, name string, redirectURIs []string) error {
	_, err := s.Pool.Exec(ctx,
		`INSERT INTO oauth_clients (id, client_name, redirect_uris) VALUES ($1, $2, $3)`,
		id, name, redirectURIs)
	return err
}

// OAuthClient looks up a registered client by id.
func (s *Store) OAuthClient(ctx context.Context, id string) (OAuthClient, error) {
	var c OAuthClient
	err := s.Pool.QueryRow(ctx,
		`SELECT id, client_name, redirect_uris, created_at FROM oauth_clients WHERE id = $1`, id).
		Scan(&c.ID, &c.ClientName, &c.RedirectURIs, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthClient{}, ErrNotFound
	}
	return c, err
}

// CreateAuthCode stores an authorization code's hash with everything the token
// exchange will need to validate and honor it.
func (s *Store) CreateAuthCode(ctx context.Context, codeHash string, c AuthCode, expiresAt time.Time) error {
	_, err := s.Pool.Exec(ctx,
		`INSERT INTO oauth_auth_codes
		   (code_hash, client_id, user_id, redirect_uri, code_challenge, scope, resource, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		codeHash, c.ClientID, c.UserID, c.RedirectURI, c.CodeChallenge, c.Scope, c.Resource, expiresAt)
	return err
}

// ConsumeAuthCode redeems an authorization code exactly once. It marks the code
// consumed and returns its bound data only if it was unconsumed and unexpired;
// otherwise it returns ErrInvalidGrant. The UPDATE ... RETURNING is the atomic
// single-use gate: a replayed code finds consumed_at already set and matches no
// row, so two token requests racing on the same code cannot both succeed.
func (s *Store) ConsumeAuthCode(ctx context.Context, codeHash string) (AuthCode, error) {
	var c AuthCode
	err := s.Pool.QueryRow(ctx,
		`UPDATE oauth_auth_codes SET consumed_at = now()
		   WHERE code_hash = $1 AND consumed_at IS NULL AND expires_at > now()
		 RETURNING client_id, user_id, redirect_uri, code_challenge, scope, resource`,
		codeHash).Scan(&c.ClientID, &c.UserID, &c.RedirectURI, &c.CodeChallenge, &c.Scope, &c.Resource)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthCode{}, ErrInvalidGrant
	}
	return c, err
}

// OAuthTokenParams is one issued token pair and its bindings.
type OAuthTokenParams struct {
	AccessHash       string
	RefreshHash      string // empty for no refresh token
	ClientID         string
	UserID           int64
	Scope            string
	Resource         string
	AccessExpiresAt  time.Time
	RefreshExpiresAt *time.Time
}

// CreateOAuthToken stores an issued access/refresh token pair.
func (s *Store) CreateOAuthToken(ctx context.Context, p OAuthTokenParams) error {
	var refresh *string
	if p.RefreshHash != "" {
		refresh = &p.RefreshHash
	}
	_, err := s.Pool.Exec(ctx,
		`INSERT INTO oauth_tokens
		   (access_token_hash, refresh_token_hash, client_id, user_id, scope, resource,
		    access_expires_at, refresh_expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		p.AccessHash, refresh, p.ClientID, p.UserID, p.Scope, p.Resource,
		p.AccessExpiresAt, p.RefreshExpiresAt)
	return err
}

// OAuthAccessAuth resolves a presented access-token hash to its owner, scope, and
// expiry, rejecting expired and revoked tokens. It is the MCP endpoint's bearer
// check; the expiry it returns lets the caller mirror the real token lifetime.
func (s *Store) OAuthAccessAuth(ctx context.Context, accessHash string) (userID int64, scope string, expiresAt time.Time, err error) {
	err = s.Pool.QueryRow(ctx,
		`UPDATE oauth_tokens SET last_used_at = now()
		  WHERE access_token_hash = $1 AND revoked_at IS NULL AND access_expires_at > now()
		  RETURNING user_id, scope, access_expires_at`,
		accessHash).Scan(&userID, &scope, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", time.Time{}, ErrNotFound
	}
	return userID, scope, expiresAt, err
}

// RotateOAuthToken redeems a refresh token for a fresh access/refresh pair. It
// rewrites the existing row in place (so the grant stays one row per connection)
// only if the presented refresh token is live, returning the bindings the new
// access token inherits. A refresh token that is unknown, revoked, or past its
// expiry matches no row and yields ErrInvalidGrant. Rotating the refresh hash on
// every use makes refresh tokens single-use, so a leaked-and-replayed refresh
// token is caught the next time the legitimate client refreshes.
func (s *Store) RotateOAuthToken(ctx context.Context, oldRefreshHash string, p OAuthTokenParams) (clientID string, userID int64, scope, resource string, err error) {
	var refresh *string
	if p.RefreshHash != "" {
		refresh = &p.RefreshHash
	}
	err = s.Pool.QueryRow(ctx,
		`UPDATE oauth_tokens
		    SET access_token_hash = $1, refresh_token_hash = $2,
		        access_expires_at = $3, refresh_expires_at = $4,
		        last_used_at = now()
		  WHERE refresh_token_hash = $5 AND revoked_at IS NULL
		    AND (refresh_expires_at IS NULL OR refresh_expires_at > now())
		  RETURNING client_id, user_id, scope, resource`,
		p.AccessHash, refresh, p.AccessExpiresAt, p.RefreshExpiresAt, oldRefreshHash).
		Scan(&clientID, &userID, &scope, &resource)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, "", "", ErrInvalidGrant
	}
	return clientID, userID, scope, resource, err
}

// ListOAuthGrants returns the clients a user has live tokens for, one row per
// client, newest connection first. It backs the account page's "connected apps"
// list. Revoked and fully expired tokens are excluded, so a disconnected client
// drops off the list.
func (s *Store) ListOAuthGrants(ctx context.Context, userID int64) ([]OAuthGrant, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT c.id, c.client_name, min(t.scope), min(t.created_at), max(t.last_used_at)
		   FROM oauth_tokens t
		   JOIN oauth_clients c ON c.id = t.client_id
		  WHERE t.user_id = $1 AND t.revoked_at IS NULL
		    AND (t.refresh_expires_at IS NULL OR t.refresh_expires_at > now() OR t.access_expires_at > now())
		  GROUP BY c.id, c.client_name
		  ORDER BY max(t.last_used_at) DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OAuthGrant
	for rows.Next() {
		var g OAuthGrant
		if err := rows.Scan(&g.ClientID, &g.ClientName, &g.Scope, &g.ConnectedAt, &g.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeOAuthGrant revokes every live token a user holds for one client,
// disconnecting it. It is scoped to the user, so a request cannot revoke another
// account's grant.
func (s *Store) RevokeOAuthGrant(ctx context.Context, userID int64, clientID string) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE oauth_tokens SET revoked_at = now()
		   WHERE user_id = $1 AND client_id = $2 AND revoked_at IS NULL`, userID, clientID)
	return err
}
