package selfupdate

import "golang.org/x/mod/semver"

// UpToDate compares the running version against the latest release tag.
//
// upToDate is true when current is the same as, or newer than, latest (so a
// build running a prerelease ahead of the latest stable is not told to
// downgrade). comparable is false when current is not a valid semver tag (a
// development build stamped with a commit SHA or "dev"), in which case upToDate
// is meaningless and callers should treat the build as updatable.
func UpToDate(current, latest string) (upToDate, comparable bool) {
	if !semver.IsValid(current) || !semver.IsValid(latest) {
		return false, false
	}
	return semver.Compare(current, latest) >= 0, true
}
