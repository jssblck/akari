package httpapi

import (
	"errors"
	"net/http"

	"github.com/jssblck/akari/internal/server/requestbudget"
)

const requestBudgetRetryAfter = "1"

// admit wraps a complete expensive handler in the shared weighted budget. It is
// used where the whole operation has one resource lifetime, such as preview-card
// rendering and dynamic registration. Analytics pages acquire inside their shared
// snapshot refresh so cache hits consume no capacity.
func (s *Server) admit(class requestbudget.WorkClass, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.runAdmitted(w, r, class, func() { next(w, r) }) {
			return
		}
	}
}

// admitMCP budgets POST request parsing and tool execution without charging the
// long-lived GET event stream or cheap DELETE teardown. The same class will own
// the temporary-file spool introduced by #134.
func (s *Server) admitMCP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		s.admit(requestbudget.MCPSpool, next.ServeHTTP)(w, r)
	})
}

// runAdmitted holds class capacity only while work runs and releases it on every
// return or panic path. Caller cancellation is propagated without attempting to
// write a response to a connection that has already gone away.
func (s *Server) runAdmitted(w http.ResponseWriter, r *http.Request, class requestbudget.WorkClass, work func()) bool {
	release, err := s.budget.Acquire(r.Context(), class)
	if err != nil {
		if r.Context().Err() != nil {
			return false
		}
		if errors.Is(err, requestbudget.ErrWaitTimeout) {
			w.Header().Set("Retry-After", requestBudgetRetryAfter)
			http.Error(w, "server busy", http.StatusServiceUnavailable)
			return false
		}
		http.Error(w, "request admission failed", http.StatusInternalServerError)
		return false
	}
	defer release()
	work()
	return true
}
