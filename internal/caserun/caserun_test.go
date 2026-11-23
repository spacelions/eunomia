package caserun_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/caserun"
	"github.com/spacelions/eunomia/internal/playbook"
	"github.com/spacelions/eunomia/test/testpg"
)

func seedPlaybook(t *testing.T, ctx context.Context, pbStore *playbook.Store, tenantID string) *playbook.Record {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "employee_onboarding_v1.md"))
	require.NoError(t, err)
	rec, err := pbStore.Publish(ctx, tenantID, string(raw))
	require.NoError(t, err)
	return rec
}

func TestStatusStateMachine(t *testing.T) {
	id := caserun.NewRunID()
	assert.True(t, strings.HasPrefix(id, "run_"))
	assert.Len(t, id, 30)
	assert.Equal(t, strings.ToLower(id), id)

	assert.True(t, caserun.StatusPending.IsValid())
	assert.True(t, caserun.StatusCompleted.IsValid())
	assert.False(t, caserun.Status("bogus").IsValid())

	assert.False(t, caserun.StatusPending.IsTerminal())
	assert.False(t, caserun.StatusRunning.IsTerminal())
	assert.True(t, caserun.StatusCompleted.IsTerminal())
	assert.True(t, caserun.StatusEscalated.IsTerminal())
	assert.True(t, caserun.StatusFailed.IsTerminal())
	assert.True(t, caserun.StatusCancelled.IsTerminal())

	assert.True(t, caserun.CanTransition(caserun.StatusPending, caserun.StatusRunning))
	assert.True(t, caserun.CanTransition(caserun.StatusRunning, caserun.StatusWaitingApproval))
	assert.True(t, caserun.CanTransition(caserun.StatusWaitingApproval, caserun.StatusRunning))
	assert.True(t, caserun.CanTransition(caserun.StatusRunning, caserun.StatusCompleted))

	assert.False(t, caserun.CanTransition(caserun.StatusPending, caserun.StatusCompleted))
	assert.False(t, caserun.CanTransition(caserun.StatusCompleted, caserun.StatusRunning))
	assert.False(t, caserun.CanTransition(caserun.StatusWaitingHuman, caserun.StatusCompleted))
}

