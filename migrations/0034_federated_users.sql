-- Federated users: accounts whose identity is asserted by an external system
-- rather than a local password.
--
-- akari's first deployment model is a single instance with local usernames and
-- argon2id passwords. To run behind an organization's own identity (a sidecar
-- behind an authenticating reverse proxy, and later OIDC/SCIM), an account has to
-- be able to exist with no local password: the only way in is the external
-- assertion, never the /login form.
--
-- Two changes make that representable:
--   1. password_hash becomes nullable. A NULL hash is the marker for "no local
--      password"; the login path refuses such an account rather than ever calling
--      the verifier with an empty hash.
--   2. auth_source records how an account authenticates, so a federated identity
--      is distinguishable from a local one and the login path can fail closed. The
--      default keeps every existing row a local 'password' account, and the
--      first-user bootstrap admin stays a password account. 'proxy' is the trusted
--      reverse-proxy source added here; OIDC/SCIM will extend this CHECK when they
--      land (drop and re-add, the forward-only equivalent, as the api_tokens scope
--      constraint already does).

ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;

ALTER TABLE users ADD COLUMN auth_source TEXT NOT NULL DEFAULT 'password'
  CHECK (auth_source IN ('password', 'proxy'));

-- Keep the two representations of "does this account have a local password" from
-- drifting. password_hash NULLness is what the login path gates on (User.HasPassword),
-- and auth_source is what callers read to tell a local account from a federated one;
-- nothing should be able to store a 'password' account with no hash, or a federated
-- account that still carries one. The biconditional ties them: an account is a
-- 'password' account exactly when it has a hash. It generalizes to the federated
-- sources OIDC and SCIM will add (all passwordless): any non-'password' source must
-- have a NULL hash. Existing rows all satisfy it (they default to 'password' and kept
-- their NOT NULL hashes), so the constraint adds without a rewrite.
ALTER TABLE users ADD CONSTRAINT users_password_matches_source
  CHECK ((auth_source = 'password') = (password_hash IS NOT NULL));

-- NULL is the one representation of "no local password". The read path COALESCEs a
-- NULL hash to "" and User.HasPassword treats "" as no password, so an empty-string
-- hash would be a third, contradictory state: it satisfies the biconditional above
-- (it IS NOT NULL) yet HasPassword reads it as passwordless and the login path
-- refuses it. Forbid it, so password_hash is either NULL or a real hash and the DB
-- invariant matches the Go projection exactly.
ALTER TABLE users ADD CONSTRAINT users_password_hash_nonempty
  CHECK (password_hash IS NULL OR password_hash <> '');
