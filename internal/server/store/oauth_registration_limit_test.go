package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

func TestOAuthRegistrationCeilingRejectsDurableGrowth(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := st.CreateOAuthClient(ctx, fmt.Sprintf("client-%d", i), "Ada's agent", []string{"http://127.0.0.1/cb"}, 2); err != nil {
			t.Fatalf("registration %d: %v", i, err)
		}
	}
	if err := st.CreateOAuthClient(ctx, "client-over-limit", "Ada's agent", []string{"http://127.0.0.1/cb"}, 2); !errors.Is(err, store.ErrOAuthRegistrationLimit) {
		t.Fatalf("registration above ceiling = %v, want ErrOAuthRegistrationLimit", err)
	}

	var count int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM oauth_clients`).Scan(&count); err != nil {
		t.Fatalf("count clients: %v", err)
	}
	if count != 2 {
		t.Fatalf("stored clients = %d, want 2", count)
	}
}

func TestOAuthRegistrationCeilingUsesRollingHour(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO oauth_clients (id, client_name, redirect_uris, created_at)
		 VALUES ('old-client', 'Anna Winlock agent', ARRAY['http://127.0.0.1/cb'], now() - interval '2 hours')`); err != nil {
		t.Fatalf("insert old registration: %v", err)
	}
	if err := st.CreateOAuthClient(ctx, "new-client", "Anna's agent", []string{"http://127.0.0.1/cb"}, 1); err != nil {
		t.Fatalf("registration after old window: %v", err)
	}
}

func TestOAuthRegistrationCeilingIsAtomicAcrossConcurrentRequests(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	const (
		attempts = 24
		limit    = 5
	)
	replica, err := store.Open(ctx, st.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	defer replica.Close()
	stores := []*store.Store{st, replica}

	start := make(chan struct{})
	var admitted atomic.Int64
	var rejected atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := stores[i%len(stores)].CreateOAuthClient(ctx, fmt.Sprintf("client-%d", i), "Grace's agent", []string{"http://127.0.0.1/cb"}, limit)
			switch {
			case err == nil:
				admitted.Add(1)
			case errors.Is(err, store.ErrOAuthRegistrationLimit):
				rejected.Add(1)
			default:
				t.Errorf("registration %d: %v", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := admitted.Load(); got != limit {
		t.Fatalf("admitted registrations = %d, want %d", got, limit)
	}
	if got := rejected.Load(); got != attempts-limit {
		t.Fatalf("rejected registrations = %d, want %d", got, attempts-limit)
	}
}
