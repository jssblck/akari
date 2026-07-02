package httpapi

import (
	"net/http"
	"net/url"

	"github.com/jssblck/akari/internal/server/web"
)

// noticeCookie carries the one-shot success banner (see layout.templ's
// noticeBanner) across the redirect that follows an action POST: "Published",
// "Session deleted", and the like. It is a plain terse string, not a sentence,
// matching the exact copy each handler passes to setNotice.
const noticeCookie = "akari_notice"

// noticeMaxLen bounds the cookie value read back from the client. The cookie is
// only ever written by setNotice with a short literal, but the value round-trips
// through the browser and is client-tamperable in transit, so the read side caps
// and validates it independently of what any handler ever sends.
const noticeMaxLen = 80

// setNotice stores a one-shot success message in a short-lived, root-scoped
// cookie, read and cleared by the next page render (see withNotice). Root-scoped
// (unlike the account page's setFlash/readFlash, which are path-scoped to
// /account) because a notice is set by an action on one page (a session, the
// overview) and read back on whatever page the redirect lands on. It honors the
// same Secure setting as the session cookie: nothing in a notice is secret, but
// consistent cookie hygiene avoids a plaintext cookie policy exception to explain.
func (s *Server) setNotice(w http.ResponseWriter, notice string) {
	http.SetCookie(w, &http.Cookie{
		Name:     noticeCookie,
		Value:    url.QueryEscape(notice),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.Cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60,
	})
}

// withNotice reads and clears the notice cookie, then stashes its text in the
// render context (web.WithNotice) for the authed layout to show once. It mirrors
// withLocation: every HTML render path runs through render(), so wrapping the
// context there covers every page in one place, and clearing the cookie here
// (rather than only on the account page, as readFlash does) means a notice never
// survives a second render regardless of which page the redirect landed on.
//
// A present-but-invalid value (too long, or carrying a non-printable byte a
// tampered cookie could smuggle in) is dropped rather than shown: the cookie is
// client-tamperable, and the banner renders as a plain text node either way, so
// the validation here is a length and character-class bound, not an XSS guard.
func withNotice(w http.ResponseWriter, r *http.Request) *http.Request {
	c, err := r.Cookie(noticeCookie)
	if err != nil {
		return r
	}
	http.SetCookie(w, &http.Cookie{Name: noticeCookie, Value: "", Path: "/", MaxAge: -1})
	v, err := url.QueryUnescape(c.Value)
	if err != nil || !validNotice(v) {
		return r
	}
	return r.WithContext(web.WithNotice(r.Context(), v))
}

// validNotice bounds a notice string to a short line of printable text: no
// control characters (a raw newline or escape could otherwise distort the page
// around the banner even though templ escapes the text node itself), and capped
// well beyond the longest literal any handler passes.
func validNotice(v string) bool {
	if v == "" || len(v) > noticeMaxLen {
		return false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
