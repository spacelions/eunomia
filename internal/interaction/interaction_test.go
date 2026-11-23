package interaction_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/caserun"
	"github.com/spacelions/eunomia/internal/interaction"
	"github.com/spacelions/eunomia/internal/playbook"
	"github.com/spacelions/eunomia/test/testpg"
)

func seedRun(t *testing.T, ctx context.Context, pbStore *playbook.Store, runStore *caserun.Store, tenantID string) *caserun.CaseRun {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "employee_onboarding_v1.md"))
	require.NoError(t, err)
	pb, err := pbStore.Publish(ctx, tenantID, string(raw))
	require.NoError(t, err)
	run, err := runStore.Create(ctx, caserun.CreateParams{
		TenantID:        tenantID,
		PlaybookID:      pb.PlaybookID,
		PlaybookVersion: pb.Version,
	})
	require.NoError(t, err)
	return run
}

func TestStateAndTokenHelpers(t *testing.T) {
	id := interaction.NewTaskID()
	assert.True(t, strings.HasPrefix(id, "task_"))
	assert.Len(t, id, 31)
	assert.Equal(t, strings.ToLower(id), id)

	assert.True(t, interaction.KindQuestion.IsValid())
	assert.True(t, interaction.KindApproval.IsValid())
	assert.False(t, interaction.Kind("other").IsValid())

	assert.False(t, interaction.StatusOpen.IsTerminal())
	assert.True(t, interaction.StatusSubmitted.IsTerminal())
	assert.True(t, interaction.StatusApproved.IsTerminal())
	assert.True(t, interaction.StatusRejected.IsTerminal())
	assert.True(t, interaction.StatusExpired.IsTerminal())
	assert.True(t, interaction.StatusCancelled.IsTerminal())

	assert.True(t, interaction.CanTransition(interaction.KindQuestion, interaction.StatusOpen, interaction.StatusSubmitted))
	assert.False(t, interaction.CanTransition(interaction.KindQuestion, interaction.StatusOpen, interaction.StatusApproved))
	assert.True(t, interaction.CanTransition(interaction.KindApproval, interaction.StatusOpen, interaction.StatusApproved))
	assert.True(t, interaction.CanTransition(interaction.KindApproval, interaction.StatusOpen, interaction.StatusRejected))
	assert.False(t, interaction.CanTransition(interaction.KindApproval, interaction.StatusOpen, interaction.StatusSubmitted))
	assert.False(t, interaction.CanTransition(interaction.KindApproval, interaction.StatusApproved, interaction.StatusRejected))

	tok, hash, err := interaction.GenerateToken()
	require.NoError(t, err)
	assert.NotEmpty(t, tok)
	assert.NotEqual(t, tok, hash)
	assert.Equal(t, hash, interaction.HashToken(tok))
}

