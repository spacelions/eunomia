package actors_test

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/actors"
	"github.com/spacelions/eunomia/test/testpg"
)

func TestStore_CRUD_Handles_LookupPeople_AndTenantIsolation(t *testing.T) {
	_, database := testpg.NewMigrated(t)
	store := actors.NewStore(database)
	ctx := context.Background()

	// Validation errors.
	require.Error(t, store.Upsert(ctx, actors.Actor{UserID: "fu", Name: "Fu", Email: "fu@spacelions.com"}))
	require.Error(t, store.Upsert(ctx, actors.Actor{TenantID: "spacelions", Name: "Fu", Email: "fu@spacelions.com"}))
	require.Error(t, store.Upsert(ctx, actors.Actor{TenantID: "spacelions", UserID: "fu", Email: "fu@spacelions.com"}))
	require.Error(t, store.Upsert(ctx, actors.Actor{TenantID: "spacelions", UserID: "fu", Name: "Fu", Email: ""}))
	require.Error(t, store.Upsert(ctx, actors.Actor{TenantID: "spacelions", UserID: "fu", Name: "Fu", Email: "not-an-email"}))
	require.Error(t, store.Upsert(ctx, actors.Actor{
		TenantID: "spacelions", UserID: "fu", Name: "Fu", Email: "fu@spacelions.com",
		Handles: map[string]string{"": "U_FU"},
	}))
	require.Error(t, store.Upsert(ctx, actors.Actor{
		TenantID: "spacelions", UserID: "fu", Name: "Fu", Email: "fu@spacelions.com",
		Handles: map[string]string{"slack": "   "},
	}))
	require.Error(t, store.Upsert(ctx, actors.Actor{
		TenantID: "spacelions", UserID: "fu", Name: "Fu", Email: "fu@spacelions.com",
		Handles: map[string]string{"Slack": "U_1", "slack": "U_2"},
	}))

	// Not found checks.
	_, err := store.Get(ctx, "spacelions", "fu")
	require.ErrorIs(t, err, actors.ErrActorNotFound)
	_, err = store.GetByEmail(ctx, "spacelions", "fu@spacelions.com")
	require.ErrorIs(t, err, actors.ErrActorNotFound)
	_, err = store.GetByEmail(ctx, "spacelions", "bad-email")
	require.Error(t, err)
	_, err = store.GetByHandle(ctx, "spacelions", "slack", "U_FU")
	require.ErrorIs(t, err, actors.ErrActorNotFound)
	_, err = store.GetByHandle(ctx, "spacelions", "", "U_FU")
	require.Error(t, err)
	require.ErrorIs(t, store.Delete(ctx, "spacelions", "fu"), actors.ErrActorNotFound)

	// Nil/empty Handle helper safety.
	var nilActor *actors.Actor
	assert.Equal(t, "", nilActor.Handle("slack"))

	// Seed internal actors across multi-member role `it_ops` + extensible connector handles + another tenant.
	require.NoError(t, store.Upsert(ctx, actors.Actor{
		TenantID: "spacelions",
		UserID:   "fu",
		Name:     "Fu Zhang",
		Email:    "Fu@SpaceLions.com",
		Roles:    []string{"IT Ops", "people-ops", "it_ops", ""},
		Handles: map[string]string{
			"Slack":  "U_FU",
			"notion": "N_FU",
			"linear": "LIN_FU",
		},
	}))
	require.NoError(t, store.Upsert(ctx, actors.Actor{
		TenantID: "spacelions",
		UserID:   "alex",
		Name:     "Alex Chen",
		Email:    "alex@spacelions.com",
		Roles:    []string{"it_ops"},
	}))
	require.NoError(t, store.Upsert(ctx, actors.Actor{
		TenantID: "other_tenant",
		UserID:   "bob",
		Name:     "Bob IT",
		Email:    "bob@other.com",
		Roles:    []string{"it_ops"},
		Handles:  map[string]string{"slack": "U_FU"}, // same external ID allowed in a different tenant
	}))

	// Duplicate external handle within the same tenant MUST be rejected by UNIQUE(tenant_id, provider, external_id).
	require.Error(t, store.Upsert(ctx, actors.Actor{
		TenantID: "spacelions",
		UserID:   "alex",
		Name:     "Alex Chen",
		Email:    "alex@spacelions.com",
		Roles:    []string{"it_ops"},
		Handles:  map[string]string{"slack": "U_FU"},
	}))

	// Get hydrates normalized email, roles, and handles.
	got, err := store.Get(ctx, "spacelions", "fu")
	require.NoError(t, err)
	assert.Equal(t, "fu@spacelions.com", got.Email)
	assert.Empty(t, cmp.Diff([]string{"it_ops", "people_ops"}, got.Roles))
	assert.Empty(t, cmp.Diff(map[string]string{
		"linear": "LIN_FU",
		"notion": "N_FU",
		"slack":  "U_FU",
	}, got.Handles))
	assert.Equal(t, "LIN_FU", got.Handle("Linear"))

	// GetByEmail & GetByHandle reverse lookup (e.g. Slack/Linear webhook user IDs).
	byEmail, err := store.GetByEmail(ctx, "spacelions", "FU@spacelions.com")
	require.NoError(t, err)
	assert.Equal(t, "fu", byEmail.UserID)
	assert.Equal(t, "U_FU", byEmail.Handle("slack"))

	byHandle, err := store.GetByHandle(ctx, "spacelions", "Linear", "LIN_FU")
	require.NoError(t, err)
	assert.Equal(t, "fu", byHandle.UserID)
	assert.Equal(t, "N_FU", byHandle.Handle("notion"))

	// List hydrates handles across actors.
	list, err := store.List(ctx, "spacelions")
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Empty(t, cmp.Diff([]string{"alex", "fu"}, []string{list[0].UserID, list[1].UserID}))
	assert.Equal(t, "U_FU", list[1].Handle("slack"))

	// Empty tenant returns empty slice cleanly.
	emptyList, err := store.List(ctx, "unknown_tenant")
	require.NoError(t, err)
	assert.Empty(t, emptyList)

	// LookupPeople by role ("IT Ops" normalizes to "it_ops") returns both members, scoped to tenant.
	itOps, err := store.LookupPeople(ctx, "spacelions", "IT Ops")
	require.NoError(t, err)
	require.Len(t, itOps, 2)
	assert.Empty(t, cmp.Diff([]string{"alex", "fu"}, []string{itOps[0].UserID, itOps[1].UserID}))
	assert.Equal(t, "LIN_FU", itOps[1].Handle("linear"))

	// LookupPeople by partial name.
	byName, err := store.LookupPeople(ctx, "spacelions", "zhang")
	require.NoError(t, err)
	require.Len(t, byName, 1)
	assert.Equal(t, "fu", byName[0].UserID)

	// LookupPeople empty query rejected.
	_, err = store.LookupPeople(ctx, "spacelions", "   ")
	require.Error(t, err)

	// Delete removes actor and cascades actor_identities cleanly.
	require.NoError(t, store.Delete(ctx, "spacelions", "fu"))
	_, err = store.Get(ctx, "spacelions", "fu")
	require.ErrorIs(t, err, actors.ErrActorNotFound)
	_, err = store.GetByHandle(ctx, "spacelions", "slack", "U_FU")
	require.ErrorIs(t, err, actors.ErrActorNotFound)

	// Canceled context propagates DB errors cleanly.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, store.Upsert(canceledCtx, actors.Actor{TenantID: "spacelions", UserID: "x", Name: "X", Email: "x@spacelions.com"}))
	_, err = store.Get(canceledCtx, "spacelions", "alex")
	require.Error(t, err)
	_, err = store.GetByEmail(canceledCtx, "spacelions", "alex@spacelions.com")
	require.Error(t, err)
	_, err = store.GetByHandle(canceledCtx, "spacelions", "slack", "U_FU")
	require.Error(t, err)
	_, err = store.List(canceledCtx, "spacelions")
	require.Error(t, err)
	require.Error(t, store.Delete(canceledCtx, "spacelions", "alex"))
	_, err = store.LookupPeople(canceledCtx, "spacelions", "it_ops")
	require.Error(t, err)
}

