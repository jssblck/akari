package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/auth"
)

// TestAccountPageAdminZone checks the account page's reordering and admin
// gating end to end: a non-admin sees the daily-use sections but not the
// Admin divider, Invites, or Reparse, while an admin sees all of it including
// an outstanding invite in the table.
func TestAccountPageAdminZone(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	admin := newClient(t)

	// Register the first account (becomes admin, no invite needed).
	if _, err := admin.PostForm(srv.URL+"/register", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("register grace: %v", err)
	}

	// Issue an invite as the admin, then register a second, non-admin account
	// with it.
	token, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	graceUser, err := st.UserByUsername(ctx, "grace")
	if err != nil {
		t.Fatalf("lookup grace: %v", err)
	}
	if _, err := st.CreateInvite(ctx, auth.HashToken(token), graceUser.ID, "for ada", nil); err != nil {
		t.Fatalf("create invite: %v", err)
	}

	member := newClient(t)
	if _, err := member.PostForm(srv.URL+"/register", url.Values{
		"username": {"ada"}, "password": {"lovelace-1843"}, "invite_token": {token},
	}); err != nil {
		t.Fatalf("register ada: %v", err)
	}

	// A second, still-open invite so the table also covers the unused status
	// (the first one is redeemed the moment ada registers with it above).
	spareToken, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if _, err := st.CreateInvite(ctx, auth.HashToken(spareToken), graceUser.ID, "for anna", nil); err != nil {
		t.Fatalf("create spare invite: %v", err)
	}

	// The non-admin sees the daily-use sections in the new order, but neither
	// the Admin divider nor its contents.
	body := readBody(t, mustGet(t, member, srv.URL+"/account"))
	for _, want := range []string{"API tokens", "Connected apps", "Publicity"} {
		if !strings.Contains(body, want) {
			t.Fatalf("non-admin account page missing %q, got:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"Admin", "Invites", "Reparse", "Create invite", "Reparse now"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("non-admin account page should not show %q, got:\n%s", unwanted, body)
		}
	}

	// API tokens leads the page, ahead of Publicity, matching the reordered
	// daily-use-first layout.
	if strings.Index(body, "API tokens") > strings.Index(body, "Publicity") {
		t.Fatalf("API tokens should render before Publicity, got:\n%s", body)
	}

	// The admin sees the Admin divider, the Invites table with both the
	// redeemed and the still-open invite, and Reparse, all trailing the
	// daily-use sections.
	adminBody := readBody(t, mustGet(t, admin, srv.URL+"/account"))
	for _, want := range []string{"Admin", "Invites", "Reparse", "for ada", "redeemed by ada", "for anna", "unused"} {
		if !strings.Contains(adminBody, want) {
			t.Fatalf("admin account page missing %q, got:\n%s", want, adminBody)
		}
	}
	if strings.Index(adminBody, "Publicity") > strings.Index(adminBody, "Admin") {
		t.Fatalf("Admin zone should trail Publicity, got:\n%s", adminBody)
	}

	// The de-noised copy lands verbatim.
	for _, want := range []string{
		"Rebuilds every parsed session from raw bytes.",
		"Publishes your usage overview at /u/grace. Sessions stay private.",
		"Read-only MCP clients connected as you.",
		"None connected.",
	} {
		if !strings.Contains(adminBody, want) {
			t.Fatalf("account page missing de-noised copy %q, got:\n%s", want, adminBody)
		}
	}
}

// TestRevokeInviteAuthz checks that revoking an invite is admin-only (a
// non-admin gets a JSON 403 from requireAdmin, not a silent redirect) and that
// an admin's revoke removes the invite and redirects back to /account.
func TestRevokeInviteAuthz(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	admin := newClient(t)

	if _, err := admin.PostForm(srv.URL+"/register", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("register grace: %v", err)
	}
	graceUser, err := st.UserByUsername(ctx, "grace")
	if err != nil {
		t.Fatalf("lookup grace: %v", err)
	}

	inviteToken, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	id, err := st.CreateInvite(ctx, auth.HashToken(inviteToken), graceUser.ID, "for ada", nil)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}

	memberToken, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if _, err := st.CreateInvite(ctx, auth.HashToken(memberToken), graceUser.ID, "for member", nil); err != nil {
		t.Fatalf("create second invite: %v", err)
	}
	member := newClient(t)
	if _, err := member.PostForm(srv.URL+"/register", url.Values{
		"username": {"ada"}, "password": {"lovelace-1843"}, "invite_token": {memberToken},
	}); err != nil {
		t.Fatalf("register ada: %v", err)
	}

	// A non-admin's revoke attempt is rejected before it reaches the store: the
	// invite must still be listed afterward.
	revokeURL := srv.URL + "/account/invites/" + strconv.FormatInt(id, 10) + "/revoke"
	resp, err := member.PostForm(revokeURL, url.Values{})
	if err != nil {
		t.Fatalf("non-admin revoke: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin revoke status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	invites, err := st.ListInvites(ctx)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	found := false
	for _, inv := range invites {
		if inv.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("invite should survive a non-admin's revoke attempt")
	}

	// The admin's revoke succeeds and redirects back to /account.
	admin.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err = admin.PostForm(revokeURL, url.Values{})
	if err != nil {
		t.Fatalf("admin revoke: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account" {
		t.Fatalf("admin revoke status=%d location=%q, want 303 to /account", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()

	invites, err = st.ListInvites(ctx)
	if err != nil {
		t.Fatalf("list invites after revoke: %v", err)
	}
	for _, inv := range invites {
		if inv.ID == id {
			t.Fatal("revoked invite should no longer be listed")
		}
	}
}
