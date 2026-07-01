package ogimage

import (
	"bytes"
	"image/png"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

func sampleAnalytics(now time.Time) store.Analytics {
	var series []store.DayPoint
	day := 24 * time.Hour
	for i := 0; i < 365; i++ {
		if i%9 == 3 {
			continue // leave gaps so empty cells appear
		}
		d := now.Add(-time.Duration(i) * day).Truncate(day)
		base := int64(200000 + i*3000)
		series = append(series, store.DayPoint{
			Day:       d,
			Input:     base / 4,
			Output:    base / 8,
			CacheRead: base / 2,
		})
	}
	return store.Analytics{
		Series:   series,
		TotalIn:  1_284_000_000,
		TotalOut: 92_000_000,
		Sessions: 1487,
	}
}

func TestRenderDimensionsAndDeterminism(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	a := sampleAnalytics(now)

	first, err := Render("grace", a, now)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("render produced no bytes")
	}

	img, err := png.Decode(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != Width || b.Dy() != Height {
		t.Fatalf("size = %dx%d, want %dx%d", b.Dx(), b.Dy(), Width, Height)
	}

	// Same inputs must render byte-identical output, so an unchanged overview does
	// not thrash the stored card (and the daily refresh is a no-op when nothing
	// changed but the clock).
	second, err := Render("grace", a, now)
	if err != nil {
		t.Fatalf("render again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("render is not deterministic for identical inputs")
	}
}

// TestRenderEmpty exercises the no-usage path: a brand-new public overview with no
// sessions still produces a valid card (an empty grid and zero figures).
func TestRenderEmpty(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	b, err := Render("newcomer", store.Analytics{}, now)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
