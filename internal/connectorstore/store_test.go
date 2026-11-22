package connectorstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/spacelions/eunomia/internal/connectorstore"
	"github.com/spacelions/eunomia/internal/testpg"
)

func TestStore_CRUDAndValidation(t *testing.T) {
	_, database := testpg.NewMigrated(t)
	store := connectorstore.New(database)
	ctx := context.Background()

	// Validation errors.
	if err := store.Upsert(ctx, connectorstore.Record{ConnectorType: "notion"}); err == nil {
		t.Fatal("expected error for missing tenant_id")
	}
	if err := store.Upsert(ctx, connectorstore.Record{TenantID: "spacelions"}); err == nil {
		t.Fatal("expected error for missing connector_type")
	}

	// Not found errors.
	if _, err := store.Get(ctx, "spacelions", "notion"); !errors.Is(err, connectorstore.ErrConnectorNotFound) {
		t.Fatalf("expected ErrConnectorNotFound, got: %v", err)
	}
	if err := store.SetEnabled(ctx, "spacelions", "notion", false); !errors.Is(err, connectorstore.ErrConnectorNotFound) {
		t.Fatalf("expected ErrConnectorNotFound on SetEnabled, got: %v", err)
	}

	// Insert with nil maps (defaults to empty JSON objects).
	if err := store.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: "slack",
		Enabled:       false,
	}); err != nil {
		t.Fatalf("Upsert slack: %v", err)
	}

	// Insert notion enabled with resources.
	if err := store.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: "notion",
		Enabled:       true,
		Settings:      map[string]string{"workspace": "spacelions"},
		Resources:     map[string]string{"onboarding_tracker": "db-123"},
	}); err != nil {
		t.Fatalf("Upsert notion: %v", err)
	}

	// Upsert updates existing record.
	if err := store.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: "notion",
		Enabled:       true,
		Settings:      map[string]string{"workspace": "spacelions"},
		Resources:     map[string]string{"onboarding_tracker": "db-456"},
	}); err != nil {
		t.Fatalf("Upsert update notion: %v", err)
	}

	got, err := store.Get(ctx, "spacelions", "notion")
	if err != nil {
		t.Fatalf("Get notion: %v", err)
	}
	if got.Resources["onboarding_tracker"] != "db-456" || !got.Enabled {
		t.Fatalf("unexpected notion record: %+v", got)
	}

	all, err := store.List(ctx, "spacelions")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 || all[0].ConnectorType != "notion" || all[1].ConnectorType != "slack" {
		t.Fatalf("unexpected List output: %+v", all)
	}

	enabled, err := store.ListEnabled(ctx, "spacelions")
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	if len(enabled) != 1 || enabled[0].ConnectorType != "notion" {
		t.Fatalf("unexpected ListEnabled output: %+v", enabled)
	}

	// Disable notion.
	if err := store.SetEnabled(ctx, "spacelions", "notion", false); err != nil {
		t.Fatalf("SetEnabled false: %v", err)
	}
	enabled, err = store.ListEnabled(ctx, "spacelions")
	if err != nil {
		t.Fatalf("ListEnabled after disable: %v", err)
	}
	if len(enabled) != 0 {
		t.Fatalf("expected 0 enabled connectors, got %d", len(enabled))
	}

	// Canceled context propagates DB errors across all store methods.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Upsert(canceledCtx, connectorstore.Record{TenantID: "spacelions", ConnectorType: "notion"}); err == nil {
		t.Fatal("expected Upsert error on canceled context")
	}
	if _, err := store.Get(canceledCtx, "spacelions", "notion"); err == nil {
		t.Fatal("expected Get error on canceled context")
	}
	if _, err := store.List(canceledCtx, "spacelions"); err == nil {
		t.Fatal("expected List error on canceled context")
	}
	if _, err := store.ListEnabled(canceledCtx, "spacelions"); err == nil {
		t.Fatal("expected ListEnabled error on canceled context")
	}
	if err := store.SetEnabled(canceledCtx, "spacelions", "notion", true); err == nil {
		t.Fatal("expected SetEnabled error on canceled context")
	}
}
