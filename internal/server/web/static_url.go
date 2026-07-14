package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
	"sync"

	"github.com/a-h/templ"
)

// StaticURL fingerprints an embedded asset so a newly deployed binary cannot reuse
// client code cached from an older HTML/data contract. The asset bytes are immutable
// for the process lifetime, so build the lookup once and keep templates to a map read.
// The render context carries the deployment's external path prefix (see BasePath),
// which lands in front of the rooted asset path.
func StaticURL(ctx context.Context, path string) templ.SafeURL {
	url, ok := staticURLs()[strings.TrimPrefix(path, "/")]
	if !ok {
		panic("web: static asset not embedded: " + path)
	}
	return templ.SafeURL(BasePath(ctx)) + url
}

var staticURLs = sync.OnceValue(func() map[string]templ.SafeURL {
	urls := make(map[string]templ.SafeURL)
	err := fs.WalkDir(Static, "static", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := Static.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(contents)
		name := strings.TrimPrefix(path, "static/")
		version := hex.EncodeToString(sum[:6])
		urls[name] = templ.SafeURL("/static/" + name + "?v=" + version)
		return nil
	})
	if err != nil {
		panic("web: fingerprint static assets: " + err.Error())
	}
	return urls
})
