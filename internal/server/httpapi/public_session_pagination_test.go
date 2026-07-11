package httpapi

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

const publicPaginationID = "public-pagination-capability"

func publicMessages(prefix string, count int) []store.MessageDelta {
	msgs := make([]store.MessageDelta, count)
	for i := range msgs {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		msgs[i] = store.MessageDelta{Ordinal: i, Role: role, Content: fmt.Sprintf("%s-%03d", prefix, i)}
	}
	return msgs
}

func seedPublishedPaginationSession(t *testing.T, st *store.Store, delta store.ProjectionDelta) int64 {
	t.Helper()
	ctx := context.Background()
	owner, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "public-pagination",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce session: %v", err)
	}
	rebuildWith(t, st, ann.SessionID, delta)
	if _, err := st.PublishSession(ctx, ann.SessionID, owner.ID, publicPaginationID); err != nil {
		t.Fatalf("publish session: %v", err)
	}
	return ann.SessionID
}

var publicEarlierURL = regexp.MustCompile(`hx-get="([^"]+/body\?before=[^"]+)"`)
var publicMessageID = regexp.MustCompile(`id="msg-(\d+)"`)

func earlierPath(t *testing.T, body string) string {
	t.Helper()
	m := publicEarlierURL.FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatalf("public page has no earlier-page URL:\n%s", body)
	}
	return html.UnescapeString(m[1])
}

func responseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return string(b)
}

func TestPublicSessionTranscriptPaginationIsBoundedAndGapFree(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	image := []byte("a deliberately fetched image body")
	imageSHA := store.HashBytes(image)
	delta := store.ProjectionDelta{
		Messages: publicMessages("original", 240),
		Attachments: []store.AttachmentDelta{{
			MessageOrdinal: 239, Body: string(image), Bytes: int64(len(image)), MediaType: "image/png", Filename: "hopper.png",
		}},
		ToolCalls: []store.ProjToolCall{{
			MessageOrdinal: 239, CallIndex: 0, ToolName: "Read", CallUID: "call-public-body",
			InputBody: `{"file_path":"large.txt"}`, InputBytes: 30, InputMediaType: "application/json",
		}},
	}
	seedPublishedPaginationSession(t, st, delta)

	pageResp := mustGet(t, http.DefaultClient, srv.URL+"/s/"+publicPaginationID)
	page := responseBody(t, pageResp)
	if pageResp.StatusCode != http.StatusOK {
		t.Fatalf("public page status = %d, want 200", pageResp.StatusCode)
	}
	if got := pageResp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("public page Cache-Control = %q, want no-store", got)
	}
	if got := strings.Count(page, `id="msg-`); got != 100 {
		t.Fatalf("initial public transcript rendered %d messages, want the bounded 100-message tail", got)
	}
	for _, want := range []string{`id="msg-140"`, `id="msg-239"`, "original-239", "/s/" + publicPaginationID + "/blob/" + imageSHA} {
		if !strings.Contains(page, want) {
			t.Fatalf("initial public page missing %q", want)
		}
	}
	if strings.Contains(page, `id="msg-139"`) || strings.Contains(page, string(image)) {
		t.Fatal("initial public request loaded content outside the tail window or an on-demand body")
	}
	if strings.Contains(page, `<img src="/s/`+publicPaginationID+`/blob/`+imageSHA) {
		t.Fatal("public image attachment fetched implicitly instead of rendering as an explicit link")
	}

	// The outline rail covers the whole session, not just the tail window the
	// transcript renders: a visitor can jump to the first turn even though it sits
	// well outside the initial 100-message page.
	for _, want := range []string{`id="ol-0"`, `href="#msg-0"`, `id="ol-239"`, `href="#msg-239"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("initial public page outline missing %q, want anchors for both the first and last message:\n%s", want, page)
		}
	}

	earlier := earlierPath(t, page)
	fragmentResp := mustGet(t, http.DefaultClient, srv.URL+earlier)
	fragment := responseBody(t, fragmentResp)
	if fragmentResp.StatusCode != http.StatusOK {
		t.Fatalf("earlier fragment status = %d, want 200", fragmentResp.StatusCode)
	}
	if got := fragmentResp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("earlier fragment Cache-Control = %q, want no-store", got)
	}
	if got := strings.Count(fragment, `id="msg-`); got != 100 {
		t.Fatalf("earlier fragment rendered %d messages, want 100", got)
	}
	for i := 40; i < 140; i++ {
		id := fmt.Sprintf(`id="msg-%d"`, i)
		if !strings.Contains(fragment, id) {
			t.Fatalf("earlier fragment skipped ordinal %d", i)
		}
		if strings.Contains(page, id) {
			t.Fatalf("ordinal %d appeared in both adjacent pages", i)
		}
	}
	finalResp := mustGet(t, http.DefaultClient, srv.URL+earlierPath(t, fragment))
	finalPage := responseBody(t, finalResp)
	if finalResp.StatusCode != http.StatusOK {
		t.Fatalf("final earlier fragment status = %d, want 200", finalResp.StatusCode)
	}
	if strings.Contains(finalPage, `id="transcript-earlier"`) {
		t.Fatal("first transcript page still offered an earlier cursor")
	}

	seen := make(map[string]int)
	for _, body := range []string{page, fragment, finalPage} {
		for _, match := range publicMessageID.FindAllStringSubmatch(body, -1) {
			seen[match[1]]++
		}
	}
	if len(seen) != 240 {
		t.Fatalf("pagination covered %d distinct ordinals, want 240", len(seen))
	}
	for i := 0; i < 240; i++ {
		key := fmt.Sprintf("%d", i)
		if seen[key] != 1 {
			t.Fatalf("ordinal %d appeared %d times across the unchanged projection", i, seen[key])
		}
	}
}

