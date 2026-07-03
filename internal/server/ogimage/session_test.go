package ogimage

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// sampleCard builds a scored, dated session card with a textured activity strip, the fixture
// the render tests draw. The activity slice is the store's fixed histogram (length
// activityCells), so it exercises the same 1:1 bucket-to-cell path the real read produces.
func sampleCard() store.SessionCard {
	grade := "A"
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(100 * time.Minute)
	activity := make([]int64, activityCells)
	for i := range activity {
		activity[i] = int64(10_000 + (i%7)*3_000)
	}
	return store.SessionCard{
		ProjectKind:  "remote",
		ProjectName:  "akari",
		ProjectKey:   "github.com/jssblck/akari",
		Title:        "Add Open Graph cards to the project and session pages",
		MessageCount: 142,
		TotalTokens:  5_427_000,
		StartedAt:    &start,
		EndedAt:      &end,
		Grade:        &grade,
		Activity:     activity,
	}
}

func TestRenderSessionDimensionsAndDeterminism(t *testing.T) {
	c := sampleCard()

	first, err := RenderSession("github.com/jssblck/akari", c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != Width || b.Dy() != Height {
		t.Fatalf("size = %dx%d, want %dx%d", b.Dx(), b.Dy(), Width, Height)
	}

	// Identical inputs render byte-identical output, so an unchanged session re-rendered
	// after its cache expires yields the same card rather than a churned one.
	second, err := RenderSession("github.com/jssblck/akari", c)
	if err != nil {
		t.Fatalf("render again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("RenderSession is not deterministic for identical inputs")
	}
}

// TestRenderSessionUnscoredNoTitleNoSpan exercises the dashed branches: no user message
// (empty title, so the title line is skipped), no grade (QUALITY dashes), and no span (a nil
// activity strip and a dashed DURATION). It must still produce a valid card.
func TestRenderSessionUnscoredNoTitleNoSpan(t *testing.T) {
	c := store.SessionCard{ProjectKind: "local", ProjectName: "scratch", MessageCount: 3}
	b, err := RenderSession("local-scratch", c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// TestRenderSessionLongTextDoesNotError pins the graceful-clip fallback: a heading and a
// title far too long to fit must shrink then truncate with an ellipsis rather than fail
// the render, so an unusually long project key or first prompt never 500s the card.
func TestRenderSessionLongTextDoesNotError(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(3 * time.Hour)
	c := store.SessionCard{
		ProjectKind:  "remote",
		MessageCount: 1,
		StartedAt:    &start,
		EndedAt:      &end,
		Title:        strings.Repeat("a very long first prompt that keeps going ", 20),
	}
	heading := strings.Repeat("github.com/really-long-org-name/really-long-repo-name-", 5)
	b, err := RenderSession(heading, c)
	if err != nil {
		t.Fatalf("render with over-long text: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// TestActivityCellsMatchGeometry pins that the strip's cell count is a positive number the
// grid actually holds, so GenerateSession asks the store for a bucket count that lays out one
// bucket per cell. A zero or mismatched count would silently blank the strip.
func TestActivityCellsMatchGeometry(t *testing.T) {
	if activityCells != activityCols*activityRows {
		t.Fatalf("activityCells = %d, want cols*rows = %d", activityCells, activityCols*activityRows)
	}
	if activityCells <= 0 {
		t.Fatalf("activityCells = %d, want a positive cell count", activityCells)
	}
}
