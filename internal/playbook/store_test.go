package playbook_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/playbook"
	"github.com/spacelions/eunomia/test/testpg"
)

func loadFixtureMarkdown(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "employee_onboarding_v1.md"))
	require.NoError(t, err)
	return string(raw)
}

func TestStore_Drafts_ImmutablePublish_AndVersions(t *testing.T) {
	_, database := testpg.NewMigrated(t)
	store := playbook.NewStore(database)
	ctx := context.Background()
	fixtureMD := loadFixtureMarkdown(t)

	const playbookID = "internal-employee-onboarding"

	// Draft validation errors & not-found checks.
	_, err := store.SaveDraft(ctx, "", playbookID, "wip")
	require.Error(t, err)
	_, err = store.SaveDraft(ctx, "spacelions", "INVALID_ID", "wip")
	require.Error(t, err)
	_, err = store.SaveDraft(ctx, "spacelions", playbookID, "   ")
	require.Error(t, err)
	_, err = store.GetDraft(ctx, "spacelions", playbookID)
	require.ErrorIs(t, err, playbook.ErrDraftNotFound)
	require.ErrorIs(t, store.DeleteDraft(ctx, "spacelions", playbookID), playbook.ErrDraftNotFound)

	// Publish validation errors & not-found checks.
	_, err = store.Publish(ctx, "", fixtureMD)
	require.Error(t, err)
	_, err = store.Publish(ctx, "spacelions", "no frontmatter")
	require.Error(t, err)
	_, err = store.GetVersion(ctx, "spacelions", playbookID, 1)
	require.ErrorIs(t, err, playbook.ErrPlaybookNotFound)
	_, err = store.GetLatest(ctx, "spacelions", playbookID)
	require.ErrorIs(t, err, playbook.ErrPlaybookNotFound)
	_, err = store.PublishDraft(ctx, "spacelions", playbookID)
	require.ErrorIs(t, err, playbook.ErrDraftNotFound)

	// 1. Draft allows saving work-in-progress markdown before it passes full validation.
	wip, err := store.SaveDraft(ctx, "spacelions", playbookID, "# WIP notes without frontmatter yet")
	require.NoError(t, err)
	assert.Equal(t, playbookID, wip.PlaybookID)

	// Publishing an invalid draft fails cleanly.
	_, err = store.PublishDraft(ctx, "spacelions", playbookID)
	require.Error(t, err)

	// Draft whose frontmatter playbook_id mismatches the draft key is rejected on PublishDraft.
	_, err = store.SaveDraft(ctx, "spacelions", "other-playbook", fixtureMD)
	require.NoError(t, err)
	_, err = store.PublishDraft(ctx, "spacelions", "other-playbook")
	require.Error(t, err)

	// 2. Update draft to valid Markdown and publish -> v1.
	_, err = store.SaveDraft(ctx, "spacelions", playbookID, fixtureMD)
	require.NoError(t, err)
	gotDraft, err := store.GetDraft(ctx, "spacelions", playbookID)
	require.NoError(t, err)
	assert.Equal(t, fixtureMD, gotDraft.RawMarkdown)

	v1, err := store.PublishDraft(ctx, "spacelions", playbookID)
	require.NoError(t, err)
	assert.Equal(t, 1, v1.Version)
	assert.Equal(t, playbookID, v1.PlaybookID)
	assert.NotEmpty(t, v1.ContentHash)

	pb1, err := v1.Playbook()
	require.NoError(t, err)
	assert.Equal(t, 1, pb1.Version)

	// 3. Publish again -> automatically assigns immutable v2 with canonical frontmatter version: 2.
	v2, err := store.Publish(ctx, "spacelions", fixtureMD+"\n## Extra Note\nAlways double-check start date.\n")
	require.NoError(t, err)
	assert.Equal(t, 2, v2.Version)
	assert.NotEqual(t, v1.ContentHash, v2.ContentHash)
	pb2, err := v2.Playbook()
	require.NoError(t, err)
	assert.Equal(t, 2, pb2.Version)

	// 4. DB-level immutability trigger MUST reject UPDATE on published playbooks.
	_, err = database.ExecContext(ctx,
		`UPDATE playbooks SET name = 'tampered' WHERE tenant_id = ? AND playbook_id = ? AND version = ?`,
		"spacelions", playbookID, 1,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "immutable")

	// 5. Queries & tenant isolation.
	gotV1, err := store.GetVersion(ctx, "spacelions", playbookID, 1)
	require.NoError(t, err)
	assert.Equal(t, v1.ContentHash, gotV1.ContentHash)

	latest, err := store.GetLatest(ctx, "spacelions", playbookID)
	require.NoError(t, err)
	assert.Equal(t, 2, latest.Version)

	versions, err := store.ListVersions(ctx, "spacelions", playbookID)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	assert.Empty(t, cmp.Diff([]int{2, 1}, []int{versions[0].Version, versions[1].Version}))

	// Different tenant has independent version numbering starting at v1.
	otherV1, err := store.Publish(ctx, "other_tenant", fixtureMD)
	require.NoError(t, err)
	assert.Equal(t, 1, otherV1.Version)

	// DeleteDraft removes working draft cleanly.
	require.NoError(t, store.DeleteDraft(ctx, "spacelions", playbookID))
	_, err = store.GetDraft(ctx, "spacelions", playbookID)
	require.ErrorIs(t, err, playbook.ErrDraftNotFound)

	// Canceled context propagates DB errors cleanly.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.SaveDraft(canceledCtx, "spacelions", playbookID, "wip")
	require.Error(t, err)
	_, err = store.GetDraft(canceledCtx, "spacelions", "other-playbook")
	require.Error(t, err)
	require.Error(t, store.DeleteDraft(canceledCtx, "spacelions", "other-playbook"))
	_, err = store.Publish(canceledCtx, "spacelions", fixtureMD)
	require.Error(t, err)
	_, err = store.GetVersion(canceledCtx, "spacelions", playbookID, 1)
	require.Error(t, err)
	_, err = store.GetLatest(canceledCtx, "spacelions", playbookID)
	require.Error(t, err)
	_, err = store.ListVersions(canceledCtx, "spacelions", playbookID)
	require.Error(t, err)
}
