package web

import (
	"strings"

	"github.com/jssblck/akari/internal/quality"
)

// StripPromptPreamble reduces a session's first-message title to the part a reader
// cares about, stripping the machine-generated preamble a coding-agent harness wraps
// around the human's words. It works on the already-single-spaced title (squashSpaces
// ran in the store), so every preamble sits on one line.
//
// It only ever clarifies: it returns a slash command as its own name, strips leading
// harness wrapper blocks, and jumps past an embedded AGENTS.md instruction dump to the
// task that follows. When it cannot find a cleaner form it returns the original, so a
// title never gets worse than it was, and it never returns something longer than the
// input.
func StripPromptPreamble(title string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		return title
	}
	// A slash-command envelope is the clearest possible title: surface the command
	// itself ("/tag-release") rather than its wrapper tags or expanded body.
	if name := slashCommandName(t); name != "" {
		return name
	}
	// Drop leading wrapper blocks (local-command caveats, injected reminders) that
	// precede the real content.
	t = stripLeadingTagBlocks(t)
	// A Codex session prepends the whole AGENTS.md as "# AGENTS.md instructions
	// <INSTRUCTIONS> ... </INSTRUCTIONS>" before the task; jump past the close tag to
	// the task itself when one follows.
	if strings.HasPrefix(t, "# AGENTS.md instructions") {
		if i := strings.Index(t, "</INSTRUCTIONS>"); i >= 0 {
			if rest := strings.TrimSpace(t[i+len("</INSTRUCTIONS>"):]); rest != "" {
				t = rest
			}
		}
	}
	if t = strings.TrimSpace(t); t == "" {
		return title
	}
	return t
}

// slashCommandName pulls the command out of a slash-command envelope
// (<command-name>/foo</command-name>, optionally followed by <command-args>bar</...>),
// returning "/foo bar" or "" when the title is not a command invocation.
func slashCommandName(t string) string {
	name := strings.TrimSpace(tagContent(t, "command-name"))
	if name == "" {
		return ""
	}
	if args := strings.TrimSpace(tagContent(t, "command-args")); args != "" {
		return name + " " + args
	}
	return name
}

// harnessWrapperTags are the leading tag blocks a harness prepends to the first user
// message. Each wraps content that carries no task (a caveat about local commands, an
// injected system reminder, the raw command envelope), so a title strips them to reach
// the human's actual words.
var harnessWrapperTags = []string{
	"local-command-caveat",
	"local-command-stdout",
	"command-message",
	"command-name",
	"command-args",
	"system-reminder",
}

// stripLeadingTagBlocks repeatedly removes a leading <tag>...</tag> block for any known
// harness wrapper, so a title that opens with one or several of them advances to the
// first line of real content.
func stripLeadingTagBlocks(t string) string {
	for {
		t = strings.TrimSpace(t)
		advanced := false
		for _, tag := range harnessWrapperTags {
			open, close := "<"+tag+">", "</"+tag+">"
			if !strings.HasPrefix(t, open) {
				continue
			}
			if k := strings.Index(t, close); k >= 0 {
				t = t[k+len(close):]
				advanced = true
				break
			}
		}
		if !advanced {
			return strings.TrimSpace(t)
		}
	}
}

// tagContent returns the text between the first <tag> and its matching </tag>, or "" when
// the pair is absent. It is a deliberately small extractor for the flattened one-line
// titles the store hands up, not a general XML parser.
func tagContent(s, tag string) string {
	open, close := "<"+tag+">", "</"+tag+">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// RowGradeClass is the CSS band for a feed row's grade chip, matching the session
// Quality tile's report-card palette. The tiering lives in quality.GradeBand so the
// feed chip cannot drift from the Quality tile and Insights bars; this only maps the
// band onto the q-* class. An absent grade returns "" so the caller renders no chip.
func RowGradeClass(grade *string) string {
	if grade == nil {
		return ""
	}
	return "q-" + quality.GradeBand(*grade)
}

// RowOutcomeNote returns a short outcome word to flag on a feed row, but only for the
// outcomes worth a glance: abandoned and errored. A completed or unknown outcome returns
// "" so the common, healthy row stays quiet and the failures stand out.
func RowOutcomeNote(outcome string) string {
	switch outcome {
	case "abandoned":
		return "abandoned"
	case "errored":
		return "errored"
	default:
		return ""
	}
}