func TestStore_Lifecycle_ConcurrencyGuard_AndToolTracking(t *testing.T) {
	_, database := testpg.NewMigrated(t)
	ctx := context.Background()
	pbStore := playbook.NewStore(database)
	store := caserun.NewStore(database)

	pb := seedPlaybook(t, ctx, pbStore, "spacelions")

	// Create validation errors & FK check against missing playbook version.
	_, err := store.Create(ctx, caserun.CreateParams{PlaybookID: pb.PlaybookID, PlaybookVersion: 1})
	require.Error(t, err)
	_, err = store.Create(ctx, caserun.CreateParams{TenantID: "spacelions", PlaybookVersion: 1})
	require.Error(t, err)
	_, err = store.Create(ctx, caserun.CreateParams{TenantID: "spacelions", PlaybookID: pb.PlaybookID, PlaybookVersion: 0})
	require.Error(t, err)
	_, err = store.Create(ctx, caserun.CreateParams{TenantID: "spacelions", PlaybookID: pb.PlaybookID, PlaybookVersion: 99})
	require.Error(t, err)

	// Not-found checks.
	_, err = store.Get(ctx, "spacelions", "run_missing")
	require.ErrorIs(t, err, caserun.ErrRunNotFound)
	_, err = store.Transition(ctx, "spacelions", "run_missing", caserun.StatusPending, caserun.StatusRunning, caserun.TransitionUpdate{})
	require.ErrorIs(t, err, caserun.ErrRunNotFound)

	// 1. Create a run pinned to playbook v1.
	run, err := store.Create(ctx, caserun.CreateParams{
		TenantID:        "spacelions",
		PlaybookID:      pb.PlaybookID,
		PlaybookVersion: pb.Version,
		DryRun:          true,
		Inputs:          map[string]any{"candidate_name": "Alice Wang"},
	})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(run.RunID, "run_"))
	assert.Equal(t, caserun.StatusPending, run.Status)
	assert.True(t, run.DryRun)
	assert.Nil(t, run.StartedAt)
	assert.Nil(t, run.CompletedAt)

	// 2. Illegal static transition rejected before touching DB.
	_, err = store.Transition(ctx, "spacelions", run.RunID, caserun.StatusPending, caserun.StatusCompleted, caserun.TransitionUpdate{})
	require.ErrorIs(t, err, caserun.ErrInvalidTransition)

	// 3. pending -> running sets started_at and temporal_workflow_id.
	wfID := "case-run-" + run.RunID
	run, err = store.Transition(ctx, "spacelions", run.RunID, caserun.StatusPending, caserun.StatusRunning, caserun.TransitionUpdate{
		TemporalWorkflowID: &wfID,
	})
	require.NoError(t, err)
	assert.Equal(t, caserun.StatusRunning, run.Status)
	assert.Equal(t, wfID, run.TemporalWorkflowID)
	require.NotNil(t, run.StartedAt)
	firstStartedAt := *run.StartedAt

	// Optimistic race: stale caller thinking run is still `pending` fails with ErrInvalidTransition.
	_, err = store.Transition(ctx, "spacelions", run.RunID, caserun.StatusPending, caserun.StatusRunning, caserun.TransitionUpdate{})
	require.ErrorIs(t, err, caserun.ErrInvalidTransition)

	// 4. Pause for human -> resume -> pause for approval -> resume -> complete.
	run, err = store.Transition(ctx, "spacelions", run.RunID, caserun.StatusRunning, caserun.StatusWaitingHuman, caserun.TransitionUpdate{})
	require.NoError(t, err)
	assert.Equal(t, caserun.StatusWaitingHuman, run.Status)

	run, err = store.Transition(ctx, "spacelions", run.RunID, caserun.StatusWaitingHuman, caserun.StatusRunning, caserun.TransitionUpdate{})
	require.NoError(t, err)
	require.NotNil(t, run.StartedAt)
	assert.Equal(t, firstStartedAt, *run.StartedAt, "resuming should preserve original started_at")

	// Record tool completions (deduped + sorted + turn counter).
	_, err = store.RecordToolCompletion(ctx, "spacelions", run.RunID, "  ", true)
	require.Error(t, err)
	_, err = store.RecordToolCompletion(ctx, "spacelions", "run_missing", "notion.upsert_db_row", true)
	require.ErrorIs(t, err, caserun.ErrRunNotFound)

	run, err = store.RecordToolCompletion(ctx, "spacelions", run.RunID, "notion.upsert_db_row", true)
	require.NoError(t, err)
	run, err = store.RecordToolCompletion(ctx, "spacelions", run.RunID, "google_workspace.create_user", true)
	require.NoError(t, err)
	run, err = store.RecordToolCompletion(ctx, "spacelions", run.RunID, "notion.upsert_db_row", false)
	require.NoError(t, err)
	assert.Empty(t, cmp.Diff([]string{"google_workspace.create_user", "notion.upsert_db_row"}, run.CompletedTools))
	assert.Equal(t, 2, run.TurnCount)

	run, err = store.Transition(ctx, "spacelions", run.RunID, caserun.StatusRunning, caserun.StatusCompleted, caserun.TransitionUpdate{
		Result: map[string]any{"work_email": "alice@spacelions.com"},
	})
	require.NoError(t, err)
	assert.Equal(t, caserun.StatusCompleted, run.Status)
	require.NotNil(t, run.CompletedAt)
	assert.Equal(t, "alice@spacelions.com", run.Result["work_email"])

	// 5. Second run failing with ErrorMessage + List filters.
	run2, err := store.Create(ctx, caserun.CreateParams{
		TenantID:        "spacelions",
		RunID:           "run_custom_01",
		PlaybookID:      pb.PlaybookID,
		PlaybookVersion: pb.Version,
	})
	require.NoError(t, err)
	errMsg := "upstream connector timeout"
	run2, err = store.Transition(ctx, "spacelions", run2.RunID, caserun.StatusPending, caserun.StatusFailed, caserun.TransitionUpdate{
		ErrorMessage: &errMsg,
	})
	require.NoError(t, err)
	assert.Equal(t, errMsg, run2.ErrorMessage)

	all, err := store.List(ctx, "spacelions", caserun.ListFilter{})
	require.NoError(t, err)
	assert.Len(t, all, 2)

	onlyFailed, err := store.List(ctx, "spacelions", caserun.ListFilter{
		PlaybookID: pb.PlaybookID,
		Status:     caserun.StatusFailed,
	})
	require.NoError(t, err)
	require.Len(t, onlyFailed, 1)
	assert.Equal(t, run2.RunID, onlyFailed[0].RunID)

	_, err = store.List(ctx, "spacelions", caserun.ListFilter{Status: caserun.Status("invalid")})
	require.Error(t, err)

	// Canceled context propagates DB errors cleanly.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.Get(canceledCtx, "spacelions", run.RunID)
	require.Error(t, err)
	_, err = store.List(canceledCtx, "spacelions", caserun.ListFilter{})
	require.Error(t, err)
	_, err = store.Transition(canceledCtx, "spacelions", run.RunID, caserun.StatusPending, caserun.StatusRunning, caserun.TransitionUpdate{})
	require.Error(t, err)
	_, err = store.RecordToolCompletion(canceledCtx, "spacelions", run.RunID, "gmail.send_email", true)
	require.Error(t, err)
}
