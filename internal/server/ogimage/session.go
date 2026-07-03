package ogimage

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"

	"github.com/jssblck/akari/internal/durationfmt"
	"github.com/jssblck/akari/internal/server/store"
)

// sessionFaces are the faces the session card draws with. The heading and title faces
// are fit per text (see RenderSession), so only the fixed-size faces live here.
type sessionFaces struct {
	brand font.Face      // the "akari" wordmark
	label font.Face      // the uppercase stat and caption labels
	num   font.Face      // the four foot figures (mono, for tabular tolerance)
	sans  *opentype.Font // kept to fit the heading and title faces
}

// sessionStatNumSize is the point size of the session card's four foot figures. It is
// smaller than the overview card's 72pt because the session card carries four figures
// across the foot rather than two or three, so each column has less width to spend.
const sessionStatNumSize = 56.0

// The activity strip's geometry, as compile-time constants so the number of cells the strip
// draws (activityCells) is the same number of buckets the store aggregates usage into. The
// store buckets in SQL to exactly this count, so each returned bucket paints one cell with no
// resampling. Deriving the count from the geometry (rather than a hand-tuned literal) keeps
// the two in step if the band is ever resized. See drawSessionActivity for the layout.
const (
	sessionPad         = 64
	activityBandHeight = 132
	activityRows       = 7
	activityGap        = 4
	activityCell       = (activityBandHeight - (activityRows-1)*activityGap) / activityRows
	activityCols       = (Width - 2*sessionPad + activityGap) / (activityCell + activityGap)
	activityCells      = activityCols * activityRows
)

func newSessionFaces() (*sessionFaces, error) {
	mono, err := loadFont("fonts/GeistMono-Medium.ttf")
	if err != nil {
		return nil, err
	}
	sans, err := loadFont("fonts/Geist-SemiBold.ttf")
	if err != nil {
		return nil, err
	}
	fc := &sessionFaces{sans: sans}
	if fc.brand, err = newFace(sans, 34); err != nil {
		return nil, err
	}
	if fc.label, err = newFace(sans, 22); err != nil {
		return nil, err
	}
	if fc.num, err = newFace(mono, sessionStatNumSize); err != nil {
		return nil, err
	}
	return fc, nil
}

