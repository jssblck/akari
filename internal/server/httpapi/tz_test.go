package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/web"
)

// validTZName gates what reaches LoadLocation: a real IANA name passes, and anything
// oversized or outside the zone-name alphabet is rejected before it can touch the
// filesystem-backed lookup.
func TestValidTZName(t *testing.T) {
	valid := []string{
		"UTC",
		"America/Los_Angeles",
		"America/Argentina/Buenos_Aires",
		"Etc/GMT+3",
		"Etc/GMT-14",
	}
	for _, n := range valid {
		if !validTZName(n) {
			t.Errorf("validTZName(%q) = false, want true", n)
		}
	}

	invalid := []string{
		"",                        // absent
		"America/New York",        // space
		"../etc/passwd",           // path traversal
		"Europe/London; rm -rf /", // injection-shaped
		"Zone\x00Null",            // control byte
		string(make([]byte, 65)),  // over the length cap
		"日本/東京",                   // non-ASCII
	}
	for _, n := range invalid {
		if validTZName(n) {
			t.Errorf("validTZName(%q) = true, want false", n)
		}
	}
}

// loadLocation resolves a valid zone name and falls back to UTC for a bad name or one
// the tzdata does not know, never returning nil.
func TestLoadLocation(t *testing.T) {
	if got := loadLocation("bogus name!"); got != time.UTC {
		t.Errorf("loadLocation(bad) = %v, want UTC", got)
	}
	if got := loadLocation("Definitely/NotAZone"); got != time.UTC {
		t.Errorf("loadLocation(unknown) = %v, want UTC", got)
	}
	loc := loadLocation("America/Los_Angeles")
	if loc == nil {
		t.Fatal("loadLocation returned nil")
	}
	if loc == time.UTC {
		t.Skip("America/Los_Angeles unavailable in this build; tzdata not embedded")
	}
	if loc.String() != "America/Los_Angeles" {
		t.Errorf("loadLocation = %q, want America/Los_Angeles", loc.String())
	}
	// A second call hits the cache and returns the identical pointer.
	if again := loadLocation("America/Los_Angeles"); again != loc {
		t.Error("loadLocation should return the cached location on the second call")
	}
}

// locationFrom reads the tz cookie off a request, resolving a valid zone and falling
// back to UTC when the cookie is absent or holds a value that does not resolve.
func TestLocationFrom(t *testing.T) {
	noCookie := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := locationFrom(noCookie); got != time.UTC {
		t.Errorf("locationFrom(no cookie) = %v, want UTC", got)
	}

	bad := httptest.NewRequest(http.MethodGet, "/", nil)
	bad.AddCookie(&http.Cookie{Name: tzCookie, Value: "Not/AZone"})
	if got := locationFrom(bad); got != time.UTC {
		t.Errorf("locationFrom(bad cookie) = %v, want UTC", got)
	}

	good := httptest.NewRequest(http.MethodGet, "/", nil)
	good.AddCookie(&http.Cookie{Name: tzCookie, Value: "America/Los_Angeles"})
	loc := locationFrom(good)
	if loc == time.UTC {
		t.Skip("America/Los_Angeles unavailable in this build; tzdata not embedded")
	}
	if loc.String() != "America/Los_Angeles" {
		t.Errorf("locationFrom(good cookie) = %q, want America/Los_Angeles", loc.String())
	}
}

// withLocation wires the resolved zone into the context the component renders with, so
// web.Loc reads it back on the render path.
func TestWithLocation(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: tzCookie, Value: "America/Los_Angeles"})
	r = withLocation(r)
	loc := web.Loc(r.Context())
	if loc == time.UTC {
		t.Skip("America/Los_Angeles unavailable in this build; tzdata not embedded")
	}
	if loc.String() != "America/Los_Angeles" {
		t.Errorf("web.Loc after withLocation = %q, want America/Los_Angeles", loc.String())
	}

	// A request with no cookie renders against UTC.
	plain := withLocation(httptest.NewRequest(http.MethodGet, "/", nil))
	if web.Loc(plain.Context()) != time.UTC {
		t.Error("withLocation with no cookie should leave the render context at UTC")
	}
}
