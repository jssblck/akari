package httpapi

import (
	"net/http"
	"sync"
	"time"

	"github.com/jssblck/akari/internal/server/web"
)

// tzCookie is the cookie a tiny inline script in the page head sets to the
// viewer's IANA timezone name (see layout.templ). The server reads it to render
// every stamp and day heading in the reader's own zone, falling back to UTC when
// it is absent or unparseable. The cookie is advisory: it only changes how times
// display, so a bad value degrades to UTC rather than erroring.
const tzCookie = "tz"

// locCache memoizes time.LoadLocation across requests. LoadLocation reads and
// parses the zoneinfo database on every call, so a shared page under many viewers
// would re-parse the same handful of zones on every render. The set of distinct
// zones is tiny and bounded by the real IANA names, so an unbounded sync.Map is
// safe here. validTZName gates every key first, so a crafted value cannot grow the
// map without bound.
var locCache sync.Map // string -> *time.Location

// locationFrom resolves the *time.Location for a request from its tz cookie,
// falling back to time.UTC when the cookie is absent, malformed, or names a zone
// the tzdata does not know. It never returns nil: every render path can format
// against the result unconditionally.
func locationFrom(r *http.Request) *time.Location {
	c, err := r.Cookie(tzCookie)
	if err != nil {
		return time.UTC
	}
	return loadLocation(c.Value)
}

// loadLocation validates a zone name and resolves it through the process-wide
// cache, returning UTC on any failure. It is split from locationFrom so the
// validation and caching are unit-testable without constructing a request.
func loadLocation(name string) *time.Location {
	if !validTZName(name) {
		return time.UTC
	}
	if v, ok := locCache.Load(name); ok {
		return v.(*time.Location)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		// Misses are deliberately not cached: validTZName bounds the alphabet, not
		// the keyspace, so caching failures would let a crafted cookie grow the map
		// without bound. Only real zone names ever enter the cache.
		return time.UTC
	}
	locCache.Store(name, loc)
	return loc
}

// validTZName reports whether a string is shaped like an IANA zone name, before it
// is handed to LoadLocation. IANA names are short (the longest real ones sit well
// under 64) and drawn from a narrow alphabet: letters, digits, and the separators
// that appear in names like "America/Argentina/Buenos_Aires" and "Etc/GMT+3". The
// gate keeps a crafted cookie from reaching the filesystem-backed LoadLocation and
// bounds what the cache can hold.
func validTZName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, ch := range name {
		switch {
		case ch >= 'A' && ch <= 'Z',
			ch >= 'a' && ch <= 'z',
			ch >= '0' && ch <= '9',
			ch == '_' || ch == '/' || ch == '+' || ch == '-':
		default:
			return false
		}
	}
	return true
}

// withLocation stashes the request's resolved timezone in the context the
// component renders with, so the templ formatting helpers (web.FmtTime and the
// rest, which take ctx) localize every stamp without threading a *time.Location
// through every signature. Every HTML render path runs through render(), so
// wrapping the context there covers the authed pages, the public pages, and the
// error pages in one place.
func withLocation(r *http.Request) *http.Request {
	return r.WithContext(web.WithLoc(r.Context(), locationFrom(r)))
}
