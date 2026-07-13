package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
)

func doJSON(t *testing.T, client *http.Client, method, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var raw io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		raw = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, raw)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil && !errorsIsEOF(err) {
		t.Fatalf("decode %s %s response: %v", method, url, err)
	}
	return response, decoded
}

func errorsIsEOF(err error) bool { return err == io.EOF }

func TestHomepageStaysTemplatedWhileApplicationRoutesServeReact(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t)

	home := readBody(t, mustGet(t, http.DefaultClient, server.URL+"/"))
	if !strings.Contains(home, "Know what your agents actually did") {
		t.Fatal("root no longer renders the templated homepage")
	}
	for _, unwanted := range []string{"/app-assets/", "htmx", "charts.js", "app.js"} {
		if strings.Contains(home, unwanted) {
			t.Fatalf("templated homepage ships application runtime %q", unwanted)
		}
	}

	login := readBody(t, mustGet(t, http.DefaultClient, server.URL+"/login"))
	if !strings.Contains(login, `/app-assets/assets/index-`) || strings.Contains(login, "<form") {
		t.Fatalf("login route does not serve the embedded React shell: %s", login)
	}

	openapiResponse := mustGet(t, http.DefaultClient, server.URL+"/api/openapi.json")
	if got := openapiResponse.Header.Get("Content-Type"); !strings.Contains(got, "application/vnd.oai.openapi+json") {
		openapiResponse.Body.Close()
		t.Fatalf("OpenAPI content type = %q", got)
	}
	var document struct {
		OpenAPI string                     `json:"openapi"`
		Paths   map[string]json.RawMessage `json:"paths"`
	}
	if err := json.NewDecoder(openapiResponse.Body).Decode(&document); err != nil {
		openapiResponse.Body.Close()
		t.Fatalf("decode OpenAPI document: %v", err)
	}
	openapiResponse.Body.Close()
	if document.OpenAPI != "3.1.0" || document.Paths["/api/v1/app/sessions/{id}"] == nil {
		t.Fatalf("OpenAPI document is incomplete: version=%q paths=%d", document.OpenAPI, len(document.Paths))
	}
}

