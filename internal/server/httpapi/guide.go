package httpapi

import (
	"net/http"

	"github.com/jssblck/akari/internal/guide"
)

// The user guide is public: a logged-out visitor and a coding agent must both be
// able to read it, so these handlers carry no auth gate. It is static content
// independent of the parsed projection, so it is not behind the reparse gate
// either. The HTML chapters render in the embedded React app (handleGuideRoute
// in app_shell.go serves the shell); these handlers cover the raw Markdown at
// /guide/<slug>.md and the two llms endpoints, which serve the machine-readable
// index and the whole guide as one file.

// serveGuideRaw serves a chapter's raw Markdown: the representation agents and
// crawlers probe for and the exact text "Copy page" copies.
func (s *Server) serveGuideRaw(w http.ResponseWriter, r *http.Request, slug string) {
	c, ok := guide.Lookup(slug)
	if !ok {
		http.NotFound(w, r)
		return
	}
	raw, err := c.Raw()
	if err != nil {
		http.Error(w, "Could not load the guide.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(raw))
}

// handleLLMsTxt serves the llms.txt discovery index (https://llmstxt.org): the
// chapters, each linked to its raw Markdown, so an agent learns the guide's shape
// in one fetch.
func (s *Server) handleLLMsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(guide.LLMsTxt(s.absURL(r, ""))))
}

// handleLLMsFullTxt serves llms-full.txt: every chapter concatenated in reading
// order, so an agent ingests the whole guide in a single request.
func (s *Server) handleLLMsFullTxt(w http.ResponseWriter, r *http.Request) {
	body, err := guide.LLMsFullTxt(s.absURL(r, ""))
	if err != nil {
		http.Error(w, "Could not load the guide.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(body))
}
