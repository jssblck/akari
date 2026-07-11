package main

import (
	"context"
	"embed"

	"github.com/jssblck/akari/migrations"
)

type schemaMigrator interface {
	Migrate(context.Context, embed.FS) error
}

// migrateStore keeps schema work under the command's lifetime context. Some
// data migrations legitimately exceed a minute, while cancellation still needs
// to stop startup or a foreground command promptly.
func migrateStore(ctx context.Context, migrator schemaMigrator) error {
	return migrator.Migrate(ctx, migrations.FS)
}
