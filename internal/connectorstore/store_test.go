package connectorstore_test

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/connectorstore"
	"github.com/spacelions/eunomia/test/testpg"
)

func TestStore_CRUDAndValidation(t *testing.T) {
	_, database := testpg.NewMigrated(t)
	store := connectorstore.New(database)
	ctx := context.Background()

	require.Error(t, store.Upsert(ctx, connectorstore.Record{ConnectorType: "notion"}))
	require.Error(t, store.Upsert(ctx, connectorstore.Record{TenantID: "spacelions"}))

	_, err := store.Get(ctx, "spacelions", "notion")
	require.ErrorIs(t, err, connectorstore.ErrConnectorNotFound)
	require.ErrorIs(t, store.SetEnabled(ctx, "spacelions", "notion", false), connectorstore.ErrConnectorNotFound)

	// Insert with nil maps (defaults to empty JSON objects).
	require.NoError(t, store.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: "slack",
		Enabled:       false,
	}))

	// Insert notion enabled with resources, then update via upsert.
	require.NoError(t, store.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: "notion",
		Enabled:       true,
		Settings:      map[string]string{"workspace": "spacelions"},
		Resources:     map[string]string{"onboarding_tracker": "db-123"},
	}))
	require.NoError(t, store.Upsert(ctx, connectorstore.Record{
		TenantID:      "spacelions",
		ConnectorType: "notion",
		Enabled:       true,
		Settings:      map[string]string{"workspace": "spacelions"},
		Resources:     map[string]string{"onboarding_tracker": "db-456"},
	}))

	got, err := store.Get(ctx, "spacelions", "notion")
	require.NoError(t, err)
	assert.True(t, got.Enabled)
	assert.Empty(t, cmp.Diff(map[string]string{"workspace": "spacelions"}, got.Settings))
	assert.Empty(t, cmp.Diff(map[string]string{"onboarding_tracker": "db-456"}, got.Resources))

	all, err := store.List(ctx, "spacelions")
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Empty(t, cmp.Diff([]string{"notion", "slack"}, []string{all[0].ConnectorType, all[1].ConnectorType}))

	enabled, err := store.ListEnabled(ctx, "spacelions")
	require.NoError(t, err)
	require.Len(t, enabled, 1)
	assert.Equal(t, "notion", enabled[0].ConnectorType)

	require.NoError(t, store.SetEnabled(ctx, "spacelions", "notion", false))
	enabled, err = store.ListEnabled(ctx, "spacelions")
	require.NoError(t, err)
	assert.Empty(t, enabled)

	// Canceled context propagates DB errors across all store methods.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, store.Upsert(canceledCtx, connectorstore.Record{TenantID: "spacelions", ConnectorType: "notion"}))
	_, err = store.Get(canceledCtx, "spacelions", "notion")
	require.Error(t, err)
	_, err = store.List(canceledCtx, "spacelions")
	require.Error(t, err)
	_, err = store.ListEnabled(canceledCtx, "spacelions")
	require.Error(t, err)
	require.Error(t, store.SetEnabled(canceledCtx, "spacelions", "notion", true))
}
