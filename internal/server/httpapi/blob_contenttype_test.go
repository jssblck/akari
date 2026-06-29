package httpapi

import "testing"

// TestSafeBlobContentType pins which stored media types serve under their real type
// and which are forced to opaque bytes. Tool bodies (json, text) and inert raster
// images render inline; anything else (notably image/svg+xml, which can carry script,
// and unknown types) is served as octet-stream so a stored body can never be
// interpreted as active content.
func TestSafeBlobContentType(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"application/json", "application/json; charset=utf-8"},
		{"text/plain", "text/plain; charset=utf-8"},
		{"", "text/plain; charset=utf-8"},
		{"image/png", "image/png"},
		{"image/jpeg", "image/jpeg"},
		{"image/gif", "image/gif"},
		{"image/webp", "image/webp"},
		// Active or unknown content must never serve under a renderable type.
		{"image/svg+xml", "application/octet-stream"},
		{"text/html", "application/octet-stream"},
		{"application/octet-stream", "application/octet-stream"},
		{"image/tiff", "application/octet-stream"},
	}
	for _, c := range cases {
		if got := safeBlobContentType(c.in); got != c.want {
			t.Errorf("safeBlobContentType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