func TestStore_MagicLinks_KindGuards_AndSingleWinnerRace(t *testing.T) {
	_, database := testpg.NewMigrated(t)
	ctx := context.Background()
	pbStore := playbook.NewStore(database)
	runStore := caserun.NewStore(database)
	store := interaction.NewStore(database)

	run := seedRun(t, ctx, pbStore, runStore, "spacelions")
	expiry := time.Now().UTC().Add(48 * time.Hour)

	// Validation errors & FK check.
	_, _, err := store.Create(ctx, interaction.CreateParams{RunID: run.RunID, Kind: interaction.KindQuestion, RecipientEmail: "fu@spacelions.com", ExpiresAt: expiry})
	require.Error(t, err)
	_, _, err = store.Create(ctx, interaction.CreateParams{TenantID: "spacelions", Kind: interaction.KindQuestion, RecipientEmail: "fu@spacelions.com", ExpiresAt: expiry})
	require.Error(t, err)
	_, _, err = store.Create(ctx, interaction.CreateParams{TenantID: "spacelions", RunID: run.RunID, Kind: interaction.Kind("bad"), RecipientEmail: "fu@spacelions.com", ExpiresAt: expiry})
	require.Error(t, err)
	_, _, err = store.Create(ctx, interaction.CreateParams{TenantID: "spacelions", RunID: run.RunID, Kind: interaction.KindQuestion, RecipientEmail: "bad-email", ExpiresAt: expiry})
	require.Error(t, err)
	_, _, err = store.Create(ctx, interaction.CreateParams{TenantID: "spacelions", RunID: run.RunID, Kind: interaction.KindQuestion, RecipientEmail: "fu@spacelions.com"})
	require.Error(t, err)
	_, _, err = store.Create(ctx, interaction.CreateParams{TenantID: "spacelions", RunID: "run_missing", Kind: interaction.KindQuestion, RecipientEmail: "fu@spacelions.com", ExpiresAt: expiry})
	require.Error(t, err)

	// Not-found checks.
	_, err = store.Get(ctx, "spacelions", "task_missing")
	require.ErrorIs(t, err, interaction.ErrTaskNotFound)
	_, err = store.GetByToken(ctx, "")
	require.ErrorIs(t, err, interaction.ErrTaskNotFound)
	_, err = store.GetByToken(ctx, "nonexistent-token")
	require.ErrorIs(t, err, interaction.ErrTaskNotFound)
	require.ErrorIs(t, store.UpdateDeliveryMeta(ctx, "spacelions", "task_missing", nil), interaction.ErrTaskNotFound)
	_, err = store.Transition(ctx, "spacelions", "task_missing", interaction.StatusCancelled, "system", nil)
	require.ErrorIs(t, err, interaction.ErrTaskNotFound)

	// 1. Question task: create -> Magic Link lookup -> update delivery meta -> submit.
	qTask, qToken, err := store.Create(ctx, interaction.CreateParams{
		TenantID:        "spacelions",
		RunID:           run.RunID,
		Kind:            interaction.KindQuestion,
		RecipientUserID: "fu",
		RecipientEmail:  "Fu@SpaceLions.com",
		PromptPayload:   map[string]any{"question": "Confirm preferred laptop model"},
		ExpiresAt:       expiry,
	})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(qTask.TaskID, "task_"))
	assert.Equal(t, "fu@spacelions.com", qTask.RecipientEmail)
	assert.Equal(t, interaction.StatusOpen, qTask.Status)
	assert.NotEqual(t, qToken, qTask.TokenHash)

	byToken, err := store.GetByToken(ctx, qToken)
	require.NoError(t, err)
	assert.Equal(t, qTask.TaskID, byToken.TaskID)

	require.NoError(t, store.UpdateDeliveryMeta(ctx, "spacelions", qTask.TaskID, map[string]any{
		"provider":   "slack",
		"message_ts": "1732320000.000100",
	}))
	refetched, err := store.Get(ctx, "spacelions", qTask.TaskID)
	require.NoError(t, err)
	assert.Equal(t, "slack", refetched.DeliveryMeta["provider"])

	// Non-terminal transition and wrong-kind transition (`approved` on `question`) MUST be rejected.
	_, err = store.Transition(ctx, "spacelions", qTask.TaskID, interaction.StatusOpen, "fu", nil)
	require.ErrorIs(t, err, interaction.ErrInvalidTransition)
	_, err = store.Transition(ctx, "spacelions", qTask.TaskID, interaction.StatusApproved, "fu", nil)
	require.ErrorIs(t, err, interaction.ErrInvalidTransition)

	submitted, err := store.Transition(ctx, "spacelions", qTask.TaskID, interaction.StatusSubmitted, "fu", map[string]any{
		"answer": "MacBook Pro 16",
	})
	require.NoError(t, err)
	assert.Equal(t, interaction.StatusSubmitted, submitted.Status)
	assert.Equal(t, "fu", submitted.RespondedBy)
	require.NotNil(t, submitted.RespondedAt)
	assert.Equal(t, "MacBook Pro 16", submitted.ResponsePayload["answer"])

	// Second submit on already-closed task MUST fail with ErrInvalidTransition.
	_, err = store.Transition(ctx, "spacelions", qTask.TaskID, interaction.StatusSubmitted, "fu", nil)
	require.ErrorIs(t, err, interaction.ErrInvalidTransition)

	// 2. Approval task: rejects `submitted`, allows concurrent single-winner approval race.
	aTask, _, err := store.Create(ctx, interaction.CreateParams{
		TenantID:       "spacelions",
		TaskID:         "task_custom_approval_01",
		RunID:          run.RunID,
		Kind:           interaction.KindApproval,
		RecipientEmail: "fu@spacelions.com",
		PromptPayload:  map[string]any{"draft_subject": "Welcome to Space Lions!"},
		ExpiresAt:      expiry,
	})
	require.NoError(t, err)

	_, err = store.Transition(ctx, "spacelions", aTask.TaskID, interaction.StatusSubmitted, "fu", nil)
	require.ErrorIs(t, err, interaction.ErrInvalidTransition)

	// Two approvers click Approve/Reject concurrently -> strictly ONE succeeds, the other gets ErrInvalidTransition.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = store.Transition(ctx, "spacelions", aTask.TaskID, interaction.StatusApproved, "approver_1", map[string]any{"note": "LGTM"})
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = store.Transition(ctx, "spacelions", aTask.TaskID, interaction.StatusRejected, "approver_2", map[string]any{"note": "hold"})
	}()
	wg.Wait()

	successes := 0
	invalidTransitions := 0
	for _, e := range errs {
		if e == nil {
			successes++
		} else {
			require.ErrorIs(t, e, interaction.ErrInvalidTransition)
			invalidTransitions++
		}
	}
	assert.Equal(t, 1, successes, "exactly one concurrent resolution must win")
	assert.Equal(t, 1, invalidTransitions, "the losing resolution must fail with ErrInvalidTransition")

	// 3. Expired task + ListByRun.
	expTask, _, err := store.Create(ctx, interaction.CreateParams{
		TenantID:       "spacelions",
		RunID:          run.RunID,
		Kind:           interaction.KindQuestion,
		RecipientEmail: "alice@gmail.com",
		ExpiresAt:      expiry,
	})
	require.NoError(t, err)
	expTask, err = store.Transition(ctx, "spacelions", expTask.TaskID, interaction.StatusExpired, "system", nil)
	require.NoError(t, err)
	assert.Equal(t, interaction.StatusExpired, expTask.Status)
	assert.Nil(t, expTask.RespondedAt)

	list, err := store.ListByRun(ctx, "spacelions", run.RunID)
	require.NoError(t, err)
	assert.Len(t, list, 3)

	// Canceled context propagates DB errors cleanly.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.Get(canceledCtx, "spacelions", qTask.TaskID)
	require.Error(t, err)
	_, err = store.GetByToken(canceledCtx, qToken)
	require.Error(t, err)
	_, err = store.ListByRun(canceledCtx, "spacelions", run.RunID)
	require.Error(t, err)
	require.Error(t, store.UpdateDeliveryMeta(canceledCtx, "spacelions", qTask.TaskID, nil))
	_, err = store.Transition(canceledCtx, "spacelions", qTask.TaskID, interaction.StatusCancelled, "system", nil)
	require.Error(t, err)
}
