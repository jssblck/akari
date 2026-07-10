package storetest

import (
	"context"
	"io/fs"
	"sync"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/migrations"
)

// TestConcurrentMigrateSerializesFreshDatabase starts several replicas against
// one empty database at the same instant. Every caller must wait for the same
// database-scoped migration lock and then observe the versions committed by the
// caller ahead of it.
func TestConcurrentMigrateSerializesFreshDatabase(t *testing.T) {
	dbURL := provision(t)
	ctx := context.Background()

	const callers = 8
	stores := make([]*store.Store, callers)
	for i := range stores {
		st, err := store.Open(ctx, dbURL)
		if err != nil {
			t.Fatalf("open store %d: %v", i, err)
		}
		stores[i] = st
		t.Cleanup(st.Close)
	}

	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for _, st := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- st.Migrate(ctx, migrations.FS)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent migrate: %v", err)
		}
	}
	if t.Failed() {
		return
	}

	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("list embedded migrations: %v", err)
	}
	var applied int
	if err := stores[0].Pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if applied != len(files) {
		t.Fatalf("applied migrations = %d, want %d", applied, len(files))
	}
}
