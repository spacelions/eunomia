package secrets_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/secrets"
	"github.com/spacelions/eunomia/test/testpg"
)

func TestLocalKeyWrapper_ParseAndGenerateErrors(t *testing.T) {
	shortKey := base64.StdEncoding.EncodeToString([]byte("too-short"))
	valid, err := secrets.GenerateLocalKey("v1")
	require.NoError(t, err)

	_, err = secrets.GenerateLocalKey("")
	require.Error(t, err)
	_, err = secrets.GenerateLocalKey("bad:id")
	require.Error(t, err)

	cases := []struct {
		name string
		spec string
	}{
		{"empty", ""},
		{"missing colon", "v1abc"},
		{"short key", "v1:" + shortKey},
		{"bad base64", "v1:!!!"},
		{"duplicate id", valid + "," + valid},
		{"trailing comma", valid + ","},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := secrets.ParseLocalKeyWrapper(tc.spec)
			require.Error(t, err)
		})
	}

	w, err := secrets.ParseLocalKeyWrapper(valid)
	require.NoError(t, err)
	ctx := context.Background()

	_, _, err = w.Wrap(ctx, []byte("short"))
	require.Error(t, err)
	_, err = w.Unwrap(ctx, "unknown", []byte("123456789012345"))
	require.Error(t, err)
	_, err = w.Unwrap(ctx, "v1", []byte("short"))
	require.Error(t, err)
	_, err = w.Unwrap(ctx, "v1", make([]byte, 40))
	require.Error(t, err)
}

func TestEnvelope_Validation_AADTamper_AndKeyRotation(t *testing.T) {
	ctx := context.Background()

	k1, err := secrets.GenerateLocalKey("v1")
	require.NoError(t, err)
	k2, err := secrets.GenerateLocalKey("v2")
	require.NoError(t, err)

	w1, err := secrets.ParseLocalKeyWrapper(k1)
	require.NoError(t, err)

	_, err = secrets.EncryptEnvelope(ctx, w1, "", "notion", "token", []byte("x"))
	require.Error(t, err)
	_, err = secrets.EncryptEnvelope(ctx, w1, "spacelions", "", "token", []byte("x"))
	require.Error(t, err)
	_, err = secrets.EncryptEnvelope(ctx, w1, "spacelions", "notion", "", []byte("x"))
	require.Error(t, err)
	_, err = secrets.EncryptEnvelope(ctx, w1, "spacelions", "notion", "token", nil)
	require.Error(t, err)
	_, err = secrets.DecryptEnvelope(ctx, w1, "", "notion", "token", secrets.Envelope{})
	require.Error(t, err)

	plaintext := []byte(`{"type":"service_account","private_key":"secret-pem-body"}`)
	env, err := secrets.EncryptEnvelope(ctx, w1, "spacelions", "google_workspace", "service_account_json", plaintext)
	require.NoError(t, err)
	assert.Equal(t, "v1", env.KeyID)

	got, err := secrets.DecryptEnvelope(ctx, w1, "spacelions", "google_workspace", "service_account_json", *env)
	require.NoError(t, err)
	assert.Empty(t, cmp.Diff(plaintext, got))

	// Cross-tenant and cross-connector AAD tamper must fail.
	_, err = secrets.DecryptEnvelope(ctx, w1, "other-tenant", "google_workspace", "service_account_json", *env)
	require.Error(t, err)
	_, err = secrets.DecryptEnvelope(ctx, w1, "spacelions", "notion", "service_account_json", *env)
	require.Error(t, err)

	// Unknown key in DecryptEnvelope must fail.
	w2Only, err := secrets.ParseLocalKeyWrapper(k2)
	require.NoError(t, err)
	_, err = secrets.DecryptEnvelope(ctx, w2Only, "spacelions", "google_workspace", "service_account_json", *env)
	require.Error(t, err)
}

