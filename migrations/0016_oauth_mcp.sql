-- OAuth 2.1 authorization server backing the remote MCP endpoint.
--
-- akari already is an identity provider: a browser session cookie names a user.
-- These tables let that same session mint scoped, audience-bound access tokens for
-- an MCP client (a coding agent) without the user ever typing a password into the
-- client. The client registers itself (RFC 7591), redirects the browser to the
-- authorize endpoint where the existing session is recognized, and exchanges the
-- resulting code (PKCE-protected) for an access token at the token endpoint.
--
-- Every secret is stored only as its sha256, the same discipline api_tokens,
-- web_sessions, and invite_tokens already follow: a database read never recovers a
-- usable credential.

-- A read-only scope for credentials that may see everything a logged-in user sees
-- but must not publish, delete, or mint further tokens. The MCP surface is
-- read-only, so its tokens carry this scope. The constraint is replaced rather than
-- extended in place because Postgres has no ALTER ... CHECK; drop and re-add is the
-- forward-only equivalent.
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_scope_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_scope_check
  CHECK (scope IN ('ingest', 'full', 'read'));

-- A dynamically registered OAuth client (a coding agent's MCP integration). Public
-- clients only: the flow is PKCE with no client secret, so there is nothing secret
-- to store here. redirect_uris is the allowlist the authorize and token endpoints
-- check a presented redirect against.
CREATE TABLE oauth_clients (
  id            TEXT PRIMARY KEY,            -- client_id, an opaque generated id
  client_name   TEXT NOT NULL DEFAULT '',
  redirect_uris TEXT[] NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A short-lived, single-use authorization code, bound to the client, the user who
-- approved it, the exact redirect it was issued for, the PKCE challenge the token
-- request must answer, and the resource (audience) it may be exchanged for. The
-- code itself is stored only as its hash; consumed_at makes redemption single-use.
CREATE TABLE oauth_auth_codes (
  code_hash      TEXT PRIMARY KEY,          -- sha256 of the issued code
  client_id      TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
  user_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  redirect_uri   TEXT NOT NULL,
  code_challenge TEXT NOT NULL,             -- PKCE S256 challenge (base64url)
  scope          TEXT NOT NULL,
  resource       TEXT NOT NULL DEFAULT '',  -- RFC 8707 audience the token is bound to
  expires_at     TIMESTAMPTZ NOT NULL,
  consumed_at    TIMESTAMPTZ
);
CREATE INDEX idx_oauth_auth_codes_client ON oauth_auth_codes(client_id);

-- An issued access token (with an optional refresh token), bound to its client and
-- user. Access tokens are short-lived and rotated via the refresh token; both are
-- stored only as hashes. revoked_at lets a user disconnect a client from the
-- account page, killing every token the grant holds at once.
CREATE TABLE oauth_tokens (
  id                 BIGSERIAL PRIMARY KEY,
  access_token_hash  TEXT NOT NULL UNIQUE,   -- sha256 of the bearer access token
  refresh_token_hash TEXT UNIQUE,            -- sha256 of the refresh token, if any
  client_id          TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
  user_id            BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  scope              TEXT NOT NULL,
  resource           TEXT NOT NULL DEFAULT '',
  access_expires_at  TIMESTAMPTZ NOT NULL,
  refresh_expires_at TIMESTAMPTZ,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at         TIMESTAMPTZ
);
CREATE INDEX idx_oauth_tokens_user ON oauth_tokens(user_id);
CREATE INDEX idx_oauth_tokens_client_user ON oauth_tokens(client_id, user_id);
