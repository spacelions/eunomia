package db_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/db"
	"github.com/spacelions/eunomia/test/testpg"
)

func TestOpen_UnreachableFailsFast(t *testing.T) {
	_, err := db.Open(context.Background(), "postgres://postgres@127.0.0.1:1/eunomia?sslmode=disable")
	require.Error(t, err)
}

func TestMigrate_Idempotent(t *testing.T) {
	_, database := testpg.NewRaw(t)
	ctx := context.Background()

	group, err := db.Migrate(ctx, database)
	require.NoError(t, err)
	assert.False(t, group.IsZero(), "expected initial migration group to be applied")

	// Second run should be a clean no-op.
	group2, err := db.Migrate(ctx, database)
	require.NoError(t, err)
	assert.True(t, group2.IsZero(), "expected zero migrations on second run")

	for _, table := range []string{"tenant_connectors", "tenant_secrets"} {
		var exists bool
		err := database.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = ?)`,
			table,
		).Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, "expected table %s to exist after migration", table)
	}

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = db.Migrate(canceledCtx, database)
	require.Error(t, err)
}
