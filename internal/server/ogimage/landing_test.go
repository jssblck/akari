package ogimage

import (
	"bytes"
	"image/png"
	"testing"

	"golang.org/x/image/font"
)

// TestLandingDimensionsAndDeterminism pins the homepage card: it decodes to
// exactly 1200x630, and two calls return byte-identical output. The card is
// static per binary (memoized), so a second call must match the first, which
// also guards the decorative band staying a pure function of cell coordinates
// (no clock, no unseeded randomness).
func TestLandingDimensionsAndDeterminism(t *testing.T) {
	first, err := Landing()
	if err != nil {
		t.Fatalf("landing: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("landing produced no bytes")
	}

	img, err := png.Decode(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != Width || b.Dy() != Height {
		t.Fatalf("size = %dx%d, want %dx%d", b.Dx(), b.Dy(), Width, Height)
	}

	second, err := Landing()
	if err != nil {
		t.Fatalf("landing again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("landing card is not deterministic across calls")
	}
}

// TestLandingTextFitsColumn guards the card's text against clipping: the
// headline and subline, measured at the very faces the render draws with, must
// fit inside the padded column. The faces are size-fitted (fitFace), so this
// pins the fit machinery end to end; a copy edit or font swap that overflows
// the column at every allowed size fails here rather than shipping a truncated
// card.
func TestLandingTextFitsColumn(t *testing.T) {
	fc, err := newLandingFaces()
	if err != nil {
		t.Fatalf("faces: %v", err)
	}
	budget := Width - 2*landingPad
	for _, line := range []struct {
		name string
		face font.Face
		text string
	}{
		{"headline", fc.head, landingHeadline},
		{"subline", fc.sub, landingSubline},
	} {
		if w := font.MeasureString(line.face, line.text).Round(); w > budget {
			t.Errorf("%s measures %dpx, exceeds the %dpx column (would clip)", line.name, w, budget)
		}
	}
}

// TestLandingBandReadsSparse pins the decorative band's texture: like a real
// akari heatmap it must be mostly empty ground with scattered activity, dim
// levels outnumbering bright ones. A remix of landingCellLevel that drifts back
// toward an even split (which reads as uniform noise, not activity) fails here.
func TestLandingBandReadsSparse(t *testing.T) {
	var counts [5]int
	total := 0
	// Sample the full plausible band extent; the property must hold across it,
	// not just on the columns the current cell size happens to draw.
	for c := 0; c < 70; c++ {
		for r := 0; r < 7; r++ {
			counts[landingCellLevel(c, r)]++
			total++
		}
	}
	if empty := float64(counts[0]) / float64(total); empty < 0.50 || empty > 0.75 {
		t.Errorf("level-0 share = %.2f, want mostly empty ground (0.50 to 0.75); counts %v", empty, counts)
	}
	if dim, bright := counts[1]+counts[2], counts[3]+counts[4]; dim <= bright {
		t.Errorf("dim cells (%d) must outnumber bright accents (%d); counts %v", dim, bright, counts)
	}
}
