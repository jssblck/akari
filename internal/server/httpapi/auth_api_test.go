package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// postJSON sends body as a JSON POST and decodes the JSON response into a map,
// returning it with the status code. A nil map means the body was not JSON.
func postJSON(t *testing.T, c *http.Client, url string, body string) (int, map[string]any) {
	t.Helper()
	resp, err := c.Post(url, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestAuthAPIFlow drives the JSON auth and token endpoints end to end: register
// the first (admin) account, log out and back in, mint and revoke API tokens,
// and mint an invite that lets a second account register. The HTML form flow has
// its own coverage; this exercises the JSON envelopes, status codes, and the
// validation branches the forms do not share (strict body decoding, the invalid
// scope rejection).
func TestAuthAPIFlow(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	c := newClient(t)

	// The first account registers open (bootstrap) and is the admin.
	status, got := postJSON(t, c, srv.URL+"/api/v1/auth/register", `{"username":"grace","password":"pw-grace"}`)
	if status != http.StatusCreated || got["username"] != "grace" || got["is_admin"] != true {
		t.Fatalf("register first account: status=%d body=%v", status, got)
	}

	// Registration validation: missing fields, malformed JSON, unknown fields, and
	// trailing data are all 400s; a duplicate username is a 409.
	for name, body := range map[string]string{
		"missing password": `{"username":"ada"}`,
		"malformed":        `{"username":`,
		"unknown field":    `{"username":"ada","password":"x","admin":true}`,
		"trailing data":    `{"username":"ada","password":"x"}{"again":1}`,
	} {
		anon := newClient(t)
		if status, _ := postJSON(t, anon, srv.URL+"/api/v1/auth/register", body); status != http.StatusBadRequest {
			t.Errorf("register %s: status=%d, want 400", name, status)
		}
	}

	// A second account cannot register without an invite once a user exists.
	anon := newClient(t)
	if status, _ := postJSON(t, anon, srv.URL+"/api/v1/auth/register", `{"username":"ada","password":"pw-ada"}`); status != http.StatusForbidden {
		t.Fatalf("uninvited second register: status=%d, want 403", status)
	}

	// Logout clears the session; the token list (full scope) then rejects.
	if status, _ := postJSON(t, c, srv.URL+"/api/v1/auth/logout", `{}`); status != http.StatusOK {
		t.Fatalf("logout: status=%d", status)
	}
	resp := mustGet(t, c, srv.URL+"/api/v1/tokens")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token list after logout: status=%d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Login: a wrong password is a 401 with no hint which part was wrong.
	if status, got := postJSON(t, c, srv.URL+"/api/v1/auth/login", `{"username":"grace","password":"wrong"}`); status != http.StatusUnauthorized || got["error"] != "invalid credentials" {
		t.Fatalf("bad login: status=%d body=%v", status, got)
	}
	if status, _ := postJSON(t, c, srv.URL+"/api/v1/auth/login", `{"username":"grace","password":"pw-grace"}`); status != http.StatusOK {
		t.Fatalf("login: status=%d", status)
	}

	// Token minting: scope defaults to ingest, an unknown scope is rejected (the
	// form flow silently defaults instead, so only this path covers the branch),
	// and a blank name is rejected.
	status, got = postJSON(t, c, srv.URL+"/api/v1/tokens", `{"name":"laptop"}`)
	if status != http.StatusCreated || got["scope"] != "ingest" || got["token"] == "" {
		t.Fatalf("create token: status=%d body=%v", status, got)
	}
	tokenID := got["id"].(float64)
	if status, _ := postJSON(t, c, srv.URL+"/api/v1/tokens", `{"name":"x","scope":"root"}`); status != http.StatusBadRequest {
		t.Fatalf("invalid scope: status=%d, want 400", status)
	}
	if status, _ := postJSON(t, c, srv.URL+"/api/v1/tokens", `{"name":"  "}`); status != http.StatusBadRequest {
		t.Fatalf("blank token name: status=%d, want 400", status)
	}

	// The list shows the minted token; revoking it succeeds and the list then
	// carries its revoked_at.
	resp = mustGet(t, c, srv.URL+"/api/v1/tokens")
	var list struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode token list: %v", err)
	}
	resp.Body.Close()
	if len(list.Tokens) != 1 || list.Tokens[0]["name"] != "laptop" || list.Tokens[0]["revoked_at"] != nil {
		t.Fatalf("token list: %+v", list.Tokens)
	}
	if status, _ := postJSON(t, c, srv.URL+"/api/v1/tokens/9999/revoke", `{}`); status != http.StatusOK {
		// Revocation is scoped by owner and idempotent; an unknown id is a no-op.
		t.Fatalf("revoke unknown token: status=%d", status)
	}
	status, _ = postJSON(t, c, srv.URL+"/api/v1/tokens/"+strconv.Itoa(int(tokenID))+"/revoke", `{}`)
	if status != http.StatusOK {
		t.Fatalf("revoke token: status=%d", status)
	}
	resp = mustGet(t, c, srv.URL+"/api/v1/tokens")
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode token list: %v", err)
	}
	resp.Body.Close()
	if list.Tokens[0]["revoked_at"] == nil {
		t.Fatalf("revoked token still live in list: %+v", list.Tokens)
	}

	// Invites are admin-only JSON; the minted token lets the second account in.
	status, got = postJSON(t, c, srv.URL+"/api/v1/invites", `{"note":"for ada"}`)
	if status != http.StatusCreated || got["invite_token"] == "" {
		t.Fatalf("create invite: status=%d body=%v", status, got)
	}
	invite := got["invite_token"].(string)
	status, got = postJSON(t, anon, srv.URL+"/api/v1/auth/register",
		`{"username":"ada","password":"pw-ada","invite_token":"`+invite+`"}`)
	if status != http.StatusCreated || got["is_admin"] != false {
		t.Fatalf("invited register: status=%d body=%v", status, got)
	}

	// The non-admin cannot mint invites.
	if status, _ := postJSON(t, anon, srv.URL+"/api/v1/invites", `{"note":"nope"}`); status != http.StatusForbidden {
		t.Fatalf("non-admin invite: status=%d, want 403", status)
	}
}
