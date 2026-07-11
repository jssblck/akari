package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jssblck/akari/internal/server/storetest"
)

func TestMCPMessagesAfterHonorsCancellation(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := st.MCPMessagesAfter(ctx, 1, nil, 100, 8<<20, 1024)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("MCPMessagesAfter error = %v, want context.Canceled", err)
	}
}
