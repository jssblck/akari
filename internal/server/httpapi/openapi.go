package httpapi

import (
	"bytes"
	_ "embed"
	"net/http"
	"sync"
)

//go:embed openapi.json
var openAPIDocument []byte

// openAPIServerMarker is the embedded document's server entry. Under a path
// prefix it must name the prefixed mount, because a root-relative "/" resolves
// against the origin, not against where the document was fetched from.
var openAPIServerMarker = []byte(`"url": "/"`)

// openAPIByPrefix memoizes the rewritten document per prefix, capped like the
// frontend index cache so header-asserted prefixes cannot grow it unbounded.
var (
	openAPIMu       sync.Mutex
	openAPIByPrefix = map[string][]byte{}
)

const openAPICacheCap = 32

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	document := openAPIDocument
	if prefix := requestPrefix(r); prefix != "" {
		openAPIMu.Lock()
		cached, ok := openAPIByPrefix[prefix]
		openAPIMu.Unlock()
		if !ok {
			// The rewrite is a plain byte substitution; if the generated
			// document ever stops carrying the marker, fail loudly instead of
			// serving a spec whose server URL points at the origin root.
			if !bytes.Contains(openAPIDocument, openAPIServerMarker) {
				writeError(w, http.StatusInternalServerError, "openapi server marker missing")
				return
			}
			cached = bytes.Replace(openAPIDocument, openAPIServerMarker, []byte(`"url": "`+prefix+`/"`), 1)
			openAPIMu.Lock()
			if len(openAPIByPrefix) < openAPICacheCap {
				openAPIByPrefix[prefix] = cached
			}
			openAPIMu.Unlock()
		}
		document = cached
	}
	w.Header().Set("Content-Type", "application/vnd.oai.openapi+json;version=3.1")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	// The body varies with the resolved prefix; tell shared caches which header
	// drove the variation when the prefix is request-asserted.
	if s.Cfg.PrefixHeader != "" {
		w.Header().Add("Vary", s.Cfg.PrefixHeader)
	}
	_, _ = w.Write(document)
}
