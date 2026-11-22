package db_test

import (
	"context"
	"testing"

	"github.com/spacelions/eunomia/internal/db"
	"github.com/spacelions/eunomia/internal/testpg"
)

func TestOpen_UnreachableFailsFast(t *testing.T) {
	_, err := db.Open(context.Background(), "postgres://postgres@127.0.0.1:1/eunomia?sslmode=disable")
	if err == nil {
		t.Fatal("expected db.Open to fail fast on unreachable postgres")
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	_, database := testpg.NewRaw(t)
	ctx := context.Background()

	group, err := db.Migrate(ctx, database)
	if err != nil {
		t.Fatalf("first Migrate failed: %v", err)
	}
	if group.IsZero() {
		t.Fatal("expected initial migration group to be applied")
	}

	// Second run should be a clean no-op.
	group2, err := db.Migrate(ctx, database)
	if err != nil {
		t.Fatalf("second Migrate failed: %v", err)
	}
	if !group2.IsZero() {
		t.Fatalf("expected zero migrations on second run, got %s", group2)
	}

	// Verify both tables were created.
	for _, table := range []string{"tenant_connectors", "tenant_secrets"} {
		var exists bool
		err := database.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = ?)`,
			table,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("checking table %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("expected table %s to exist after migration", table)
		}
	}

	// Verify Migrate fails cleanly with a canceled context.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.Migrate(canceledCtx, database); err == nil {
		t.Fatal("expected Migrate error on canceled context")
	}
}