func TestApplicationAPIFlow(t *testing.T) {
	t.Parallel()
	server, st := newTestServer(t)
	client := newClient(t)

	response, bootstrap := doJSON(t, client, http.MethodGet, server.URL+"/api/v1/app/bootstrap", nil)
	if response.StatusCode != http.StatusOK || bootstrap["authenticated"] != false {
		t.Fatalf("anonymous bootstrap: status=%d body=%v", response.StatusCode, bootstrap)
	}
	response, _ = doJSON(t, client, http.MethodGet, server.URL+"/api/v1/app/overview", nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous overview = %d, want 401", response.StatusCode)
	}

	status, registered := postJSON(t, client, server.URL+"/api/v1/auth/register", `{"username":"grace","password":"hopper-1906"}`)
	if status != http.StatusCreated {
		t.Fatalf("register: status=%d body=%v", status, registered)
	}
	response, bootstrap = doJSON(t, client, http.MethodGet, server.URL+"/api/v1/app/bootstrap", nil)
	if response.StatusCode != http.StatusOK || bootstrap["authenticated"] != true || bootstrap["username"] != "grace" {
		t.Fatalf("authenticated bootstrap: status=%d body=%v", response.StatusCode, bootstrap)
	}
	if _, leaked := bootstrap["PasswordHash"]; leaked {
		t.Fatalf("bootstrap leaked password material: %v", bootstrap)
	}

	ctx := context.Background()
	user, err := st.UserByUsername(ctx, "grace")
	if err != nil {
		t.Fatalf("load registered user: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/grace/akari", "github.com", "grace", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	announced, err := st.Announce(ctx, store.AnnounceParams{
		UserID: user.ID, Agent: "codex", SourceSessionID: "react-api-flow",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "hopper",
	})
	if err != nil {
		t.Fatalf("announce session: %v", err)
	}
	rebuildWith(t, st, announced.SessionID, store.ProjectionDelta{Messages: []store.MessageDelta{
		{Ordinal: 0, Role: "user", Content: "trace the API migration"},
		{Ordinal: 1, Role: "assistant", Content: "The React read model is connected."},
	}})

	response, projects := doJSON(t, client, http.MethodGet, server.URL+"/api/v1/app/projects", nil)
	if response.StatusCode != http.StatusOK || len(projects["projects"].([]any)) != 1 {
		t.Fatalf("projects API: status=%d body=%v", response.StatusCode, projects)
	}
	response, sessions := doJSON(t, client, http.MethodGet, server.URL+"/api/v1/app/sessions", nil)
	if response.StatusCode != http.StatusOK || len(sessions["sessions"].([]any)) != 1 {
		t.Fatalf("sessions API: status=%d body=%v", response.StatusCode, sessions)
	}
	response, detail := doJSON(t, client, http.MethodGet, server.URL+"/api/v1/app/sessions/"+strconvFormat(announced.SessionID), nil)
	if response.StatusCode != http.StatusOK || detail["owner"] != true {
		t.Fatalf("session API: status=%d body=%v", response.StatusCode, detail)
	}

	response, publication := doJSON(t, client, http.MethodPut, server.URL+"/api/v1/app/sessions/"+strconvFormat(announced.SessionID)+"/publication", map[string]bool{"published": true})
	if response.StatusCode != http.StatusOK || publication["published"] != true || publication["public_id"] == "" {
		t.Fatalf("publish session: status=%d body=%v", response.StatusCode, publication)
	}
	publicID, _ := publication["public_id"].(string)
	response, public := doJSON(t, http.DefaultClient, http.MethodGet, server.URL+"/api/v1/app/public/sessions/"+publicID, nil)
	if response.StatusCode != http.StatusOK || public["snapshot"] == nil {
		t.Fatalf("public session API: status=%d body=%v", response.StatusCode, public)
	}
}

// The public project API publishes aggregate usage only. The by-user cost
// split names the accounts that ran in a repo and how much each spent, so it
// must never reach an anonymous caller even though the client would not
// render it; the signed-in project API keeps the breakdown.
func TestPublicProjectAPIOmitsUserBreakdown(t *testing.T) {
	t.Parallel()
	server, st := newTestServer(t)
	client := registerAdmin(t, server.URL)
	ctx := context.Background()

	user, err := st.UserByUsername(ctx, "grace")
	if err != nil {
		t.Fatalf("load registered user: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/grace/akari", "github.com", "grace", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	announced, err := st.Announce(ctx, store.AnnounceParams{
		UserID: user.ID, Agent: "claude", SourceSessionID: "public-users-split",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "hopper",
	})
	if err != nil {
		t.Fatalf("announce session: %v", err)
	}
	cost := 1.25
	ordinal := 1
	rebuildWith(t, st, announced.SessionID, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "chart the fleet"},
			{Ordinal: 1, Role: "assistant", Content: "Charted."},
		},
		Usage: []store.ProjUsage{{
			MessageOrdinal: &ordinal, Model: "claude-fable-5",
			Input: 100, Output: 50, CostUSD: &cost,
			OccurredAt: time.Now().UTC(), DedupKey: "public-users-split-0",
		}},
	})
	if err := st.PublishProjectOverview(ctx, projectID); err != nil {
		t.Fatalf("publish project: %v", err)
	}

	usersOf := func(body map[string]any) []any {
		analytics, ok := body["analytics"].(map[string]any)
		if !ok {
			t.Fatalf("response has no analytics object: %v", body)
		}
		users, _ := analytics["Users"].([]any)
		return users
	}

	response, private := doJSON(t, client, http.MethodGet, server.URL+"/api/v1/app/projects/"+strconvFormat(projectID), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("signed-in project API: status=%d body=%v", response.StatusCode, private)
	}
	if len(usersOf(private)) == 0 {
		t.Fatal("signed-in project API lost the by-user breakdown; the fixture should produce one row")
	}

	response, public := doJSON(t, http.DefaultClient, http.MethodGet, server.URL+"/api/v1/app/public/projects/"+strconvFormat(projectID), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("public project API: status=%d body=%v", response.StatusCode, public)
	}
	if users := usersOf(public); len(users) != 0 {
		t.Fatalf("public project API leaked the by-user breakdown: %v", users)
	}
}

func TestApplicationAPIGatesParsedReadsDuringRebuild(t *testing.T) {
	t.Parallel()
	server, _, worker := newTestServerWithReparse(t)
	client := registerAdmin(t, server.URL)
	worker.SetStatusForTest(parse.Status{InProgress: true, Done: 2, Total: 5, Failed: 1})
	t.Cleanup(func() { worker.SetStatusForTest(parse.Status{}) })

	response, body := doJSON(t, client, http.MethodGet, server.URL+"/api/v1/app/overview", nil)
	if response.StatusCode != http.StatusServiceUnavailable || body["error"] != "projection rebuild in progress" {
		t.Fatalf("gated overview: status=%d body=%v", response.StatusCode, body)
	}
	if response.Header.Get("Retry-After") != "2" {
		t.Fatalf("Retry-After = %q, want 2", response.Header.Get("Retry-After"))
	}
	response, account := doJSON(t, client, http.MethodGet, server.URL+"/api/v1/app/account", nil)
	if response.StatusCode != http.StatusOK || account["reparse"] == nil {
		t.Fatalf("account should remain available during rebuild: status=%d body=%v", response.StatusCode, account)
	}
}

func strconvFormat(value int64) string {
	return strconv.FormatInt(value, 10)
}
