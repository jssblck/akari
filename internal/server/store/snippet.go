package store

import "strings"

// squashSpaces collapses every run of whitespace (including newlines and tabs) to
// a single space and trims the ends, so a session title reads as one clean line
// rather than carrying the raw prompt's line breaks and indentation into a
// single-line row. It leaves the visible text otherwise intact; the display cap
// (titleCap in SQL) and the CSS ellipsis handle length.
func squashSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// snippetWindow is how many characters of context the search snippet shows,
// centered on the match. It is a soft target: word-boundary trimming may shorten
// each side slightly so the window does not start or end mid-word.
const snippetWindow = 160

// buildSnippet windows a matching message's content around the first
// case-insensitive occurrence of query, returning the trimmed text with ellipses
// where it was cut and the byte offsets of the match within that text. The window
// is centered on the match and trimmed to word boundaries so it does not begin or
// end mid-word; when the source is shorter than the window it is returned whole.
//
// The offsets are byte positions into the returned Text (after any leading
// ellipsis is prepended), so a renderer can split Text into before/match/after and
// wrap only the matched run without re-scanning. A content that does not actually
// contain the query (which should not happen, since the caller only builds a
// snippet for a matched row) yields a zero-value snippet rather than a wrong window.
func buildSnippet(content, query string) SearchSnippet {
	if content == "" || query == "" {
		return SearchSnippet{}
	}
	// squash first so the snippet reads as one line and the offsets are into the
	// same normalized text the template renders.
	content = squashSpaces(content)
	idx := indexFold(content, query)
	if idx < 0 {
		return SearchSnippet{}
	}
	matchLen := len(query)

	// The window is centered on the match: aim to show as much context before as
	// after, but never let either side push the total past the window budget.
	extra := snippetWindow - matchLen
	if extra < 0 {
		extra = 0
	}
	before := extra / 2
	start := idx - before
	if start < 0 {
		start = 0
	}
	end := start + snippetWindow
	if end > len(content) {
		end = len(content)
		// Pull the start back if we hit the tail, so a match near the end still shows
		// a full window of leading context rather than a short tail-only snippet.
		if start > end-snippetWindow {
			start = end - snippetWindow
		}
		if start < 0 {
			start = 0
		}
	}

	leadCut := start > 0
	trailCut := end < len(content)
	// Trim to word boundaries so the window does not begin or end mid-word, but only
	// when that edge was actually cut (a natural start/end needs no trimming).
	if leadCut {
		if adj := nextWordStart(content, start, idx); adj <= idx {
			start = adj
		}
	}
	if trailCut {
		if adj := prevWordEnd(content, end, idx+matchLen); adj >= idx+matchLen {
			end = adj
		}
	}

	text := content[start:end]
	matchStart := idx - start
	matchEnd := matchStart + matchLen

	// Apply ellipses last, shifting the match offsets by any prepended lead so they
	// stay exact byte positions into the returned Text.
	const ellipsis = "…"
	if leadCut {
		text = ellipsis + text
		shift := len(ellipsis)
		matchStart += shift
		matchEnd += shift
	}
	if trailCut {
		text += ellipsis
	}
	return SearchSnippet{Text: text, MatchStart: matchStart, MatchEnd: matchEnd}
}

// indexFold reports the byte index of the first case-insensitive occurrence of
// substr in s, or -1. It lowercases both sides for the search; the returned index
// is into the original s, valid because ASCII-lowercasing does not change byte
// length (and the callers' content is UTF-8 where non-ASCII bytes are unaffected
// by ToLower's byte-length-preserving mapping for the Latin range this matches).
func indexFold(s, substr string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(substr))
}

// nextWordStart returns the index of the first character after a run of spaces at
// or after from, so a lead-trimmed window begins at a word boundary. It never
// advances past the match (cap), so a match glued to the window edge is never
// clipped. content is single-spaced (squashSpaces), so a boundary is a space.
func nextWordStart(content string, from, cap int) int {
	i := from
	// Advance to the next space, then past it, so we start at a fresh word.
	for i < cap && content[i] != ' ' {
		i++
	}
	for i < cap && content[i] == ' ' {
		i++
	}
	if i > cap {
		return from
	}
	return i
}

// prevWordEnd returns the index just past the last full word at or before to, so a
// trail-trimmed window ends at a word boundary. It never retreats past the match
// end (floor), so the matched run is never clipped.
func prevWordEnd(content string, to, floor int) int {
	i := to
	for i > floor && content[i-1] != ' ' {
		i--
	}
	for i > floor && content[i-1] == ' ' {
		i--
	}
	if i < floor {
		return to
	}
	return i
}
