// Package ogimage renders the Open Graph preview card for a published usage
// overview: a 1200x630 PNG a link unfurler (Slack, Discord, iMessage, and the
// like) shows when someone shares a /u/<username> link. It is a self-contained,
// pure-Go raster render in the house style (the machinist's bench: a deep
// violet-graphite ground, a lilac activity grid, and machined mono figures), so
// the binary stays free of a headless browser or any Node toolchain.
//
// The card carries a simplified copy of the overview's activity heatmap (the same
// trailing-year calendar of daily token intensity, drawn as flat lilac cells),
// plus the two headline figures a shared link most wants to convey: the total
// tokens and the session count over the default window.
package ogimage

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/jssblck/akari/internal/server/store"
)

// The card is the standard Open Graph size. 1200x630 (1.91:1) is what every major
// unfurler crops to, so rendering at that exact size avoids a re-crop that would
// clip the grid or the figures.
const (
	Width  = 1200
	Height = 630
)

//go:embed fonts/*.ttf
var fontFS embed.FS

// The house palette, straight from DESIGN.md. The heatmap levels are the same
// steps the live grid uses (overview.css): level 0 is Surface Elevated, levels 1-4
// step Machined Lilac's alpha over the Room ground, which we pre-composite to
// solid colors here since a PNG card has no page behind it to blend against.
var (
	colRoom     = color.NRGBA{0x14, 0x13, 0x1b, 0xff} // The Room, the page ground
	colSurface3 = color.NRGBA{0x2b, 0x29, 0x39, 0xff} // Surface Elevated (empty cell)
	colScribe   = color.NRGBA{0x38, 0x35, 0x48, 0xff} // Scribe Line hairline
	colLilac    = color.NRGBA{0xc6, 0xa8, 0xf2, 0xff} // Machined Lilac
	colText     = color.NRGBA{0xe6, 0xe3, 0xf0, 0xff} // Text
	colMuted    = color.NRGBA{0x9a, 0x94, 0xad, 0xff} // Muted (labels)
)

// heatLevels are the five cell fills, level 0 (empty) through level 4 (peak),
// matching .lvl-0..4 in overview.css after compositing the lilac alphas over the
// Room ground.
var heatLevels = [5]color.NRGBA{
	colSurface3,
	blend(colLilac, colRoom, 0.28),
	blend(colLilac, colRoom, 0.50),
	blend(colLilac, colRoom, 0.74),
	colLilac,
}

// blend composites fg over bg at the given alpha, the same as painting a
// semi-transparent fill on an opaque ground. It lets the heatmap cells match the
// live grid's rgba lilac steps as solid colors.
func blend(fg, bg color.NRGBA, alpha float64) color.NRGBA {
	mix := func(a, b uint8) uint8 {
		return uint8(math.Round(float64(a)*alpha + float64(b)*(1-alpha)))
	}
	return color.NRGBA{mix(fg.R, bg.R), mix(fg.G, bg.G), mix(fg.B, bg.B), 0xff}
}

// faces holds the parsed font faces at the sizes the card uses. Parsing a face is
// cheap but not free, so a render builds them once up front. Geist Mono carries
// every figure (the tabular-tolerance rule); Geist SemiBold carries the display
// and label text.
type faces struct {
	num   font.Face // big mono figures (the two headline numbers)
	name  font.Face // the username heading (semibold display)
	label font.Face // the uppercase stat labels
	brand font.Face // the wordmark
	sub   font.Face // the "/ usage overview" subhead
}

func newFaces() (*faces, error) {
	mono, err := loadFont("fonts/GeistMono-Medium.ttf")
	if err != nil {
		return nil, err
	}
	sans, err := loadFont("fonts/Geist-SemiBold.ttf")
	if err != nil {
		return nil, err
	}
	mk := func(f *opentype.Font, size float64) (font.Face, error) {
		return opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	}
	fc := &faces{}
	specs := []struct {
		dst  *font.Face
		f    *opentype.Font
		size float64
	}{
		{&fc.num, mono, 72},
		{&fc.name, sans, 58},
		{&fc.label, sans, 22},
		{&fc.brand, sans, 34},
		{&fc.sub, sans, 34},
	}
	for _, s := range specs {
		face, err := mk(s.f, s.size)
		if err != nil {
			return nil, err
		}
		*s.dst = face
	}
	return fc, nil
}

func loadFont(name string) (*opentype.Font, error) {
	b, err := fontFS.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return opentype.Parse(b)
}

