package httpapi

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openAPIDocument []byte

func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/vnd.oai.openapi+json;version=3.1")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(openAPIDocument)
}
