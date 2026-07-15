package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestOAuthDecisionAcceptsCookieTokenWithoutOrigin exercises the fallback used
// by privacy-preserving browsers and proxies that omit both browser-origin
// signals. The React consent model must expose the cookie token so its form can
// prove that it came from this server.
func TestOAuthDecisionAcceptsCookieTokenWithoutOrigin(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t)
	browser := registerAdmin(t, server.URL)
	client := &http.Client{
		Jar: browser.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	registration, err := json.Marshal(map[string]any{
		"client_name":   "Ada's agent",
		"redirect_uris": []string{"http://127.0.0.1:9999/callback"},
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Post(server.URL+"/oauth/register", "application/json", strings.NewReader(string(registration)))
	if err != nil {
		t.Fatalf("register OAuth client: %v", err)
	}
	var registered map[string]any
	decodeBody(t, response, &registered)
	clientID, _ := registered["client_id"].(string)
	if clientID == "" {
		t.Fatalf("registration returned no client_id: %v", registered)
	}

	redirectURI := "http://127.0.0.1:9999/callback"
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {"state-ada"},
		"code_challenge":        {"challenge-ada-lovelace"},
		"code_challenge_method": {"S256"},
	}.Encode()
	response, err = client.Get(server.URL + "/api/v1/app/oauth/authorize?" + query)
	if err != nil {
		t.Fatalf("read consent model: %v", err)
	}
	var consent map[string]any
	decodeBody(t, response, &consent)
	appCSRF, _ := consent["app_csrf"].(string)
	oauthCSRF, _ := consent["csrf"].(string)
	if appCSRF == "" || oauthCSRF == "" {
		t.Fatalf("consent model omitted CSRF tokens: %v", consent)
	}

	response, err = client.PostForm(server.URL+"/oauth/authorize", url.Values{
		"client_id":      {clientID},
		"redirect_uri":   {redirectURI},
		"state":          {"state-ada"},
		"code_challenge": {"challenge-ada-lovelace"},
		"csrf":           {oauthCSRF},
		csrfFormName:     {appCSRF},
		"decision":       {"deny"},
	})
	if err != nil {
		t.Fatalf("submit consent without origin headers: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("consent status = %d, want %d", response.StatusCode, http.StatusSeeOther)
	}
	location, err := response.Location()
	if err != nil {
		t.Fatalf("parse consent redirect: %v", err)
	}
	if got := location.Query().Get("error"); got != "access_denied" {
		t.Fatalf("consent redirect error = %q, want access_denied", got)
	}
}