// Render draws the overview card for one account and returns the encoded PNG. The
// heatmap and the two figures are scoped to whatever the analytics carry (the
// caller queries the default trailing-year window, so the card matches the page a
// visitor first lands on). now fixes the grid's trailing edge, injected so the
// render is deterministic under test.
func Render(username string, a store.Analytics, now time.Time) ([]byte, error) {
	fc, err := newFaces()
	if err != nil {
		return nil, err
	}

	img := image.NewNRGBA(image.Rect(0, 0, Width, Height))
	fillRect(img, img.Bounds(), colRoom)

	// A single scribed hairline frame, inset from the edge: the bench's one-line
	// border, not a heavy box (the scribed-line rule).
	strokeRect(img, image.Rect(1, 1, Width-1, Height-1), colScribe)

	const pad = 64

	// Brand wordmark, top-left: the lilac aperture glyph beside "akari".
	drawAperture(img, pad+11, pad+11, 12)
	brandBaseline := pad + 22
	drawText(img, fc.brand, pad+34, brandBaseline, colText, "akari")

	// No "public" tag and no "as of" date: the card only exists for a published
	// overview, so anyone seeing it already knows it is public, and a date stamp is
	// noise on a glanceable share thumbnail. The figures are the trailing-year totals
	// as of the render, which is enough context for a preview.

	// Heading: the username in the display face, then "/ usage overview" muted, the
	// same scent as the page's own head.
	headBase := pad + 150
	nameEnd := drawText(img, fc.name, pad, headBase, colText, username)
	drawText(img, fc.sub, nameEnd+18, headBase, colMuted, "/ usage overview")

	// The activity grid sits in the card's middle band, full trailing year.
	gridTop := pad + 196
	drawHeatmap(img, a, now, pad, gridTop, Width-2*pad)

	// The two headline figures, along the foot: total tokens and session count, each
	// number (mono, for tabular tolerance) nested directly under its uppercase label
	// so the two stat blocks read as two columns rather than one crowded row.
	//
	// The token total is shown here on its own, without akari's usual four-class split
	// and cost. Everywhere else a bare total is paired with its breakdown (input,
	// output, cache read, cache write, cost) so a figure is never seen without its
	// composition; this card is the deliberate exception. An Open Graph card is a
	// glanceable thumbnail seen at a distance in a chat unfurl, and five extra figures
	// crammed under the total is information overload at that size. The card shows the
	// total and the session count only; the full breakdown is one click away on the
	// page the card links to.
	numBase := Height - pad - 12
	// Sit the label clear of the number rather than tight against it: the figures are
	// 72pt, so a small offset buries the label against the digits' caps. This drop
	// echoes the space between the PUBLIC tag and the "as of" date up top.
	labelY := numBase - 66
	drawText(img, fc.label, pad, labelY, colMuted, "TOTAL TOKENS")
	drawText(img, fc.num, pad, numBase, colText, fmtScale(a.TotalTokens()))

	col2 := Width/2 + 120
	drawText(img, fc.label, col2, labelY, colMuted, "SESSIONS")
	drawText(img, fc.num, col2, numBase, colText, fmtScale(int64(a.Sessions)))

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawHeatmap renders the simplified activity calendar: a trailing 53-week grid,
// one cell per day, cell intensity stepped by that day's total token volume. It
// mirrors the live grid in charts.js (a Sunday-aligned start, a sqrt intensity
// ramp, five levels) so the card reads as the same instrument, just smaller and
// static. It draws left-to-right by week, top-to-bottom by weekday.
func drawHeatmap(img *image.NRGBA, a store.Analytics, now time.Time, x, y, width int) {
	const (
		weeks = 53
		rows  = 7
		gap   = 4
	)
	cell := (width - (weeks-1)*gap) / weeks
	if cell < 1 {
		cell = 1
	}

	// Per-day token totals, keyed by UTC date, the same bucketing the store uses.
	byDay := make(map[string]int64, len(a.Series))
	for _, p := range a.Series {
		byDay[p.Day.UTC().Format("2006-01-02")] += p.Input + p.Output + p.CacheRead + p.CacheWrite
	}

	// The grid ends on the current week and spans `weeks` columns back to a Sunday,
	// exactly as the client computes it.
	day := 24 * time.Hour
	end := now.UTC().Truncate(day)
	start := end.Add(-time.Duration(int(end.Weekday())) * day).Add(-time.Duration((weeks-1)*7) * day)

	// Peak volume across the visible window sets the ramp ceiling.
	var max int64
	for c := 0; c < weeks; c++ {
		for r := 0; r < rows; r++ {
			if v := byDay[start.Add(time.Duration(c*7+r)*day).Format("2006-01-02")]; v > max {
				max = v
			}
		}
	}

	for c := 0; c < weeks; c++ {
		for r := 0; r < rows; r++ {
			d := start.Add(time.Duration(c*7+r) * day)
			if d.After(end) {
				continue // future days in the current week stay blank
			}
			lvl := levelFor(byDay[d.Format("2006-01-02")], max)
			cx := x + c*(cell+gap)
			cy := y + r*(cell+gap)
			fillRect(img, image.Rect(cx, cy, cx+cell, cy+cell), heatLevels[lvl])
		}
	}
}

// levelFor maps a day's value to one of five intensity steps, the same sqrt ramp
// as the live grid so a small day stays legible off the floor.
func levelFor(v, max int64) int {
	if v <= 0 || max <= 0 {
		return 0
	}
	f := math.Sqrt(float64(v) / float64(max))
	switch {
	case f > 0.75:
		return 4
	case f > 0.5:
		return 3
	case f > 0.25:
		return 2
	default:
		return 1
	}
}

// drawAperture draws the brand's small lilac aperture: an open ring with a filled
// center, the wordmark glyph rendered at cx,cy with the given outer radius.
func drawAperture(img *image.NRGBA, cx, cy, radius int) {
	strokeCircle(img, cx, cy, radius, colLilac)
	fillCircle(img, cx, cy, radius/3+1, colLilac)
}

// --- primitive raster helpers ---------------------------------------------

func fillRect(img *image.NRGBA, r image.Rectangle, c color.NRGBA) {
	draw.Draw(img, r, &image.Uniform{c}, image.Point{}, draw.Src)
}

// strokeRect draws a 1px hairline rectangle border.
func strokeRect(img *image.NRGBA, r image.Rectangle, c color.NRGBA) {
	fillRect(img, image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+1), c)
	fillRect(img, image.Rect(r.Min.X, r.Max.Y-1, r.Max.X, r.Max.Y), c)
	fillRect(img, image.Rect(r.Min.X, r.Min.Y, r.Min.X+1, r.Max.Y), c)
	fillRect(img, image.Rect(r.Max.X-1, r.Min.Y, r.Max.X, r.Max.Y), c)
}

