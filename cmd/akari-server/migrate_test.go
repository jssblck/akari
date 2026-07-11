package main

import (
	"context"
	"embed"
	"errors"
	"testing"
)

type contextRecordingMigrator struct {
	ctx context.Context
}

func (m *contextRecordingMigrator) Migrate(ctx context.Context, _ embed.FS) error {
	m.ctx = ctx
	return ctx.Err()
}

func TestMigrateStorePropagatesCommandContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	migrator := &contextRecordingMigrator{}

	err := migrateStore(ctx, migrator)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("migrateStore error = %v, want context cancellation", err)
	}
	if migrator.ctx != ctx {
		t.Fatal("migrateStore replaced the command context")
	}
}
