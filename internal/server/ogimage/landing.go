package ogimage

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

// The landing card is the Open Graph preview for the instance root ("/"): the
// image a link unfurler shows when someone shares the akari homepage itself,
// rather than a specific /u/<username> overview. It carries no account data (the
// wordmark, the product headline, a one-clause subline, and a decorative
// heatmap band in the house style), so it is static per binary: the same bytes
// for every request of a given build. That is why it skips the store's TTL cache
// the overview card uses (there is nothing per-user to key on and nothing that
// goes stale between deploys); it is rendered once lazily and memoized in
// process by Landing.
//
// The heatmap band here is decorative, not data: it exists so the homepage card
// visibly belongs to the same family as the overview card. Its pattern is a
// fixed hash of each cell's coordinates (no clock, no unseeded randomness), so
// renders stay byte-stable across runs and processes.

// LandingHeadline and LandingSubline are the canonical homepage copy, written
// down once here. The card draws them, and the httpapi root handler builds the
// landing page's og:title and og:description from them, so those surfaces
// follow a copy edit by construction. The templ hero (landing.templ) cannot
// import this package from a template, so a reconciliation test in the web
// package pins its h1 to LandingHeadline instead: an edit that lands in only
// one place fails that test rather than shipping a homepage, meta tags, and
// preview card that say different things.
const (
	LandingHeadline = "Know what your agents actually did."
	LandingSubline  = "Every Claude Code, Codex, and pi session in one searchable, priced history."
)

const (
	landingPad = 64

	// landingTextMargin is extra slack inside the padded column when fitting the
	// text sizes, so a fitted line never runs flush to the right pad boundary.
	landingTextMargin = 24
)

// landingFaces holds the font faces the landing card draws with. The headline
// and subline faces are size-fitted to the padded column at build time (see
// fitFace), so a copy edit that lengthens either line shrinks it to fit instead
// of clipping off the card's right edge.
type landingFaces struct {
	head  font.Face // the product headline (display semibold, fitted)
	brand font.Face // the "akari" wordmark
	sub   font.Face // the muted one-clause subline (fitted)
	label font.Face // the small uppercase band caption
}

