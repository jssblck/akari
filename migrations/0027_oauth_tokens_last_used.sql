-- Real usage stamp for OAuth grants. The account page's "Last used" column was
-- derived from max(created_at), but a grant keeps one row per connection and
-- rewrites it in place on refresh, so created_at never advances and the column
-- was indistinguishable from "Connected". Track use directly, like
-- api_tokens.last_used_at: stamped when an access token authenticates a request
-- and when a refresh token is redeemed.
ALTER TABLE oauth_tokens ADD COLUMN last_used_at TIMESTAMPTZ NOT NULL DEFAULT now();
