package store_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// seedUser is defined in analytics_test.go: it inserts an account directly and
// returns its id, which these tests reuse to own grants and tokens.

func TestSessionFeedKeysetPaging(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	uid := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// Seven sessions, distinct recency so the feed order is total.
	for i := 0; i < 7; i++ {
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine, updated_at)
			 VALUES ($1,$2,'claude',$3,'box', now() - make_interval(mins => $4))`,
			uid, pid, "s"+strconv.Itoa(i), i); err != nil {
			t.Fatalf("insert session %d: %v", i, err)
		}
	}

	var rows []store.SessionRow
	seen := map[int64]bool{}
	var cursor *store.SessionFeedCursor
	for page := 0; page < 10; page++ {
		batch, next, err := st.SessionFeed(ctx, store.SessionFilter{}, 3, cursor)
		if err != nil {
			t.Fatalf("feed page %d: %v", page, err)
		}
		for _, r := range batch {
			if seen[r.ID] {
				t.Fatalf("session %d returned on two pages", r.ID)
			}
			seen[r.ID] = true
			rows = append(rows, r)
		}
		if next == nil {
			break
		}
		cursor = next
	}
	if len(rows) != 7 {
		t.Fatalf("feed returned %d sessions, want 7", len(rows))
	}
	// Newest active first: updated_at never increases across the concatenated pages,
	// so keyset paging reproduces the single-query order without gaps or repeats.
	for i := 1; i < len(rows); i++ {
		if rows[i].UpdatedAt.After(*rows[i-1].UpdatedAt) {
			t.Fatalf("feed order broken at %d: %v after %v", i, rows[i].UpdatedAt, rows[i-1].UpdatedAt)
		}
	}
}

func TestOAuthClientRoundTrip(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uris := []string{"http://127.0.0.1:7777/callback", "https://app.example/cb"}
	if err := st.CreateOAuthClient(ctx, "client-abc", "Grace's agent", uris); err != nil {
		t.Fatalf("create client: %v", err)
	}
	got, err := st.OAuthClient(ctx, "client-abc")
	if err != nil {
		t.Fatalf("load client: %v", err)
	}
	if got.ClientName != "Grace's agent" || len(got.RedirectURIs) != 2 || got.RedirectURIs[0] != uris[0] {
		t.Fatalf("client round-trip mismatch: %+v", got)
	}
	if _, err := st.OAuthClient(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown client: want ErrNotFound, got %v", err)
	}
}

func TestAuthCodeIsSingleUseAndExpires(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	uid := seedUser(t, st, "grace")
	if err := st.CreateOAuthClient(ctx, "c1", "agent", []string{"http://127.0.0.1/cb"}); err != nil {
		t.Fatalf("create client: %v", err)
	}

	ac := store.AuthCode{ClientID: "c1", UserID: uid, RedirectURI: "http://127.0.0.1/cb", CodeChallenge: "chal", Scope: "read", Resource: "http://x/mcp"}
	if err := st.CreateAuthCode(ctx, "code-hash-1", ac, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("create code: %v", err)
	}

	got, err := st.ConsumeAuthCode(ctx, "code-hash-1")
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if got.UserID != uid || got.ClientID != "c1" || got.CodeChallenge != "chal" || got.Resource != "http://x/mcp" {
		t.Fatalf("consumed code mismatch: %+v", got)
	}
	// A second redemption of the same code must fail: that is the replay gate.
	if _, err := st.ConsumeAuthCode(ctx, "code-hash-1"); !errors.Is(err, store.ErrInvalidGrant) {
		t.Fatalf("replay: want ErrInvalidGrant, got %v", err)
	}

	// An already-expired code is never redeemable.
	if err := st.CreateAuthCode(ctx, "code-hash-2", ac, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("create expired code: %v", err)
	}
	if _, err := st.ConsumeAuthCode(ctx, "code-hash-2"); !errors.Is(err, store.ErrInvalidGrant) {
		t.Fatalf("expired: want ErrInvalidGrant, got %v", err)
	}
}

func TestOAuthAccessAuthRejectsExpiredAndRevoked(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	uid := seedUser(t, st, "grace")
	if err := st.CreateOAuthClient(ctx, "c1", "agent", []string{"http://127.0.0.1/cb"}); err != nil {
		t.Fatalf("create client: %v", err)
	}

	refresh := time.Now().Add(time.Hour)
	mk := func(accessHash string, accessExpiry time.Time) {
		if err := st.CreateOAuthToken(ctx, store.OAuthTokenParams{
			AccessHash: accessHash, RefreshHash: accessHash + "-r", ClientID: "c1", UserID: uid,
			Scope: "read", Resource: "http://x/mcp", AccessExpiresAt: accessExpiry, RefreshExpiresAt: &refresh,
		}); err != nil {
			t.Fatalf("create token %s: %v", accessHash, err)
		}
	}

	mk("live", time.Now().Add(time.Hour))
	gotUID, scope, expiresAt, err := st.OAuthAccessAuth(ctx, "live")
	if err != nil || gotUID != uid || scope != "read" || expiresAt.Before(time.Now()) {
		t.Fatalf("live token: uid=%d scope=%q exp=%v err=%v", gotUID, scope, expiresAt, err)
	}

	mk("stale", time.Now().Add(-time.Minute))
	if _, _, _, err := st.OAuthAccessAuth(ctx, "stale"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired access: want ErrNotFound, got %v", err)
	}

	// Revoking the grant kills the live token.
	if err := st.RevokeOAuthGrant(ctx, uid, "c1"); err != nil {
		t.Fatalf("revoke grant: %v", err)
	}
	if _, _, _, err := st.OAuthAccessAuth(ctx, "live"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked access: want ErrNotFound, got %v", err)
	}
}

func TestRotateRefreshTokenIsSingleUse(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	uid := seedUser(t, st, "grace")
	if err := st.CreateOAuthClient(ctx, "c1", "agent", []string{"http://127.0.0.1/cb"}); err != nil {
		t.Fatalf("create client: %v", err)
	}
	refresh := time.Now().Add(time.Hour)
	if err := st.CreateOAuthToken(ctx, store.OAuthTokenParams{
		AccessHash: "a0", RefreshHash: "r0", ClientID: "c1", UserID: uid,
		Scope: "read", Resource: "http://x/mcp", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: &refresh,
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}

	newRefresh := time.Now().Add(time.Hour)
	clientID, gotUID, scope, resource, err := st.RotateOAuthToken(ctx, "r0", store.OAuthTokenParams{
		AccessHash: "a1", RefreshHash: "r1", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: &newRefresh,
	})
	if err != nil || clientID != "c1" || gotUID != uid || scope != "read" || resource != "http://x/mcp" {
		t.Fatalf("rotate: client=%q uid=%d scope=%q resource=%q err=%v", clientID, gotUID, scope, resource, err)
	}
	// The old refresh token no longer works (single-use rotation).
	if _, _, _, _, err := st.RotateOAuthToken(ctx, "r0", store.OAuthTokenParams{AccessHash: "a2", RefreshHash: "r2", AccessExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, store.ErrInvalidGrant) {
		t.Fatalf("reused refresh: want ErrInvalidGrant, got %v", err)
	}
	// The rotated access token authenticates; the old one is gone.
	if _, _, _, err := st.OAuthAccessAuth(ctx, "a1"); err != nil {
		t.Fatalf("rotated access should authenticate: %v", err)
	}
	if _, _, _, err := st.OAuthAccessAuth(ctx, "a0"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old access after rotation: want ErrNotFound, got %v", err)
	}
}

func TestListOAuthGrants(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	uid := seedUser(t, st, "grace")
	if err := st.CreateOAuthClient(ctx, "c1", "Ada's agent", []string{"http://127.0.0.1/cb"}); err != nil {
		t.Fatalf("create client: %v", err)
	}
	refresh := time.Now().Add(time.Hour)
	if err := st.CreateOAuthToken(ctx, store.OAuthTokenParams{
		AccessHash: "a0", RefreshHash: "r0", ClientID: "c1", UserID: uid,
		Scope: "read", AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: &refresh,
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}

	grants, err := st.ListOAuthGrants(ctx, uid)
	if err != nil || len(grants) != 1 || grants[0].ClientID != "c1" || grants[0].ClientName != "Ada's agent" || grants[0].Scope != "read" {
		t.Fatalf("grants: %+v err=%v", grants, err)
	}

	if err := st.RevokeOAuthGrant(ctx, uid, "c1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	grants, err = st.ListOAuthGrants(ctx, uid)
	if err != nil || len(grants) != 0 {
		t.Fatalf("after revoke want empty, got %+v err=%v", grants, err)
	}
}
