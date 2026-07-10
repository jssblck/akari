package httpapi

import "net/http"

// withStyledNotFound preserves ServeMux's method-aware 405 responses while
// replacing only its bare unmatched-path 404 with the application's error page.
// A methodless catch-all cannot make that distinction: it claims every method and
// turns a wrong-method request into a misleading 404.
func withStyledNotFound(mux *http.ServeMux, notFound http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		if pattern != "" || pathMatchesAnotherMethod(mux, r) {
			// ServeHTTP performs the match again and installs wildcard values on
			// the request before dispatching; Handler alone only reports a match.
			mux.ServeHTTP(w, r)
			return
		}
		notFound(w, r)
	})
}

func pathMatchesAnotherMethod(mux *http.ServeMux, r *http.Request) bool {
	for _, method := range []string{
		http.MethodGet,
		http.MethodHead,
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodOptions,
		http.MethodConnect,
		http.MethodTrace,
	} {
		if method == r.Method {
			continue
		}
		probe := r.Clone(r.Context())
		probe.Method = method
		if _, pattern := mux.Handler(probe); pattern != "" {
			return true
		}
	}
	return false
}