func TestRecipientGuard_AllowsDirectoryAndTrustedExternal_BlocksHallucinated(t *testing.T) {
	_, database := testpg.NewMigrated(t)
	store := actors.NewStore(database)
	ctx := context.Background()

	require.NoError(t, store.Upsert(ctx, actors.Actor{
		TenantID: "spacelions",
		UserID:   "fu",
		Name:     "Fu Zhang",
		Email:    "fu@spacelions.com",
		Roles:    []string{"it_ops"},
		Handles: map[string]string{
			"slack":  "U_FU",
			"notion": "N_FU",
			"linear": "LIN_FU",
		},
	}))
	require.NoError(t, store.Upsert(ctx, actors.Actor{
		TenantID: "spacelions",
		UserID:   "email_only",
		Name:     "Email Only",
		Email:    "emailonly@spacelions.com",
	}))

	// Bad initial external email fails guard creation.
	_, err := actors.NewRecipientGuard(store, "spacelions", "not-an-email")
	require.Error(t, err)

	guard, err := actors.NewRecipientGuard(store, "spacelions", "alice.personal@gmail.com")
	require.NoError(t, err)

	// 1. Directory actor authorized by user_id and by email across gmail, email, slack_dm, notion, and arbitrary new providers like linear.
	a, err := guard.Authorize(ctx, "fu", actors.ChannelGmail)
	require.NoError(t, err)
	assert.False(t, a.External)
	assert.Equal(t, "fu@spacelions.com", a.Email)

	a, err = guard.Authorize(ctx, "fu", actors.ChannelEmail)
	require.NoError(t, err)
	assert.Equal(t, "fu@spacelions.com", a.Email)

	a, err = guard.Authorize(ctx, "fu@spacelions.com", actors.ChannelSlackDM)
	require.NoError(t, err)
	assert.Equal(t, "U_FU", a.Handle("slack"))

	a, err = guard.Authorize(ctx, "fu", actors.ChannelNotion)
	require.NoError(t, err)
	assert.Equal(t, "N_FU", a.Handle("notion"))

	a, err = guard.Authorize(ctx, "fu", "linear")
	require.NoError(t, err)
	assert.Equal(t, "LIN_FU", a.Handle("linear"))

	// 2. Directory actor missing handles fails with ErrMissingChannelHandle (for Slack, Notion, or any future provider like github).
	_, err = guard.Authorize(ctx, "email_only", actors.ChannelSlackDM)
	require.ErrorIs(t, err, actors.ErrMissingChannelHandle)
	_, err = guard.Authorize(ctx, "email_only", actors.ChannelNotion)
	require.ErrorIs(t, err, actors.ErrMissingChannelHandle)
	_, err = guard.Authorize(ctx, "fu", "github")
	require.ErrorIs(t, err, actors.ErrMissingChannelHandle)

	// 3. Trusted external email allowed for Gmail (External: true), blocked for connector channels.
	ext, err := guard.Authorize(ctx, "ALICE.PERSONAL@gmail.com", actors.ChannelGmail)
	require.NoError(t, err)
	assert.True(t, ext.External)
	assert.Equal(t, "alice.personal@gmail.com", ext.Email)

	_, err = guard.Authorize(ctx, "alice.personal@gmail.com", actors.ChannelSlackDM)
	require.ErrorIs(t, err, actors.ErrMissingChannelHandle)

	// 4. Hallucinated / unauthorized email or unknown user_id MUST be rejected.
	_, err = guard.Authorize(ctx, "attacker@evil.com", actors.ChannelGmail)
	require.ErrorIs(t, err, actors.ErrRecipientNotAllowed)
	_, err = guard.Authorize(ctx, "ghost_user", actors.ChannelGmail)
	require.ErrorIs(t, err, actors.ErrRecipientNotAllowed)
	_, err = guard.Authorize(ctx, "", actors.ChannelGmail)
	require.ErrorIs(t, err, actors.ErrRecipientNotAllowed)
	_, err = guard.Authorize(ctx, "bad@@email.com", actors.ChannelGmail)
	require.ErrorIs(t, err, actors.ErrRecipientNotAllowed)

	// 5. Dynamically adding a newly provisioned work email allows contacting it.
	require.NoError(t, guard.AllowEmail("alice@spacelions.com"))
	work, err := guard.Authorize(ctx, "alice@spacelions.com", actors.ChannelGmail)
	require.NoError(t, err)
	assert.Equal(t, "alice@spacelions.com", work.Email)

	// 6. Empty channel rejected.
	_, err = guard.Authorize(ctx, "fu", "   ")
	require.Error(t, err)

	// 7. DB error propagates cleanly instead of masking as not allowed.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = guard.Authorize(canceledCtx, "fu", actors.ChannelGmail)
	require.Error(t, err)
	_, err = guard.Authorize(canceledCtx, "fu@spacelions.com", actors.ChannelGmail)
	require.Error(t, err)
}