// fillCircle and strokeCircle draw the small brand glyph. They sample per pixel in
// the bounding box, which is fine for a glyph a dozen pixels across.
func fillCircle(img *image.NRGBA, cx, cy, radius int, c color.NRGBA) {
	rr := float64(radius) * float64(radius)
	for y := cy - radius; y <= cy+radius; y++ {
		for x := cx - radius; x <= cx+radius; x++ {
			dx, dy := float64(x-cx), float64(y-cy)
			if dx*dx+dy*dy <= rr {
				img.SetNRGBA(x, y, c)
			}
		}
	}
}

func strokeCircle(img *image.NRGBA, cx, cy, radius int, c color.NRGBA) {
	outer := float64(radius) * float64(radius)
	inner := float64(radius-2) * float64(radius-2)
	for y := cy - radius; y <= cy+radius; y++ {
		for x := cx - radius; x <= cx+radius; x++ {
			dx, dy := float64(x-cx), float64(y-cy)
			d := dx*dx + dy*dy
			if d <= outer && d >= inner {
				img.SetNRGBA(x, y, c)
			}
		}
	}
}

// drawText paints s at the given left edge x and baseline y in color c, returning
// the x just past the drawn text (its advance), so callers can chain runs.
func drawText(img *image.NRGBA, face font.Face, x, y int, c color.NRGBA, s string) int {
	d := &font.Drawer{
		Dst:  img,
		Src:  &image.Uniform{c},
		Face: face,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
	return d.Dot.X.Round()
}

// fmtScale renders a count in a compact scaled form for the card's headline
// figures. The scale climbs K, M (million), B (billion), then T (trillion), carrying
// up to three decimal places rounded down (never padded with trailing zeros); a
// count below 1000 is shown raw, with no suffix. So 512 stays "512", 1,696 becomes
// "1.696K", 2,000,000 becomes "2M", and 12,137,766,601 becomes "12.137B". The scale
// starts at K rather than the M the fleet-wide page uses, since one account's figures
// are smaller and would otherwise read as a long string of raw digits.
func fmtScale(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	// perMilli is a thousandth of each unit's divisor, so integer division by it
	// yields the value in thousandths (value x 1000) directly: floor(n*1000/divisor).
	// Working in integer thousandths keeps the round-down exact and dodges the drift a
	// float divide-then-truncate would introduce at these magnitudes.
	units := []struct {
		suffix   string
		perMilli int64
	}{
		{"T", 1_000_000_000},
		{"B", 1_000_000},
		{"M", 1_000},
		{"K", 1},
	}
	for _, u := range units {
		if n >= u.perMilli*1000 {
			thousandths := n / u.perMilli
			whole, frac := thousandths/1000, thousandths%1000
			s := strconv.FormatInt(whole, 10)
			if frac > 0 {
				s += "." + strings.TrimRight(fmt.Sprintf("%03d", frac), "0")
			}
			return s + u.suffix
		}
	}
	return strconv.FormatInt(n, 10) // unreachable: n >= 1000 always matches the K unit
}
