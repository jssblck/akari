package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/config"
)

func TestStripPrefix(t *testing.T) {
	for _, tc := range []struct {
		path, prefix, want string
		ok                 bool
	}{
		{path: "/proxy/akari", prefix: "/proxy/akari", want: "/", ok: true},
		{path: "/proxy/akari/", prefix: "/proxy/akari", want: "/", ok: true},
		{path: "/proxy/akari/overview", prefix: "/proxy/akari", want: "/overview", ok: true},
		{path: "/proxy/akari-other/x", prefix: "/proxy/akari", ok: false},
		{path: "/overview", prefix: "/proxy/akari", ok: false},
	} {
		got, ok := stripPrefix(tc.path, tc.prefix)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("stripPrefix(%q, %q) = (%q, %v), want (%q, %v)", tc.path, tc.prefix, got, ok, tc.want, tc.ok)
		}
	}
}

func TestResolvePrefix(t *testing.T) {
	newRequest := func(header, value, secretHeader, secret string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/overview", nil)
		if header != "" {
			r.Header.Set(header, value)
		}
		if secretHeader != "" {
			r.Header.Set(secretHeader, secret)
		}
		return r
	}

	t.Run("static prefix", func(t *testing.T) {
		s := &Server{Cfg: config.Server{PathPrefix: "/proxy/akari"}}
		if got := s.resolvePrefix(newRequest("", "", "", "")); got != "/proxy/akari" {
			t.Fatalf("resolvePrefix = %q, want static prefix", got)
		}
	})

	t.Run("header wins over static", func(t *testing.T) {
		s := &Server{Cfg: config.Server{PathPrefix: "/static", PrefixHeader: "X-Forwarded-Prefix"}}
		if got := s.resolvePrefix(newRequest("X-Forwarded-Prefix", "/from/header", "", "")); got != "/from/header" {
			t.Fatalf("resolvePrefix = %q, want header prefix", got)
		}
	})

	t.Run("header normalizes trailing slash", func(t *testing.T) {
		s := &Server{Cfg: config.Server{PrefixHeader: "X-Forwarded-Prefix"}}
		if got := s.resolvePrefix(newRequest("X-Forwarded-Prefix", "/proxy/akari/", "", "")); got != "/proxy/akari" {
			t.Fatalf("resolvePrefix = %q, want normalized header prefix", got)
		}
	})

	t.Run("invalid header falls back", func(t *testing.T) {
		s := &Server{Cfg: config.Server{PathPrefix: "/static", PrefixHeader: "X-Forwarded-Prefix"}}
		for _, bad := range []string{"no-slash", "//evil.example", "/a/../b", "/a?b", `/a"b`} {
			if got := s.resolvePrefix(newRequest("X-Forwarded-Prefix", bad, "", "")); got != "/static" {
				t.Fatalf("resolvePrefix(%q) = %q, want fallback to static", bad, got)
			}
		}
	})

	t.Run("header unconfigured is ignored", func(t *testing.T) {
		s := &Server{Cfg: config.Server{}}
		if got := s.resolvePrefix(newRequest("X-Forwarded-Prefix", "/from/header", "", "")); got != "" {
			t.Fatalf("resolvePrefix = %q, want empty when no header configured", got)
		}
	})

	t.Run("proxy secret gates the header", func(t *testing.T) {
		s := &Server{Cfg: config.Server{
			PrefixHeader: "X-Forwarded-Prefix", ProxyAuthSecret: "s3cret", ProxyAuthSecretHeader: "X-Akari-Proxy-Secret",
		}}
		if got := s.resolvePrefix(newRequest("X-Forwarded-Prefix", "/proxy/akari", "X-Akari-Proxy-Secret", "wrong")); got != "" {
			t.Fatalf("resolvePrefix = %q, want empty on secret mismatch", got)
		}
		if got := s.resolvePrefix(newRequest("X-Forwarded-Prefix", "/proxy/akari", "X-Akari-Proxy-Secret", "s3cret")); got != "/proxy/akari" {
			t.Fatalf("resolvePrefix = %q, want header prefix on secret match", got)
		}
	})
}

// noRedirectClient keeps redirects observable while still presenting the
// browser origin signals the CSRF gate expects on writes.
func noRedirectClient(t *testing.T) *http.Client {
	t.Helper()
	client := newClient(t)
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

const testPrefix = "/proxy/akari"

func newPrefixedTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server, _ := newTestServerWithConfig(t, config.Server{PathPrefix: testPrefix})
	return server
}