func TestPublicSessionProjectionChangeResyncsAndRevocationStopsPaging(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	sessionID := seedPublishedPaginationSession(t, st, store.ProjectionDelta{Messages: publicMessages("old", 240)})

	pageResp := mustGet(t, http.DefaultClient, srv.URL+"/s/"+publicPaginationID)
	page := responseBody(t, pageResp)
	staleEarlier := earlierPath(t, page)

	// A whole-session rebuild can renumber every earlier turn. The stale revision
	// must replace the body from the new tail rather than append into the old DOM.
	rebuildWith(t, st, sessionID, store.ProjectionDelta{Messages: publicMessages("rebuilt", 20)})
	resyncResp := mustGet(t, http.DefaultClient, srv.URL+staleEarlier)
	resync := responseBody(t, resyncResp)
	if resyncResp.StatusCode != http.StatusOK {
		t.Fatalf("stale page request status = %d, want 200", resyncResp.StatusCode)
	}
	for name, want := range map[string]string{
		"HX-Retarget":                "#session-body",
		"HX-Reswap":                  "innerHTML",
		"X-Akari-Projection-Changed": "1",
		"Cache-Control":              "no-store",
	} {
		if got := resyncResp.Header.Get(name); got != want {
			t.Errorf("stale page %s = %q, want %q", name, got, want)
		}
	}
	if strings.Contains(resync, "old-") || !strings.Contains(resync, "rebuilt-019") {
		t.Fatalf("resync mixed projections:\n%s", resync)
	}
	if got := strings.Count(resync, `id="msg-`); got != 20 {
		t.Fatalf("resync rendered %d transcript rows, want the new 20-row projection", got)
	}

	detail, err := st.SessionDetailByID(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read owner: %v", err)
	}
	if err := st.UnpublishSession(context.Background(), sessionID, detail.OwnerID); err != nil {
		t.Fatalf("revoke publication: %v", err)
	}
	revoked := mustGet(t, http.DefaultClient, srv.URL+staleEarlier)
	revokedBody := responseBody(t, revoked)
	if revoked.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked earlier page status = %d body=%q, want 404", revoked.StatusCode, revokedBody)
	}
	if got := revoked.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("revoked earlier page Cache-Control = %q, want no-store", got)
	}
}
