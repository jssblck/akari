package web

import (
	"context"

	"github.com/a-h/templ"
)

// basePathCtxKey keys the external path prefix in the render context, the same
// seam Loc and Notice use: the httpapi layer resolves the prefix per request
// (config or a trusted proxy header) and stashes it at the render seam, so
// every templated href and asset link externalizes without each component
// threading it. An unexported type keeps the key from colliding with any other
// package's context values.
type basePathCtxKey struct{}

// WithBasePath returns a context carrying the external path prefix akari is
// served under for this request ("" for a root deployment), for the httpapi
// render path to attach before a component renders.
func WithBasePath(ctx context.Context, prefix string) context.Context {
	if prefix == "" {
		return ctx
	}
	return context.WithValue(ctx, basePathCtxKey{}, prefix)
}

// BasePath is the current render's external path prefix, or "" when the
// deployment serves from the origin root.
func BasePath(ctx context.Context) string {
	p, _ := ctx.Value(basePathCtxKey{}).(string)
	return p
}

// Href externalizes a rooted path for the current render: the base path plus
// the path. Every literal href, form action, and hx-get a template emits must
// pass through here, because the browser resolves them against the external
// URL space, not the server's stripped internal one. The prefix is validated
// at the trust boundary (config.NormalizePathPrefix), so the concatenation is
// safe to mark as a SafeURL.
func Href(ctx context.Context, path string) templ.SafeURL {
	return templ.SafeURL(BasePath(ctx) + path)
}
