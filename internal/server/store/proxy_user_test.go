package store_test

import (
	"context"
	"testing"

	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestUpsertProxyUserProvisions covers the just-in-time provisioning behind
// proxy-header auth: the first sight of a username mints a federated account (no
// password, source "proxy", not admin), and a second sight resolves the same
// account rather than a duplicate.
func TestUpsertProxyUserProvisions(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.UpsertProxyUser(ctx, "ada")
	if err != nil {
		t.Fatalf("provision proxy user: %v", err)
	}
	if u.HasPassword() {
		t.Fatal("proxy user should have no local password")
	}
	if u.AuthSource != "proxy" {
		t.Fatalf("auth_source = %q, want %q", u.AuthSource, "proxy")
	}
	if u.IsAdmin {
		t.Fatal("proxy user must not be admin")
	}

	again, err := st.UpsertProxyUser(ctx, "ada")
	if err != nil {
		t.Fatalf("re-resolve proxy user: %v", err)
	}
	if again.ID != u.ID {
		t.Fatalf("second upsert minted a new account: id %d then %d", u.ID, again.ID)
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("expected exactly one account, got %d", len(users))
	}
}

// TestUpsertProxyUserAdoptsLocalAccount confirms that a proxy assertion for a
// username that already exists as a local password account resolves that account
// without stripping its password or flipping its source. In the proxy deployment
// the proxy is the identity authority, so asserting an existing name is that user;
// the row must survive intact (including admin and its password), so the same
// person can still reach the account both ways during a migration.
func TestUpsertProxyUserAdoptsLocalAccount(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	// The first registered account is the bootstrap admin, with a local password.
	local, err := st.Register(ctx, "grace", mustHashPW(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register local admin: %v", err)
	}
	// Register's returned row must carry the same auth_source a later lookup does,
	// so a caller does not see a different source than UserByUsername for one row.
	if local.AuthSource != "password" {
		t.Fatalf("Register returned auth_source %q, want %q", local.AuthSource, "password")
	}

	adopted, err := st.UpsertProxyUser(ctx, "grace")
	if err != nil {
		t.Fatalf("adopt local account via proxy: %v", err)
	}
	if adopted.ID != local.ID {
		t.Fatalf("proxy assertion minted a new account (id %d) instead of adopting %d", adopted.ID, local.ID)
	}
	if !adopted.IsAdmin {
		t.Fatal("adopting the local admin dropped its admin bit")
	}
	if !adopted.HasPassword() || adopted.AuthSource != "password" {
		t.Fatalf("adoption altered the local account: hasPassword=%v source=%q", adopted.HasPassword(), adopted.AuthSource)
	}
}

// TestAuthSourcePasswordInvariant confirms the cross-column CHECK from migration
// 0034 rejects the two contradictory states directly: a 'password' account with no
// hash, and a federated ('proxy') account that still carries one. The invariant is
// what lets the login path gate on password_hash NULLness while callers read
// auth_source, without the two disagreeing for a row.
func TestAuthSourcePasswordInvariant(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	// A 'password' account must have a hash.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO users (username, password_hash, auth_source) VALUES ('bad1', NULL, 'password')`); err == nil {
		t.Fatal("expected a CHECK violation for a passwordless 'password' account")
	}
	// A federated account must not carry a hash.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO users (username, password_hash, auth_source) VALUES ('bad2', 'some-hash', 'proxy')`); err == nil {
		t.Fatal("expected a CHECK violation for a 'proxy' account carrying a password hash")
	}
	// An empty-string hash is not a valid "has password" state: NULL is the only
	// representation of no local password, so the Go projection (HasPassword reads
	// "" as no password) and the DB invariant cannot disagree.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO users (username, password_hash, auth_source) VALUES ('bad3', '', 'password')`); err == nil {
		t.Fatal("expected a CHECK violation for an empty-string password hash")
	}
}

// mustHashPW hashes a password for seeding a local account directly through the
// store.
func mustHashPW(t *testing.T, pw string) string {
	t.Helper()
	h, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	return h
}
