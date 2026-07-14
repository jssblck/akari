package web

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/jssblck/akari/internal/server/store"
)

// SelectedUserIDs parses the overview's repeated ?user= ids against the known
// accounts, keeping only ids that name a real user and returning them in the
// users-list order. A tampered, stale, or non-numeric id silently drops out, and
// the stable order keeps the collapsed chips from reshuffling between requests.
func SelectedUserIDs(raw []string, users []store.User) []int64 {
	if len(raw) == 0 {
		return nil
	}
	want := map[int64]bool{}
	for _, v := range raw {
		if id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	var out []int64
	for _, u := range users {
		if want[u.ID] {
			out = append(out, u.ID)
		}
	}
	return out
}

// DefaultSessionLimit is the global feed's page size, the fixed slice each request reads.
// "Show more" does not grow this: the client passes a keyset cursor and appends the next
// page of the same size, so depth is unbounded and the page cost stays flat.
const DefaultSessionLimit = 100

// PublicPath is the plain-string public URL, shown to the owner as the shareable
// link to copy.
func PublicPath(publicID string) string { return "/s/" + publicID }

// PublicOverviewPath is the plain-string path of a user's public usage overview,
// rooted at /u/<username>. The username is path-escaped so an unusual character
// cannot break the URL or escape the segment.
func PublicOverviewPath(username string) string { return "/u/" + url.PathEscape(username) }

// PublicOverviewOGPath is the path of the Open Graph preview card for a user's
// published overview, the /u/<username>/og.png the page advertises as og:image and
// the route serves the rendered PNG from. It is PublicOverviewPath with the card
// suffix, so the page tag and the route stay one definition rather than two string
// literals that could drift.
func PublicOverviewOGPath(username string) string { return PublicOverviewPath(username) + "/og.png" }

// PublicProjectPath is the plain-string path of a project's public usage overview,
// rooted at /p/<id>.
func PublicProjectPath(id int64) string { return fmt.Sprintf("/p/%d", id) }

// PublicProjectOGPath is the path of the Open Graph preview card for a project's
// published overview, the /p/<id>/og.png the page advertises as og:image and the
// route serves the rendered PNG from. Built off PublicProjectPath so the tag and the
// route share one definition.
func PublicProjectOGPath(id int64) string { return PublicProjectPath(id) + "/og.png" }

// PublicSessionOGPath is the path of the Open Graph preview card for a published
// session, the /s/<public_id>/og.png the page advertises as og:image and the route
// serves the rendered PNG from. Built off PublicPath so the tag and the route share
// one definition.
func PublicSessionOGPath(publicID string) string { return PublicPath(publicID) + "/og.png" }
