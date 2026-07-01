package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jssblck/akari/internal/server/ogimage"
	"github.com/jssblck/akari/internal/server/store"
)

// ogImageMaxAge is how old a published overview's preview card may get before the
// background refresh re-renders it. The card is a share-preview snapshot, so a day
// of lag is fine; this is what makes the refresh "once per day" per card, however
// often the loop below wakes.
const ogImageMaxAge = 24 * time.Hour

// runOGRefresh re-renders the Open Graph preview cards of published overviews on a
// fixed interval until the context is cancelled. Each wake regenerates only the
// cards older than ogImageMaxAge (or missing entirely), so a card refreshes about
// once a day regardless of the wake cadence, and a restart does not reset the clock
// (staleness is measured from the stored generated_at, not process start). Each
// pass is bounded by its own timeout so a slow render cannot stack up behind the
// ticker.
func runOGRefresh(ctx context.Context, st *store.Store, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refreshStaleOGImages(ctx, st)
		}
	}
}

// refreshStaleOGImages renders every published overview whose card is stale or
// missing. A single account's failure is logged and skipped rather than aborting
// the pass, so one bad render does not starve the rest.
//
// Each render's analytics query is user-scoped and, thanks to the
// (session_id, occurred_at) index (migration 0018), scans only that user's events
// rather than the whole trailing-year window. So the pass costs about the published
// users' own usage in total, not (users x all recent events): it stays linear in
// the data as published accounts and their usage grow.
//
// Coordination with reparse lives inside ogimage.Generate, which aborts
// (ErrReparseInProgress) rather than cache a card read across a projection rebuild.
// The pass counts those aborts as skips and continues; a cheap up-front lock check
// short-circuits the whole pass when a reparse is already running, so it does not
// churn through every user only to skip each one.
func refreshStaleOGImages(ctx context.Context, st *store.Store) {
	passCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if held, err := st.ReparseLockHeld(passCtx); err != nil {
		log.Printf("og refresh: checking reparse lock: %v", err)
		return
	} else if held {
		log.Printf("og refresh: reparse in progress, skipping pass")
		return
	}

	now := time.Now()
	users, err := st.PublicOverviewsNeedingOGImage(passCtx, now.Add(-ogImageMaxAge))
	if err != nil {
		log.Printf("og refresh: list stale cards: %v", err)
		return
	}
	var done, skipped, failed int
	for _, u := range users {
		if passCtx.Err() != nil {
			break
		}
		switch err := ogimage.Generate(passCtx, st, u, now); {
		case err == nil:
			done++
		case errors.Is(err, ogimage.ErrReparseInProgress):
			// A reparse began mid-pass; stop rather than skip every remaining user in
			// turn, and let the next pass fill the cards once it finishes.
			skipped++
			log.Printf("og refresh: reparse started mid-pass, stopping after %d card(s)", done)
			cancel()
		default:
			failed++
			log.Printf("og refresh: render for user %d (%s): %v", u.ID, u.Username, err)
		}
	}
	if done > 0 || failed > 0 || skipped > 0 {
		log.Printf("og refresh: rendered %d card(s), %d failed, %d skipped", done, failed, skipped)
	}
}
