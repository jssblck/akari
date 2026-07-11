package main

import (
	"context"
	"log"

	"github.com/jssblck/akari/internal/server/store"
)

// runMessageHashBackfill fills any message rows migration 0049 left at its
// unbackfilled sentinel (see Store.BackfillMessageContentHashes) so an
// upgraded server reaches full column coverage without the migration ever
// holding messages' access-exclusive lock across a full-table UPDATE. It runs
// once, after migrations apply, entirely off the request path: a fresh
// database has nothing to do and returns almost immediately, while a large
// pre-migration corpus catches up in the background over bounded, paced
// batches while the server keeps serving traffic normally.
func runMessageHashBackfill(ctx context.Context, st *store.Store) {
	log.Printf("message content hash backfill starting")
	n, err := st.BackfillMessageContentHashes(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		log.Printf("message content hash backfill stopped early after %d row(s): %v", n, err)
	case err != nil:
		log.Printf("message content hash backfill: %v", err)
	default:
		log.Printf("message content hash backfill complete: %d row(s) backfilled", n)
	}
}
