package web

// The templ templates in this package compile to Go through `templ generate`,
// which writes a sibling <name>_templ.go for every <name>.templ. Those generated
// files are deliberately NOT committed: they are large, machine-written, and
// regenerated wholesale on any template edit, so committing them made every UI
// change collide on shifting generated offsets. They are gitignored
// (internal/server/web/*_templ.go) and regenerated on every build instead, in
// CI, in Docker, and locally via `make generate` (which runs `go generate ./...`).
// The .templ files are the sole source of truth.
//
// Because the generated files are gitignored, the Go toolchain's vcs.modified
// flag never sees them, so a plain `go build` reports a clean version (no
// "-dirty" from codegen); release builds stamp the tag through ldflags regardless.
//
//go:generate go tool templ generate
