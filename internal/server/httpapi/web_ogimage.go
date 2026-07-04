package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/jssblck/akari/internal/server/ogimage"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
)

// handlePublicOverviewOGImage serves the Open Graph preview card for a published
// overview at /u/<username>/og.png. The card is rendered lazily and cached: a
// request served a card younger than the TTL returns the cached bytes; a miss or an
// expired card renders a fresh one on demand, stores it, and serves that. So a burst
// of crawler fetches after a share costs one render, not one per fetch, and a card
// nobody shares is never rendered at all. An unpublished or unknown account 404s,
// matching how /u/<username> itself resolves.
func (s *Server) handlePublicOverviewOGImage(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	now := time.Now()

	// One query resolves the user, checks the public gate, and reads any cached card
	// together. Folding the public check into the card read keeps the serve atomic: a
	// split (resolve the user, then read the card) would leave a window where a
	// concurrent unpublish slips between the two steps and a card is served for an
	// overview that just went private.
	u, cached, found, err := s.Store.PublicOverviewCard(r.Context(), username)
	if err != nil {
		http.Error(w, "Could not load preview image.", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	haveCache := cached.PNG != nil
	if haveCache && now.Sub(cached.GeneratedAt) < s.ogCacheTTL() {
		s.writeOGImage(w, cached.PNG)
		return
	}

	// Cache miss or expired: render on demand through the per-user singleflight group,
	// re-confirm the overview is still public, then serve the fresh card, fall back to the
	// last good one, or fail. serveOGCard owns that shared tail. reparseAware is true: a
	// reparse rebuilding the projection makes a consistent snapshot impossible, so Generate
	// aborts with ErrReparseInProgress, a transient stale-or-404 rather than a logged
	// failure.
	s.serveOGCard(w, r,
		"overview og", fmt.Sprintf("user %d (%s)", u.ID, u.Username), true,
		func() ([]byte, error) { return s.renderOGImage(r.Context(), u, now) },
		func() ([]byte, bool, error) {
			_, latest, stillPublic, gateErr := s.Store.PublicOverviewCard(r.Context(), username)
			return latest.PNG, stillPublic, gateErr
		},
	)
}

// handlePublicProjectOGImage serves the Open Graph preview card for a published project
// overview at /p/<id>/og.png. It is the project mirror of handlePublicOverviewOGImage:
// the card is rendered lazily and cached, so a burst of crawler fetches after a share
// costs one render, not one per fetch, and a card nobody shares is never rendered.
// PublicProjectCard folds the public gate and the cached-card read into one query, so a
// concurrent unpublish cannot slip between resolving the project and reading its card.
// An unpublished or unknown id 404s, matching how /p/<id> itself resolves.
func (s *Server) handlePublicProjectOGImage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	now := time.Now()

	proj, cached, found, err := s.Store.PublicProjectCard(r.Context(), id)
	if err != nil {
		http.Error(w, "Could not load preview image.", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	haveCache := cached.PNG != nil
	if haveCache && now.Sub(cached.GeneratedAt) < s.ogCacheTTL() {
		s.writeOGImage(w, cached.PNG)
		return
	}

	// Cache miss or expired: render on demand through the per-project singleflight group,
	// re-confirm the project is still public, then serve the fresh card, fall back to the
	// last good one, or fail. serveOGCard owns that shared tail. reparseAware is true:
	// GenerateProject aborts with ErrReparseInProgress rather than caching a card built
	// from a half-rebuilt projection, a transient stale-or-404 rather than a logged
	// failure. The heading is the same title the /p/<id> page shows, passed in so the
	// ogimage package stays free of the web view layer.
	s.serveOGCard(w, r,
		"project og", fmt.Sprintf("project %d", id), true,
		func() ([]byte, error) {
			return s.renderProjectOGImage(r.Context(), id, web.ProjectTitle(proj), now)
		},
		func() ([]byte, bool, error) {
			_, latest, stillPublic, gateErr := s.Store.PublicProjectCard(r.Context(), id)
			return latest.PNG, stillPublic, gateErr
		},
	)
}

// renderProjectOGImage renders and caches one project's preview card through the
// per-project singleflight group, so concurrent misses for the same overview share a
// single render. It mirrors renderOGImage: the shared render runs under a bounded
// context detached from any single caller (so one dropped crawler connection cannot
// cancel it for the others), while each caller still waits on its own request context.
func (s *Server) renderProjectOGImage(ctx context.Context, projectID int64, heading string, now time.Time) ([]byte, error) {
	ch := s.ogProjectRender.DoChan(strconv.FormatInt(projectID, 10), func() (any, error) {
		renderCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ogRenderTimeout)
		defer cancel()
		return ogimage.GenerateProject(renderCtx, s.Store, projectID, heading, now)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.([]byte), nil
	}
}

// handlePublicSessionOGImage serves the Open Graph preview card for a published session
// at /s/<public_id>/og.png. Like the overview and project cards it is rendered lazily
// and cached, and PublicSessionCard folds the visibility gate and the cached-card read
// into one query so a concurrent unpublish cannot slip between them. An unpublished or
// unknown public id 404s, matching how /s/<public_id> resolves.
//
// The reparse handling is lighter than the aggregate cards. GenerateSession reads every
// card input in one repeatable-read snapshot, and a single session is rebuilt atomically
// during a reparse, so the render never sees a half-built session and needs no reparse-lock
// gate. The render is still skipped while a reparse runs (serve the last good card, else
// 404) so a card is not re-rendered from a session about to be rewritten, only to be
// superseded moments later; it is a courtesy, not a correctness gate. The check is
// best-effort, exactly as gatePublicParsed is: a reparse starting mid-render at worst
// re-renders a card that self-heals on the TTL.
func (s *Server) handlePublicSessionOGImage(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("public_id")
	now := time.Now()

	sessionID, cached, found, err := s.Store.PublicSessionCard(r.Context(), pid)
	if err != nil {
		http.Error(w, "Could not load preview image.", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	haveCache := cached.PNG != nil
	if haveCache && now.Sub(cached.GeneratedAt) < s.ogCacheTTL() {
		s.writeOGImage(w, cached.PNG)
		return
	}

	// Skip the render while a reparse rewrites the corpus (see the doc comment): serve the
	// last good card if we hold one, else 404 until the reparse ends and a later fetch
	// renders it.
	if s.worker.FleetStatus(r.Context()).InProgress {
		if haveCache {
			s.writeOGImage(w, cached.PNG)
			return
		}
		http.NotFound(w, r)
		return
	}

	// Render on demand, re-confirm the session is still public, then serve the fresh card,
	// fall back to the last good one, or fail. serveOGCard owns that shared tail.
	// reparseAware is false: GenerateSession reads every card input in one snapshot and
	// never aborts with ErrReparseInProgress (the up-front FleetStatus gate above stands in
	// for it), so every render error is a real failure, logged and stale-or-500, with no
	// reparse special case.
	s.serveOGCard(w, r,
		"session og", fmt.Sprintf("session %d", sessionID), false,
		func() ([]byte, error) { return s.renderSessionOGImage(r.Context(), sessionID, now) },
		func() ([]byte, bool, error) {
			_, latest, stillPublic, gateErr := s.Store.PublicSessionCard(r.Context(), pid)
			return latest.PNG, stillPublic, gateErr
		},
	)
}

// renderSessionOGImage renders and caches one session's preview card through the
// per-session singleflight group. GenerateSession reads every card input in one store
// snapshot inside the coalesced render, so a crawler burst on a cold cache runs one
// read-and-render rather than one per request. The heading closure turns the card's project
// identity into the label the page's heading shows, kept here so the ogimage package stays
// free of the web view layer.
func (s *Server) renderSessionOGImage(ctx context.Context, sessionID int64, now time.Time) ([]byte, error) {
	ch := s.ogSessionRender.DoChan(strconv.FormatInt(sessionID, 10), func() (any, error) {
		renderCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ogRenderTimeout)
		defer cancel()
		heading := func(c store.SessionCard) string {
			return web.ProjectLabel(c.ProjectKind, c.ProjectName, c.ProjectKey)
		}
		return ogimage.GenerateSession(renderCtx, s.Store, sessionID, heading, now)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.([]byte), nil
	}
}

// serveOGCard runs the render-recheck-serve tail the three public OG card handlers share.
// By the time it is called each handler has resolved its subject, served a fresh cache hit,
// and (for the session card) cleared the up-front reparse gate; what remains was near
// byte-identical across the three. The handler supplies two closures: render coalesces the
// on-demand render through its own singleflight group and returns the request context's
// error if the caller disconnected, and regate re-reads the publish gate, returning the
// last good card's bytes (nil if none), whether the subject is still public, and any real
// lookup error.
//
// The flow: bail quietly if the client already disconnected mid-render (nothing to serve,
// nothing broke, and the cancelled context would only fail the gate re-read below); re-read
// the gate so an unpublish during the render 404s instead of unfurling a card for a
// now-private subject, surfacing a real lookup error as a 500 rather than disguising it as a
// missing card; then serve the fresh card, or on failure fall back to the last good one,
// otherwise report the error. logPrefix and subject name the card in the two log lines,
// e.g. ("overview og", "user 5 (grace)").
//
// reparseAware is the one real difference between the callers. The overview and project
// renders abort with ogimage.ErrReparseInProgress when a reparse makes a consistent snapshot
// impossible; that is a transient, unlogged stale-or-404. The session render reads its
// inputs in one snapshot and never returns that error (its handler gates on the reparse up
// front instead), so it treats every render error as a real failure: logged, stale-or-500.
// Passing reparseAware=false routes ErrReparseInProgress through the default branch, keeping
// the session handler's exact behavior.
func (s *Server) serveOGCard(
	w http.ResponseWriter,
	r *http.Request,
	logPrefix, subject string,
	reparseAware bool,
	render func() ([]byte, error),
	regate func() (stalePNG []byte, stillPublic bool, err error),
) {
	png, genErr := render()

	// The client may have disconnected mid-render: render returns the request context's
	// error when it does. Nothing to serve and nothing broke, so return quietly (and skip
	// the gate re-read below, which that cancelled context would fail anyway) rather than
	// logging a spurious failure.
	if r.Context().Err() != nil {
		return
	}

	// Re-confirm the subject is still public before serving anything. One gated read does
	// double duty: it re-checks visibility and returns the canonical cached card the
	// stale-fallback branches serve. A real lookup error is distinct from a closed gate:
	// withhold the card either way, but surface the backend failure rather than disguising
	// it as a missing card.
	stalePNG, stillPublic, gateErr := regate()
	switch {
	case gateErr != nil:
		log.Printf("%s: public re-check for %s failed: %v", logPrefix, subject, gateErr)
		http.Error(w, "Could not load preview image.", http.StatusInternalServerError)
		return
	case !stillPublic:
		http.NotFound(w, r)
		return
	}

	switch {
	case genErr == nil:
		s.writeOGImage(w, png)
	case reparseAware && errors.Is(genErr, ogimage.ErrReparseInProgress):
		// A reparse blocked the fresh render. Serve the last good card if the gated re-read
		// still holds one, else 404 (transient, clears once the reparse ends).
		if stalePNG != nil {
			s.writeOGImage(w, stalePNG)
			return
		}
		http.NotFound(w, r)
	default:
		// A real render failure. Log it regardless of whether a stale card saves the
		// response: serving stale masks the failure from the crawler, but the refresh still
		// broke, and a persistently failing render must stay diagnosable rather than hiding
		// behind an ever-staler card. Then serve the last good card if we hold one (it beats
		// a 500 to a crawler), else report the error.
		log.Printf("%s: render for %s failed: %v", logPrefix, subject, genErr)
		if stalePNG != nil {
			s.writeOGImage(w, stalePNG)
			return
		}
		http.Error(w, "Could not load preview image.", http.StatusInternalServerError)
	}
}

// landingOGCacheMaxAge is the Cache-Control window for the homepage card at
// /og.png. The card is static per binary (it reads no parsed data), so it only
// changes on deploy: a full day of crawler caching is safe, and mirrors the
// "changes only on deploy" lifetime the overview card gets through its TTL.
const landingOGCacheMaxAge = 86400

// handleLandingOGImage serves the Open Graph preview card for the instance root
// ("/") at /og.png. Unlike the per-user overview card, it carries no account data
// (just the wordmark, the product headline, and a decorative band), so it is
// static per binary: ogimage.Landing renders it once and memoizes the bytes, and
// there is nothing to gate on a reparse or scope to a user. A render failure is a
// missing font asset in the binary, an internal error, not a 404.
func (s *Server) handleLandingOGImage(w http.ResponseWriter, r *http.Request) {
	png, err := ogimage.Landing()
	if err != nil {
		log.Printf("landing og: render failed: %v", err)
		http.Error(w, "Could not load preview image.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", landingOGCacheMaxAge))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// ogRenderTimeout bounds a single on-demand card render (its analytics snapshot, the
// raster, and the cache write). The render is detached from the request that triggers
// it so a dropped crawler connection cannot cancel it for the other waiters, so it
// needs its own deadline: without one a stuck query would pin the singleflight leader
// and every same-user waiter, and could stall shutdown. Rendering is normally
// sub-second, so 30s is a generous safety ceiling well above the expected duration.
const ogRenderTimeout = 30 * time.Second

// renderOGImage renders and caches one user's preview card through the per-user
// singleflight group, so concurrent misses for the same overview share a single
// render rather than each running the full year-window analytics and raster. The
// waiters receive the same bytes and error the leader produced; ogimage.Generate
// already reconciles a losing guarded write to the canonical cached card, so every
// caller here serves what the cache holds.
//
// The shared render runs under a bounded context detached from any single caller
// (context.WithoutCancel plus a timeout), so one request disconnecting does not cancel
// it for the others, yet it cannot run unbounded. Each caller still waits on its own
// request context: a crawler that drops its connection returns promptly with that
// context's error while the detached render continues for whoever remains.
func (s *Server) renderOGImage(ctx context.Context, u store.User, now time.Time) ([]byte, error) {
	ch := s.ogRender.DoChan(strconv.FormatInt(u.ID, 10), func() (any, error) {
		renderCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ogRenderTimeout)
		defer cancel()
		return ogimage.Generate(renderCtx, s.Store, u, now)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.([]byte), nil
	}
}

// ogCacheTTL is how long a rendered preview card is served before a request
// re-renders it. It honors the configured value and falls back to a sane default, so
// a zero-value config (as the tests construct) still caches rather than rendering on
// every request.
func (s *Server) ogCacheTTL() time.Duration {
	if s.Cfg.OGCacheTTL > 0 {
		return s.Cfg.OGCacheTTL
	}
	return time.Hour
}

// writeOGImage serves the card bytes as a PNG. The Cache-Control window mirrors the
// server-side TTL, so a crawler's repeat unfurls stay off the render path for about
// as long as the cached card is considered fresh, without pinning a stale card
// longer.
func (s *Server) writeOGImage(w http.ResponseWriter, png []byte) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(s.ogCacheTTL().Seconds())))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}