func TestPathPrefixServesPrefixedAndStrippedPaths(t *testing.T) {
	server := newPrefixedTestServer(t)
	client := newClient(t)

	// The proxy may forward the external path unstripped or stripped; both must
	// route, and both must generate externalized URLs.
	for _, path := range []string{testPrefix + "/login", "/login"} {
		response := mustGet(t, client, server.URL+path)
		body := readBody(t, response)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, response.StatusCode)
		}
		if !strings.Contains(body, `window.__AKARI_BASE_PATH__="`+testPrefix+`";`) {
			t.Fatalf("GET %s: shell does not inject the base path: %s", path, body)
		}
		if !strings.Contains(body, `src="`+testPrefix+`/app-assets/assets/`) {
			t.Fatalf("GET %s: shell does not prefix asset URLs: %s", path, body)
		}
	}
}

func TestPathPrefixExternalizesLoginRedirect(t *testing.T) {
	server := newPrefixedTestServer(t)
	client := noRedirectClient(t)

	response := mustGet(t, client, server.URL+testPrefix+"/overview")
	defer response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", response.StatusCode)
	}
	want := testPrefix + "/login?next=%2Fproxy%2Fakari%2Foverview"
	if got := response.Header.Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestPathPrefixScopesSessionCookies(t *testing.T) {
	server := newPrefixedTestServer(t)
	client := noRedirectClient(t)

	response, err := client.Post(server.URL+testPrefix+"/api/v1/auth/register", "application/json", strings.NewReader(`{"username":"grace","password":"hopper-1906"}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("register: status = %d", response.StatusCode)
	}
	paths := map[string]string{}
	for _, c := range response.Cookies() {
		paths[c.Name] = c.Path
	}
	// The session and CSRF cookies scope to the external prefix, so unrelated
	// applications sharing the origin never see them.
	for _, name := range []string{cookieName, csrfCookieName} {
		if paths[name] != testPrefix {
			t.Fatalf("%s cookie Path = %q, want %q", name, paths[name], testPrefix)
		}
	}
}

func TestPathPrefixExternalizesOAuthDiscovery(t *testing.T) {
	server := newPrefixedTestServer(t)
	client := newClient(t)

	// The metadata is served at the plain well-known path and at the RFC 8414
	// suffixed form a client derives from a path-carrying issuer.
	for _, path := range []string{
		"/.well-known/oauth-authorization-server",
		"/.well-known/oauth-authorization-server" + testPrefix,
	} {
		response := mustGet(t, client, server.URL+path)
		var meta struct {
			Issuer                string `json:"issuer"`
			AuthorizationEndpoint string `json:"authorization_endpoint"`
		}
		if err := json.NewDecoder(response.Body).Decode(&meta); err != nil {
			t.Fatalf("GET %s: decode: %v", path, err)
		}
		response.Body.Close()
		if meta.Issuer != server.URL+testPrefix {
			t.Fatalf("GET %s: issuer = %q, want %q", path, meta.Issuer, server.URL+testPrefix)
		}
		if meta.AuthorizationEndpoint != server.URL+testPrefix+"/oauth/authorize" {
			t.Fatalf("GET %s: authorization_endpoint = %q", path, meta.AuthorizationEndpoint)
		}
	}

	response := mustGet(t, client, server.URL+"/.well-known/oauth-protected-resource"+testPrefix+"/mcp")
	var resource struct {
		Resource string `json:"resource"`
	}
	if err := json.NewDecoder(response.Body).Decode(&resource); err != nil {
		t.Fatalf("decode protected resource metadata: %v", err)
	}
	response.Body.Close()
	if resource.Resource != server.URL+testPrefix+"/mcp" {
		t.Fatalf("resource = %q, want %q", resource.Resource, server.URL+testPrefix+"/mcp")
	}
}

func TestSafeNextRequiresPrefix(t *testing.T) {
	s := &Server{}
	withPrefix := func(prefix string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/login", nil)
		if prefix != "" {
			r = r.WithContext(context.WithValue(r.Context(), prefixKey, prefix))
		}
		return r
	}

	// next is external by convention, so under a prefix a value that does not
	// carry the mount point (a link minted before the prefix existed) must fall
	// back rather than redirect off the mount after login.
	prefixed := withPrefix(testPrefix)
	fallback := testPrefix + "/overview"
	for next, want := range map[string]string{
		"/overview":                  fallback,
		testPrefix + "/sessions/5":   testPrefix + "/sessions/5",
		testPrefix:                   testPrefix,
		"/proxy/akari-other/x":       fallback,
		"":                           fallback,
		"https://evil.example/proxy": fallback,
	} {
		if got := s.safeNext(prefixed, next); got != want {
			t.Errorf("safeNext(prefixed, %q) = %q, want %q", next, got, want)
		}
	}

	// A root deployment keeps accepting plain rooted paths.
	if got := s.safeNext(withPrefix(""), "/overview"); got != "/overview" {
		t.Errorf("safeNext(root, /overview) = %q", got)
	}
}

func TestWellKnownSuffixResolvesPrefixWithoutHeader(t *testing.T) {
	// The C3 deployment mode: the prefix arrives per request via a header the
	// proxy attaches inside its mount block, but the origin-root well-known
	// forward is a separate rule with no reason to carry it. The suffix names
	// the mount, so discovery must externalize correctly anyway.
	server, _ := newTestServerWithConfig(t, config.Server{PrefixHeader: "X-Forwarded-Prefix"})
	client := newClient(t)

	response := mustGet(t, client, server.URL+"/.well-known/oauth-authorization-server"+testPrefix)
	var meta struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	if err := json.NewDecoder(response.Body).Decode(&meta); err != nil {
		t.Fatalf("decode auth server metadata: %v", err)
	}
	response.Body.Close()
	if meta.Issuer != server.URL+testPrefix {
		t.Fatalf("issuer = %q, want %q", meta.Issuer, server.URL+testPrefix)
	}
	if meta.AuthorizationEndpoint != server.URL+testPrefix+"/oauth/authorize" {
		t.Fatalf("authorization_endpoint = %q", meta.AuthorizationEndpoint)
	}

	response = mustGet(t, client, server.URL+"/.well-known/oauth-protected-resource"+testPrefix+"/mcp")
	var resource struct {
		Resource string `json:"resource"`
	}
	if err := json.NewDecoder(response.Body).Decode(&resource); err != nil {
		t.Fatalf("decode protected resource metadata: %v", err)
	}
	response.Body.Close()
	if resource.Resource != server.URL+testPrefix+"/mcp" {
		t.Fatalf("resource = %q, want %q", resource.Resource, server.URL+testPrefix+"/mcp")
	}

	// The prefix-mounted (stripped) form still resolves from the header: its
	// suffix carries no prefix information, only the header does.
	request, err := http.NewRequest(http.MethodGet, server.URL+"/.well-known/oauth-protected-resource/mcp", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("X-Forwarded-Prefix", testPrefix)
	headered, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET headered well-known: %v", err)
	}
	if err := json.NewDecoder(headered.Body).Decode(&resource); err != nil {
		t.Fatalf("decode headered metadata: %v", err)
	}
	headered.Body.Close()
	if resource.Resource != server.URL+testPrefix+"/mcp" {
		t.Fatalf("headered resource = %q, want %q", resource.Resource, server.URL+testPrefix+"/mcp")
	}
}

func TestPathPrefixExternalizesOpenAPIServer(t *testing.T) {
	server := newPrefixedTestServer(t)
	client := newClient(t)

	body := readBody(t, mustGet(t, client, server.URL+testPrefix+"/api/openapi.json"))
	if !strings.Contains(body, `"url": "`+testPrefix+`/"`) {
		t.Fatalf("openapi servers url is not prefixed: %s", body[:200])
	}
}

func TestPathPrefixExternalizesGuideSurfaces(t *testing.T) {
	server := newPrefixedTestServer(t)
	client := newClient(t)

	llms := readBody(t, mustGet(t, client, server.URL+testPrefix+"/llms.txt"))
	if !strings.Contains(llms, server.URL+testPrefix+"/guide") {
		t.Fatalf("llms.txt does not carry prefixed URLs: %s", llms)
	}

	// The landing page is the live templ surface: its static asset links and
	// its navigation must externalize through the render context.
	landing := readBody(t, mustGet(t, client, server.URL+testPrefix+"/"))
	if !strings.Contains(landing, `href="`+testPrefix+`/static/css/base.css?v=`) {
		t.Fatalf("landing page does not prefix static assets: %s", landing)
	}
	if !strings.Contains(landing, `href="`+testPrefix+`/guide"`) {
		t.Fatalf("landing page does not prefix nav links: %s", landing)
	}
}

func TestPrefixHeaderResolvesPerRequest(t *testing.T) {
	server, _ := newTestServerWithConfig(t, config.Server{PrefixHeader: "X-Forwarded-Prefix"})
	client := newClient(t)

	request, err := http.NewRequest(http.MethodGet, server.URL+"/login", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("X-Forwarded-Prefix", testPrefix)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	if body := readBody(t, response); !strings.Contains(body, `window.__AKARI_BASE_PATH__="`+testPrefix+`";`) {
		t.Fatalf("header-asserted prefix not injected: %s", body)
	}

	// The same instance reached without the header is a root deployment.
	if body := readBody(t, mustGet(t, client, server.URL+"/login")); !strings.Contains(body, `window.__AKARI_BASE_PATH__="";`) {
		t.Fatalf("headerless request should resolve the root prefix: %s", body)
	}
}
