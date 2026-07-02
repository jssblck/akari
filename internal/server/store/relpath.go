package store

import "strings"

// sessionRelPath reduces an absolute tool-call file_path to the form relative to the session's
// working directory, so churn (and any per-project file view) can aggregate one repo file across
// the git worktrees it was edited from. A worktree's cwd differs per checkout
// (C:\...\worktrees\akari\foo vs C:\...\projects\akari), so the absolute path fragments the same
// repo file into separate rows; stripping the cwd prefix yields a worktree-invariant key that,
// paired with the project (which already collapses worktrees on the canonical remote), collapses
// those rows back together.
//
// It returns ok=false, and the caller stores NULL, whenever a stable relative form cannot be
// derived: an empty path, an absolute path with no cwd to anchor it, or a path that does not sit
// under the cwd (a file edited outside the workspace stays absolute-only rather than being forced
// into a misleading relative key). It never returns ("", true): a path equal to the cwd has no
// remainder to key on and reads as not-ok, so an ok result always carries a non-empty relative
// path.
//
// The match is deliberately half case-insensitive. On Windows the drive letter commonly differs
// in case between the announced cwd and the tool input (one arrives lowercased, the other not),
// so the leading `X:` is compared case-insensitively; the rest of the path is compared exactly,
// because path segments are case-sensitive on the systems akari targets and folding them would
// collapse genuinely distinct files.
func sessionRelPath(cwd, filePath string) (string, bool) {
	if filePath == "" {
		return "", false
	}
	fp := normalizeSeparators(filePath)

	// A path the agent already reported relative to the workspace needs no cwd to anchor it: trim
	// a leading "./" and it is already the key. This also covers agents that emit repo-relative
	// paths directly, independent of whether a cwd is known.
	if !isAbsPath(fp) {
		rel := strings.TrimPrefix(fp, "./")
		if rel == "" {
			return "", false
		}
		return rel, true
	}

	// From here the path is absolute, so it can only be made relative against a known cwd.
	if cwd == "" {
		return "", false
	}
	base := strings.TrimRight(normalizeSeparators(cwd), "/")
	if base == "" {
		return "", false
	}

	if !hasPathPrefix(fp, base) {
		return "", false
	}
	rel := fp[len(base):]
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		// The path is the cwd itself: no file remainder to key on.
		return "", false
	}
	return rel, true
}

// normalizeSeparators rewrites Windows backslashes to forward slashes so a path from either
// platform compares and stores in one canonical form.
func normalizeSeparators(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}

// isAbsPath reports whether a separator-normalized path is absolute: a POSIX path rooted at "/"
// or a Windows path carrying a `X:` drive-letter prefix.
func isAbsPath(p string) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}
	return hasDriveLetter(p)
}

// hasDriveLetter reports whether a normalized path starts with a Windows drive-letter prefix
// (a single ASCII letter followed by ':', as in "C:/Users/...").
func hasDriveLetter(p string) bool {
	return len(p) >= 2 && isASCIILetter(p[0]) && p[1] == ':'
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// hasPathPrefix reports whether fp sits under base: fp equals base, or fp continues past base at a
// path boundary (base + "/..."). Both are already separator-normalized and base is
// trailing-slash-trimmed. A Windows drive letter is matched case-insensitively (announce and tool
// input frequently disagree on its case) while the remaining segments match exactly. The boundary
// check rules out a sibling that merely shares a name prefix ("/repo-two" is not under "/repo").
func hasPathPrefix(fp, base string) bool {
	if len(fp) < len(base) {
		return false
	}
	head, rest := fp[:len(base)], fp[len(base):]
	if hasDriveLetter(base) && hasDriveLetter(head) {
		// Compare the drive letter case-insensitively, the rest exactly.
		if !strings.EqualFold(head[:2], base[:2]) || head[2:] != base[2:] {
			return false
		}
	} else if head != base {
		return false
	}
	return rest == "" || strings.HasPrefix(rest, "/")
}
