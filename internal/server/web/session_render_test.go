package web

import (
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// sessionFixture is a representative session detail for render assertions: a
// remote project session with a couple of turns and a tool call whose bodies live
// in the CAS, so the chip renders its stamps as body-opening buttons.
func sessionFixture() (store.SessionDetail, []store.Message, map[int][]store.ToolCallView) {
	start := time.Date(2026, 6, 28, 9, 2, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)
	d := store.SessionDetail{
		SessionSummary: store.SessionSummary{
			ID:               1826,
			Agent:            "codex",
			Machine:          "grace",
			GitBranch:        "main",
			Username:         "jessoteric",
			MessageCount:     5,
			UserMessageCount: 3,
			TotalInput:       180237,
			TotalOutput:      12248,
			TotalCacheRead:   807552,
			TotalCacheWrite:  4096,
			TotalCostUSD:     1.67,
			Visibility:       "internal",
			StartedAt:        &start,
			EndedAt:          &end,
		},
		ProjectID:   7,
		ProjectKey:  "jssblck/dots",
		ProjectName: "dots",
		ProjectKind: "remote",
	}
	msgs := []store.Message{
		{Ordinal: 0, Role: "user", Content: "Run the guarded rerun.", Timestamp: &start},
		{Ordinal: 1, Role: "assistant", Content: "Running the stop-slop pass.", Model: "gpt-5", HasToolUse: true, Timestamp: &start},
	}
	tools := map[int][]store.ToolCallView{
		1: {{
			MessageOrdinal: 1, ToolName: "shell_command", FilePath: "scripts/stop-slop.sh",
			InputSHA: "aaaa", InputBytes: 143, InputMediaType: "json",
			ResultSHA: "bbbb", ResultBytes: 5800, ResultMediaType: "text", ResultStatus: "ok",
		}},
	}
	return d, msgs, tools
}

// The redesigned session header carries its controls inline and opens tool bodies
// in a modal: an owner of an internal session gets a compact actions cluster with
// Publish and Delete, the full-width page wrapper, and the modal overlay host. The
// old stacked publish banners and the density toggle must be gone.
func TestSessionPageCompactHeaderAndModal(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	html := renderComponent(t, SessionPage(p, d, msgs, tools, nil, nil, 0, false, true))

	for _, want := range []string{
		`class="session-page"`,    // full-width wrapper drives main:has(> .session-page)
		`class="session-actions"`, // inline control cluster
		`>Publish</button>`,       // internal owner can publish
		`>Delete</button>`,        // owner can delete
		`id="session-modal"`,      // overlay host
		`id="session-inspector"`,  // dialog the body renders into
		`role="dialog"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("session page missing %q", want)
		}
	}
	for _, unwant := range []string{
		`class="publish card"`,   // old publish banner
		`class="publish subtle"`, // old delete banner
		`>Delete session</button>`,
		`data-density`, // density toggle removed
		`>Comfortable<`, `>Compact<`,
		`class="inspector"`, // docked inspector column removed
	} {
		if strings.Contains(html, unwant) {
			t.Errorf("session page should no longer render %q", unwant)
		}
	}
}

// The session stat strip folds input, output, cache read, cache write, and cost
// into one Tokens tile carrying its hover breakdown, and the always-on "live"
// badge is gone from the header.
func TestSessionStatsFoldTokensAndDropLive(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	html := renderComponent(t, SessionPage(p, d, msgs, tools, nil, nil, 0, true, true))

	// The tile must show the aggregate of all four token classes, and the tooltip
	// must carry each class value plus the cost, run through the same formatters
	// the loose tiles used. A nonzero cache-write guards that field specifically.
	total := FmtTokens(d.TotalInput + d.TotalOutput + d.TotalCacheRead + d.TotalCacheWrite)
	for _, want := range []string{
		`class="stat tokens-stat"`,                   // the folded tile
		`data-stat-key="tokens">` + total + `</div>`, // the aggregate, flashed on live change
		`class="stat-tip"`,                           // its hover breakdown
		`>Cache read</dt>`, `>Cache write</dt>`,      // the per-class labels
		`<dd>` + FmtTokens(d.TotalInput) + `</dd>`,                                // input value
		`<dd>` + FmtTokens(d.TotalCacheWrite) + `</dd>`,                           // cache-write value (nonzero)
		`class="tt-cost">` + FmtCost(d.TotalCostUSD, d.CostIncomplete) + `</div>`, // cost text
	} {
		if !strings.Contains(html, want) {
			t.Errorf("session stats missing %q", want)
		}
	}
	for _, unwant := range []string{
		`data-stat-key="input"`, `data-stat-key="cw"`, `data-stat-key="cost"`, // the old loose tiles
		`class="live-dot"`, // the always-on live badge
	} {
		if strings.Contains(html, unwant) {
			t.Errorf("session stats should no longer render %q", unwant)
		}
	}
}

// A published session owned by the viewer swaps Publish for Unpublish and surfaces
// the public share path as a link.
func TestSessionPagePublicShowsUnpublish(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	pub := "k3y"
	d.Visibility = "public"
	d.PublicID = &pub
	html := renderComponent(t, SessionPage(p, d, msgs, tools, nil, nil, 0, false, true))

	for _, want := range []string{`>Unpublish</button>`, `class="share-link`, PublicPath(pub)} {
		if !strings.Contains(html, want) {
			t.Errorf("public session page missing %q", want)
		}
	}
	if strings.Contains(html, `>Publish</button>`) {
		t.Error("a public session should not offer Publish")
	}
}

// A non-owner admin can delete but not publish; a plain logged-in viewer gets no
// actions cluster at all.
func TestSessionPageActionsGating(t *testing.T) {
	d, msgs, tools := sessionFixture()

	admin := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "ada", IsAdmin: true}
	adminHTML := renderComponent(t, SessionPage(admin, d, msgs, tools, nil, nil, 0, false, false))
	if !strings.Contains(adminHTML, `>Delete</button>`) {
		t.Error("admin should be able to delete a session they do not own")
	}
	if strings.Contains(adminHTML, `>Publish</button>`) {
		t.Error("a non-owner admin should not see Publish")
	}

	viewer := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "anna"}
	viewerHTML := renderComponent(t, SessionPage(viewer, d, msgs, tools, nil, nil, 0, false, false))
	if strings.Contains(viewerHTML, `class="session-actions"`) {
		t.Error("a plain viewer should see no actions cluster")
	}
}

// The public (logged-out) session page takes the same full-width wrapper and modal
// host, and drops the density toggle, so a published session reads with the same
// ergonomics as the owner's view.
func TestPublicSessionPageWrapperAndModal(t *testing.T) {
	d, msgs, tools := sessionFixture()
	pub := "k3y"
	d.Visibility = "public"
	d.PublicID = &pub
	html := renderComponent(t, PublicSessionPage(d, msgs, tools, nil, nil))

	for _, want := range []string{`class="session-page"`, `id="session-modal"`, `id="session-inspector"`} {
		if !strings.Contains(html, want) {
			t.Errorf("public session page missing %q", want)
		}
	}
	if strings.Contains(html, `data-density`) {
		t.Error("public session page should not render the density toggle")
	}
}