func newLandingFaces() (*landingFaces, error) {
	sans, err := loadFont("fonts/Geist-SemiBold.ttf")
	if err != nil {
		return nil, err
	}
	mk := func(size float64) (font.Face, error) {
		return opentype.NewFace(sans, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	}
	fc := &landingFaces{}
	if fc.brand, err = mk(34); err != nil {
		return nil, err
	}
	if fc.label, err = mk(22); err != nil {
		return nil, err
	}
	maxW := Width - 2*landingPad - landingTextMargin
	if fc.head, err = fitFace(sans, LandingHeadline, 68, 40, maxW); err != nil {
		return nil, err
	}
	if fc.sub, err = fitFace(sans, LandingSubline, 30, 20, maxW); err != nil {
		return nil, err
	}
	return fc, nil
}

// fitFace returns a face at the largest whole-point size, walking down from
// maxSize, at which s measures no wider than maxWidth. Measuring up front (with
// the same hinting the draw uses) is what keeps the card's fixed-width column
// from clipping the text: the alternative, a hardcoded size, silently truncates
// the moment the copy or the font changes. minSize bounds the walk so an
// impossible fit fails loudly instead of rendering unreadably small text.
func fitFace(f *opentype.Font, s string, maxSize, minSize float64, maxWidth int) (font.Face, error) {
	for size := maxSize; size >= minSize; size-- {
		face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
		if err != nil {
			return nil, err
		}
		if font.MeasureString(face, s).Round() <= maxWidth {
			return face, nil
		}
	}
	return nil, fmt.Errorf("ogimage: %q does not fit %dpx even at %gpt", s, maxWidth, minSize)
}

// Landing renders the homepage preview card and returns the encoded PNG. It is
// memoized: the card is static per binary, so the first call renders and every
// later call returns the same bytes with no rework. The error is memoized too,
// so a font-load failure surfaces on every call rather than being retried.
func Landing() ([]byte, error) { return landingOnce() }

var landingOnce = sync.OnceValues(renderLanding)

// renderLanding draws the homepage card once. Its composition is deliberately
// spare: the wordmark and glyph up top, the product headline, a one-clause
// subline in the voice of the landing lede, and a decorative heatmap band along
// the foot to tie it to the overview card.
func renderLanding() ([]byte, error) {
	fc, err := newLandingFaces()
	if err != nil {
		return nil, err
	}

	img := image.NewNRGBA(image.Rect(0, 0, Width, Height))
	fillRect(img, img.Bounds(), colRoom)

	// The same single scribed hairline frame the overview card uses (the
	// scribed-line rule), so the two cards share a border, not just a palette.
	strokeRect(img, image.Rect(1, 1, Width-1, Height-1), colScribe)

	const pad = landingPad

	// Brand wordmark, top-left: the lilac aperture glyph beside "akari", drawn
	// with the overview card's shared glyph helper so the mark is identical.
	drawAperture(img, pad+11, pad+11, 12)
	drawText(img, fc.brand, pad+34, pad+22, colText, "akari")

	// The canonical headline and subline (shared with the root handler's meta
	// tags and, via the web package's reconciliation test, the templ hero), the
	// headline set large in the display face as the card's focal line and the
	// subline muted beneath it as support.
	drawText(img, fc.head, pad, pad+164, colText, LandingHeadline)
	drawText(img, fc.sub, pad, pad+220, colMuted, LandingSubline)

	// The decorative heatmap band along the foot, captioned like the overview
	// card's own grid so the two read as the same instrument. Tall enough to
	// anchor the card's lower third, so the space between the subline and the
	// band reads as breathing room, not an empty middle.
	bandTop := Height - pad - 150
	drawText(img, fc.label, pad, bandTop-16, colMuted, "ACTIVITY")
	drawLandingBand(img, pad, bandTop, Width-2*pad, 150)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawLandingBand paints the decorative activity band: a fixed grid of heatmap
// cells whose levels come from a deterministic hash of each cell's row and
// column, reusing heatLevels so the palette matches the overview card's real
// grid. It is illustration, not data, so the pattern only needs to be stable and
// on-brand, never accurate. Keeping it a pure function of (col, row) makes the
// whole card byte-identical across runs and processes.
func drawLandingBand(img *image.NRGBA, x, y, width, height int) {
	const (
		rows = 7
		gap  = 4
	)
	// Size the cells so a whole number of columns fills the band width at the
	// fixed gap, matching the overview grid's square cells and 4px gutter.
	cell := (height - (rows-1)*gap) / rows
	if cell < 1 {
		cell = 1
	}
	cols := (width + gap) / (cell + gap)
	for c := 0; c < cols; c++ {
		for r := 0; r < rows; r++ {
			lvl := landingCellLevel(c, r)
			cx := x + c*(cell+gap)
			cy := y + r*(cell+gap)
			fillRect(img, image.Rect(cx, cy, cx+cell, cy+cell), heatLevels[lvl])
		}
	}
}

// landingCellLevel maps a band cell's coordinates to one of the five heatmap
// levels through a small integer hash (an xorshift-style mix of col and row).
// It is deterministic and self-contained: no time, no package-level randomness,
// so the band draws the same on every render. The mix is chosen only to scatter
// the levels into a plausible activity texture, not to model anything.
func landingCellLevel(col, row int) int {
	h := uint32(col)*73856093 ^ uint32(row)*19349663
	h ^= h >> 13
	h *= 0x9e3779b1
	h ^= h >> 15
	// A real akari heatmap is mostly empty ground with scattered work, so the
	// mapping leans the same way: over half the cells stay level 0, most active
	// cells sit at the dim levels 1-2, and the bright levels are rare accents.
	// An even split reads as confetti, not activity.
	switch h % 16 {
	case 0:
		return 4
	case 1, 2:
		return 3
	case 3, 4:
		return 2
	case 5, 6:
		return 1
	default:
		return 0
	}
}