func TestDBSecretProvider_CRUD_TenantIsolation_AndMasterKeyRotation(t *testing.T) {
	_, database := testpg.NewMigrated(t)
	ctx := context.Background()

	k1, err := secrets.GenerateLocalKey("v1")
	require.NoError(t, err)
	k2, err := secrets.GenerateLocalKey("v2")
	require.NoError(t, err)

	w1, err := secrets.ParseLocalKeyWrapper(k1)
	require.NoError(t, err)
	p1 := secrets.NewDBSecretProvider(database, w1)

	require.Error(t, p1.Set(ctx, "", "notion", "token", []byte("x")))
	_, err = p1.Get(ctx, "", "notion", "token")
	require.Error(t, err)
	_, err = p1.Get(ctx, "spacelions", "notion", "token")
	require.ErrorIs(t, err, secrets.ErrSecretNotFound)
	_, err = p1.List(ctx, "")
	require.Error(t, err)
	require.Error(t, p1.Delete(ctx, "", "notion", "token"))
	require.ErrorIs(t, p1.Delete(ctx, "spacelions", "notion", "token"), secrets.ErrSecretNotFound)

	// Set, overwrite, and store across two tenants.
	require.NoError(t, p1.Set(ctx, "spacelions", "notion", "token", []byte("old-token")))
	require.NoError(t, p1.Set(ctx, "spacelions", "notion", "token", []byte("new-token")))
	require.NoError(t, p1.Set(ctx, "other", "slack", "bot_token", []byte("xoxb-other")))

	val, err := p1.Get(ctx, "spacelions", "notion", "token")
	require.NoError(t, err)
	assert.Equal(t, "new-token", string(val))

	meta, err := p1.List(ctx, "spacelions")
	require.NoError(t, err)
	require.Len(t, meta, 1)
	assert.Equal(t, "notion", meta[0].Connector)
	assert.Equal(t, "token", meta[0].Key)
	assert.Equal(t, "v1", meta[0].KeyID)

	// Rotate master key from v1 -> v2 using v2,v1 keyring.
	w21, err := secrets.ParseLocalKeyWrapper(k2 + "," + k1)
	require.NoError(t, err)
	p21 := secrets.NewDBSecretProvider(database, w21)

	rotated, err := p21.RotateMasterKey(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, rotated)

	rotated2, err := p21.RotateMasterKey(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, rotated2)

	// Retire v1 completely and verify both secrets still decrypt under v2 only.
	w2Only, err := secrets.ParseLocalKeyWrapper(k2)
	require.NoError(t, err)
	p2 := secrets.NewDBSecretProvider(database, w2Only)

	val, err = p2.Get(ctx, "spacelions", "notion", "token")
	require.NoError(t, err)
	assert.Equal(t, "new-token", string(val))

	otherVal, err := p2.Get(ctx, "other", "slack", "bot_token")
	require.NoError(t, err)
	assert.Equal(t, "xoxb-other", string(otherVal))

	require.NoError(t, p2.Delete(ctx, "spacelions", "notion", "token"))
	_, err = p2.Get(ctx, "spacelions", "notion", "token")
	require.ErrorIs(t, err, secrets.ErrSecretNotFound)

	// Rotating when stored row uses a retired/unknown key must fail cleanly.
	k3, err := secrets.GenerateLocalKey("v3")
	require.NoError(t, err)
	w3Only, err := secrets.ParseLocalKeyWrapper(k3)
	require.NoError(t, err)
	_, err = secrets.NewDBSecretProvider(database, w3Only).RotateMasterKey(ctx)
	require.Error(t, err)

	// Rotating when Wrap fails must roll back cleanly and preserve existing v2 row.
	wrapFailProvider := secrets.NewDBSecretProvider(database, unwrapOnlyWrapper{
		inner:   w2Only,
		wrapErr: errors.New("kms wrap failed"),
	})
	_, err = wrapFailProvider.RotateMasterKey(ctx)
	require.Error(t, err)
	otherVal, err = p2.Get(ctx, "other", "slack", "bot_token")
	require.NoError(t, err)
	assert.Equal(t, "xoxb-other", string(otherVal))

	// Canceled context propagates DB errors cleanly.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, p2.Set(canceledCtx, "spacelions", "notion", "token", []byte("x")))
	_, err = p2.Get(canceledCtx, "spacelions", "notion", "token")
	require.Error(t, err)
	_, err = p2.List(canceledCtx, "spacelions")
	require.Error(t, err)
	require.Error(t, p2.Delete(canceledCtx, "spacelions", "notion", "token"))
	_, err = p2.RotateMasterKey(canceledCtx)
	require.Error(t, err)
}

type failingWrapper struct {
	wrapErr   error
	unwrapErr error
}

func (f failingWrapper) ActiveKeyID() string { return "fail" }
func (f failingWrapper) Wrap(_ context.Context, _ []byte) (string, []byte, error) {
	return "", nil, f.wrapErr
}
func (f failingWrapper) Unwrap(_ context.Context, _ string, _ []byte) ([]byte, error) {
	return nil, f.unwrapErr
}

type unwrapOnlyWrapper struct {
	inner   secrets.KeyWrapper
	wrapErr error
}

func (u unwrapOnlyWrapper) ActiveKeyID() string { return "v-broken" }
func (u unwrapOnlyWrapper) Wrap(_ context.Context, _ []byte) (string, []byte, error) {
	return "", nil, u.wrapErr
}
func (u unwrapOnlyWrapper) Unwrap(ctx context.Context, keyID string, wrappedDEK []byte) ([]byte, error) {
	return u.inner.Unwrap(ctx, keyID, wrappedDEK)
}

func TestEnvelope_WrapperErrors(t *testing.T) {
	ctx := context.Background()
	fw := failingWrapper{wrapErr: errors.New("kms down"), unwrapErr: errors.New("kms down")}
	_, err := secrets.EncryptEnvelope(ctx, fw, "t", "c", "k", []byte("val"))
	require.Error(t, err)
}