// RenderSession draws the preview card for one published session and returns the encoded
// PNG. It is the session counterpart to Render (the overview card): where the overview
// card draws a trailing-year calendar, the session card draws the session's own activity
// over its span (see drawSessionActivity), so both cards share a visualization band in
// the same house style. It leads with the same head the page shows (the project label,
// then "/ session") and the session's title (what the run was about), then the activity
// strip, then four foot figures: total tokens, message count, the session's quality
// grade, and its duration.
//
// heading is the project label the page's <h1> shows (derived from the card's project
// identity), passed in rather than derived here so this package stays free of the web view
// layer, the same reason Generate and GenerateProject take resolved strings. Every figure
// comes straight off the card's session rollups and gated grade the page also renders (the
// token figure folds all four classes, so no class is dropped: the static card just condenses
// the per-class breakdown the interactive page keeps behind a hover card), so the card
// reconciles with the session page a visitor lands on. c.Activity is the session's usage
// already bucketed over its span; it is nil for an undated or still-running session, in which
// case the strip draws as an empty grid.
func RenderSession(heading string, c store.SessionCard) ([]byte, error) {
	fc, err := newSessionFaces()
	if err != nil {
		return nil, err
	}

	img := image.NewNRGBA(image.Rect(0, 0, Width, Height))
	fillRect(img, img.Bounds(), colRoom)
	strokeRect(img, image.Rect(1, 1, Width-1, Height-1), colScribe)

	const pad = sessionPad

	// Brand wordmark, top-left: the lilac aperture glyph beside "akari", drawn with the
	// overview card's shared glyph helper so the mark is identical across cards.
	drawAperture(img, pad+11, pad+11, 12)
	drawText(img, fc.brand, pad+34, pad+22, colText, "akari")

	// A "public" tag on the top-right, right-aligned to the pad, echoing the page's own
	// public tag so the card reads as the same object.
	pubW := font.MeasureString(fc.label, "public").Round()
	drawText(img, fc.label, Width-pad-pubW, pad+18, colMuted, "public")

	// Heading: the project label in the display face, then "/ session" muted, the same
	// head the session page shows. The label face is fit to the text so a long project
	// key shrinks instead of overrunning the right pad.
	const subhead = "/ session"
	subW := font.MeasureString(fc.label, subhead).Round()
	headFace, headText, err := fitHeading(fc.sans, heading, Width-2*pad-18-subW)
	if err != nil {
		return nil, err
	}
	headBase := pad + 118
	nameEnd := drawText(img, headFace, pad, headBase, colText, headText)
	drawText(img, fc.label, nameEnd+16, headBase, colMuted, subhead)

	// The session title (its first user message, what the run was about): the most
	// shareable line, set below the head and fit to the padded column so a long prompt
	// shrinks or, past the smallest size, clips with an ellipsis rather than overrunning.
	// A session with no user message has no title, so the line is skipped.
	if c.Title != "" {
		titleFace, titleText, err := fitTextTrunc(fc.sans, c.Title, 38, 24, Width-2*pad)
		if err != nil {
			return nil, err
		}
		drawText(img, titleFace, pad, headBase+58, colText, titleText)
	}

	// The activity strip fills the card's middle band, captioned like the overview card's
	// grid so the two read as the same instrument. It is the session's own usage over its
	// span rather than a calendar year (see drawSessionActivity).
	bandTop := pad + 216
	drawText(img, fc.label, pad, bandTop-16, colMuted, "ACTIVITY")
	drawSessionActivity(img, c.Activity, pad, bandTop)

	// The four foot figures: total tokens (all four classes folded), message count, the
	// session's quality grade, and its duration. Quality and duration dash when unmeasured
	// (an unscored session, or one with no measured span) rather than printing a misleading
	// zero.
	grade := unmeasured
	if c.Grade != nil {
		grade = *c.Grade
	}
	// DURATION goes through the very function the page's Duration tile uses (web.FmtDuration
	// delegates to durationfmt.Span), so the same helper both decides the span is measurable and
	// formats it: the card cannot show a duration the page dashes, or a different figure than the
	// page, for any bounds. A "-" (unmeasured on the page) becomes the card's own em-dash marker,
	// the same one QUALITY uses, so an absent duration reads as unmeasured rather than a zero.
	dur := unmeasured
	if s := durationfmt.Span(c.StartedAt, c.EndedAt); s != "-" {
		dur = s
	}
	numBase := Height - pad - 12
	labelY := numBase - 50
	drawStats(img, fc.label, fc.num, pad, labelY, numBase, Width-2*pad, []stat{
		{"TOTAL TOKENS", fmtScale(c.TotalTokens)},
		{"MESSAGES", fmtScale(int64(c.MessageCount))},
		{"QUALITY", grade},
		{"DURATION", dur},
	})

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawSessionActivity paints the session's activity strip: a fixed grid of cells, one per
// bucket the store aggregated the session's usage into, each cell's intensity stepped by that
// bucket's token volume. The grid geometry is fixed (activityRows rows, activityCols columns),
// so the slice of time each cell covers is set by the session's own length: a 100-minute
// session over activityCells cells makes each cell about 15 seconds, a day-long session makes
// each cell a few minutes. Cells fill row-major (left to right, top to bottom), so time reads
// as it flows. It reuses the overview heatmap's levels and sqrt ramp so the two cards share a
// palette. When the session has no span (buckets is nil), every cell stays at level 0 (an empty
// strip), which still frames the card in the family style rather than leaving a hole. buckets
// is the store's fixed-size histogram (length activityCells when a span exists), so the strip
// is drawn straight from it with no per-event work here.
func drawSessionActivity(img *image.NRGBA, buckets []int64, x, y int) {
	var max int64
	for _, v := range buckets {
		if v > max {
			max = v
		}
	}
	for i := 0; i < activityCells; i++ {
		var v int64
		if i < len(buckets) {
			v = buckets[i]
		}
		lvl := levelFor(v, max)
		cx := x + (i%activityCols)*(activityCell+activityGap)
		cy := y + (i/activityCols)*(activityCell+activityGap)
		fillRect(img, image.Rect(cx, cy, cx+activityCell, cy+activityCell), heatLevels[lvl])
	}
}

// GenerateSession renders the preview card for one published session, stores it in the
// cache, and returns the encoded PNG so the caller can serve the very bytes it just
// rendered without a second read. It is the session mirror of Generate and GenerateProject.
//
// It reads every input the card draws from through one store call (SessionCard), a single
// repeatable-read snapshot: the project identity, the rollups, the span, the gated grade, and
// the activity strip pre-bucketed to the card's fixed cell count all come from one instant. The
// figures and the strip were previously stitched from the handler's session-detail read and a
// separate activity read, which an append or reparse between could tear; one snapshot removes
// that seam. A session card is a single session (not a cross-session aggregate), and a single
// session is rebuilt atomically during a reparse, so the snapshot never sees a half-built row
// and needs no reparse-lock gate the aggregate cards take.
//
// headingFor turns the card's project identity into the heading the page's <h1> shows, injected
// so this package stays free of the web view layer (the handler passes a closure over
// web.ProjectLabel). now stamps the stored card so a slower render that read older rollups cannot
// overwrite a fresher one (see PutSessionOGImage); it does not enter the render, which is a pure
// function of the session snapshot.
func GenerateSession(ctx context.Context, st *store.Store, sessionID int64, headingFor func(store.SessionCard) string, now time.Time) ([]byte, error) {
	card, found, err := st.SessionCard(ctx, sessionID, activityCells)
	if err != nil {
		return nil, fmt.Errorf("loading session card data for session %d: %w", sessionID, err)
	}
	if !found {
		return nil, fmt.Errorf("session %d not found for card render", sessionID)
	}
	png, err := RenderSession(headingFor(card), card)
	if err != nil {
		return nil, fmt.Errorf("rendering session card for session %d: %w", sessionID, err)
	}
	wrote, err := st.PutSessionOGImage(ctx, sessionID, png, now)
	if err != nil {
		return nil, fmt.Errorf("storing session card for session %d: %w", sessionID, err)
	}
	if !wrote {
		// A concurrent render stamped later already won the cache, so the guarded upsert
		// skipped ours. Return that canonical card rather than our own bytes: the served
		// image must equal what the cache holds, or two fetches of the same URL could unfurl
		// different pictures.
		canonical, err := st.SessionOGImage(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("reloading canonical session card for session %d: %w", sessionID, err)
		}
		return canonical.PNG, nil
	}
	return png, nil
}

// newFace builds one font face at the given point size, the shared one-liner the card
// face builders use instead of repeating the FaceOptions literal.
func newFace(f *opentype.Font, size float64) (font.Face, error) {
	return opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
}
