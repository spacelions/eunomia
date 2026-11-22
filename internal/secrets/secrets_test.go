package secrets_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/spacelions/eunomia/internal/secrets"
	"github.com/spacelions/eunomia/internal/testpg"
)

func TestLocalKeyWrapper_ParseAndGenerateErrors(t *testing.T) {
	shortKey := base64.StdEncoding.EncodeToString([]byte("too-short"))
	valid, err := secrets.GenerateLocalKey("v1")
	if err != nil {
		t.Fatalf("GenerateLocalKey: %v", err)
	}

	if _, err := secrets.GenerateLocalKey(""); err == nil {
		t.Fatal("expected error for empty key id")
	}
	if _, err := secrets.GenerateLocalKey("bad:id"); err == nil {
		t.Fatal("expected error for invalid key id chars")
	}

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
			if _, err := secrets.ParseLocalKeyWrapper(tc.spec); err == nil {
				t.Fatalf("expected error for spec %q", tc.spec)
			}
		})
	}

	w, err := secrets.ParseLocalKeyWrapper(valid)
	if err != nil {
		t.Fatalf("ParseLocalKeyWrapper: %v", err)
	}
	ctx := context.Background()
	if _, _, err := w.Wrap(ctx, []byte("short")); err == nil {
		t.Fatal("expected Wrap error for wrong DEK length")
	}
	if _, err := w.Unwrap(ctx, "unknown", []byte("123456789012345")); err == nil {
		t.Fatal("expected Unwrap error for unknown key_id")
	}
	if _, err := w.Unwrap(ctx, "v1", []byte("short")); err == nil {
		t.Fatal("expected Unwrap error for truncated wrapped DEK")
	}
	if _, err := w.Unwrap(ctx, "v1", make([]byte, 40)); err == nil {
		t.Fatal("expected Unwrap error for corrupted wrapped DEK")
	}
}

func TestEnvelope_Validation_AADTamper_AndKeyRotation(t *testing.T) {
	ctx := context.Background()

	k1, err := secrets.GenerateLocalKey("v1")
	if err != nil {
		t.Fatalf("GenerateLocalKey v1: %v", err)
	}
	k2, err := secrets.GenerateLocalKey("v2")
	if err != nil {
		t.Fatalf("GenerateLocalKey v2: %v", err)
	}

	w1, err := secrets.ParseLocalKeyWrapper(k1)
	if err != nil {
		t.Fatalf("ParseLocalKeyWrapper v1: %v", err)
	}

	// Coordinate and value validation.
	if _, err := secrets.EncryptEnvelope(ctx, w1, "", "notion", "token", []byte("x")); err == nil {
		t.Fatal("expected error for empty tenant_id")
	}
	if _, err := secrets.EncryptEnvelope(ctx, w1, "spacelions", "", "token", []byte("x")); err == nil {
		t.Fatal("expected error for empty connector")
	}
	if _, err := secrets.EncryptEnvelope(ctx, w1, "spacelions", "notion", "", []byte("x")); err == nil {
		t.Fatal("expected error for empty key")
	}
	if _, err := secrets.EncryptEnvelope(ctx, w1, "spacelions", "notion", "token", nil); err == nil {
		t.Fatal("expected error for empty secret value")
	}
	if _, err := secrets.DecryptEnvelope(ctx, w1, "", "notion", "token", secrets.Envelope{}); err == nil {
		t.Fatal("expected error for empty tenant_id on decrypt")
	}

	plaintext := []byte(`{"type":"service_account","private_key":"secret-pem-body"}`)
	env, err := secrets.EncryptEnvelope(ctx, w1, "spacelions", "google_workspace", "service_account_json", plaintext)
	if err != nil {
		t.Fatalf("EncryptEnvelope: %v", err)
	}
	if env.KeyID != "v1" {
		t.Fatalf("expected key_id v1, got %q", env.KeyID)
	}

	got, err := secrets.DecryptEnvelope(ctx, w1, "spacelions", "google_workspace", "service_account_json", *env)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("DecryptEnvelope mismatch: err=%v got=%q", err, got)
	}

	// Cross-tenant and cross-connector AAD tamper must fail.
	if _, err := secrets.DecryptEnvelope(ctx, w1, "other-tenant", "google_workspace", "service_account_json", *env); err == nil {
		t.Fatal("expected AAD auth failure across tenants")
	}
	if _, err := secrets.DecryptEnvelope(ctx, w1, "spacelions", "notion", "service_account_json", *env); err == nil {
		t.Fatal("expected AAD auth failure across connectors")
	}

	// Unknown key in DecryptEnvelope must fail.
	w2Only, err := secrets.ParseLocalKeyWrapper(k2)
	if err != nil {
		t.Fatalf("ParseLocalKeyWrapper v2: %v", err)
	}
	if _, err := secrets.DecryptEnvelope(ctx, w2Only, "spacelions", "google_workspace", "service_account_json", *env); err == nil {
		t.Fatal("expected unwrap failure when v1 is missing")
	}
}

func TestDBSecretProvider_CRUD_TenantIsolation_AndMasterKeyRotation(t *testing.T) {
	_, database := testpg.NewMigrated(t)
	ctx := context.Background()

	k1, err := secrets.GenerateLocalKey("v1")
	if err != nil {
		t.Fatalf("GenerateLocalKey v1: %v", err)
	}
	k2, err := secrets.GenerateLocalKey("v2")
	if err != nil {
		t.Fatalf("GenerateLocalKey v2: %v", err)
	}

	w1, err := secrets.ParseLocalKeyWrapper(k1)
	if err != nil {
		t.Fatalf("ParseLocalKeyWrapper v1: %v", err)
	}
	p1 := secrets.NewDBSecretProvider(database, w1)

	// Validation & not found errors.
	if err := p1.Set(ctx, "", "notion", "token", []byte("x")); err == nil {
		t.Fatal("expected Set validation error")
	}
	if _, err := p1.Get(ctx, "", "notion", "token"); err == nil {
		t.Fatal("expected Get validation error")
	}
	if _, err := p1.Get(ctx, "spacelions", "notion", "token"); !errors.Is(err, secrets.ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got: %v", err)
	}
	if _, err := p1.List(ctx, ""); err == nil {
		t.Fatal("expected List empty tenant error")
	}
	if err := p1.Delete(ctx, "", "notion", "token"); err == nil {
		t.Fatal("expected Delete validation error")
	}
	if err := p1.Delete(ctx, "spacelions", "notion", "token"); !errors.Is(err, secrets.ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound on Delete, got: %v", err)
	}

	// Set, overwrite, and store across two tenants.
	if err := p1.Set(ctx, "spacelions", "notion", "token", []byte("old-token")); err != nil {
		t.Fatalf("Set initial: %v", err)
	}
	if err := p1.Set(ctx, "spacelions", "notion", "token", []byte("new-token")); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	if err := p1.Set(ctx, "other", "slack", "bot_token", []byte("xoxb-other")); err != nil {
		t.Fatalf("Set other tenant: %v", err)
	}

	val, err := p1.Get(ctx, "spacelions", "notion", "token")
	if err != nil || string(val) != "new-token" {
		t.Fatalf("Get failed: err=%v val=%q", err, val)
	}

	meta, err := p1.List(ctx, "spacelions")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(meta) != 1 || meta[0].Connector != "notion" || meta[0].Key != "token" || meta[0].KeyID != "v1" {
		t.Fatalf("unexpected List metadata: %+v", meta)
	}

	// Rotate master key from v1 -> v2 using v2,v1 keyring.
	w21, err := secrets.ParseLocalKeyWrapper(k2 + "," + k1)
	if err != nil {
		t.Fatalf("ParseLocalKeyWrapper v2,v1: %v", err)
	}
	p21 := secrets.NewDBSecretProvider(database, w21)

	rotated, err := p21.RotateMasterKey(ctx)
	if err != nil {
		t.Fatalf("RotateMasterKey: %v", err)
	}
	if rotated != 2 {
		t.Fatalf("expected 2 rotated secrets across tenants, got %d", rotated)
	}

	// Second rotation is a no-op.
	rotated2, err := p21.RotateMasterKey(ctx)
	if err != nil || rotated2 != 0 {
		t.Fatalf("expected 0 rotated on second run, got %d (err=%v)", rotated2, err)
	}

	// Retire v1 completely and verify both secrets still decrypt under v2 only.
	w2Only, err := secrets.ParseLocalKeyWrapper(k2)
	if err != nil {
		t.Fatalf("ParseLocalKeyWrapper v2: %v", err)
	}
	p2 := secrets.NewDBSecretProvider(database, w2Only)
	val, err = p2.Get(ctx, "spacelions", "notion", "token")
	if err != nil || string(val) != "new-token" {
		t.Fatalf("Get after retiring v1 failed: err=%v val=%q", err, val)
	}
	otherVal, err := p2.Get(ctx, "other", "slack", "bot_token")
	if err != nil || string(otherVal) != "xoxb-other" {
		t.Fatalf("Get other tenant after retiring v1 failed: err=%v val=%q", err, otherVal)
	}

	// Delete works cleanly.
	if err := p2.Delete(ctx, "spacelions", "notion", "token"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p2.Get(ctx, "spacelions", "notion", "token"); !errors.Is(err, secrets.ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound after delete, got: %v", err)
	}

	// Rotating when stored row uses a retired/unknown key must fail cleanly.
	k3, _ := secrets.GenerateLocalKey("v3")
	w3Only, _ := secrets.ParseLocalKeyWrapper(k3)
	p3 := secrets.NewDBSecretProvider(database, w3Only)
	if _, err := p3.RotateMasterKey(ctx); err == nil {
		t.Fatal("expected RotateMasterKey error when old key v2 is missing from keyring")
	}

	// Rotating when Wrap fails must roll back cleanly and preserve existing v2 row.
	wrapFailProvider := secrets.NewDBSecretProvider(database, unwrapOnlyWrapper{
		inner:   w2Only,
		wrapErr: errors.New("kms wrap failed"),
	})
	if _, err := wrapFailProvider.RotateMasterKey(ctx); err == nil {
		t.Fatal("expected RotateMasterKey error when Wrap fails")
	}
	if otherVal, err = p2.Get(ctx, "other", "slack", "bot_token"); err != nil || string(otherVal) != "xoxb-other" {
		t.Fatalf("expected secret to remain intact after failed rotation: err=%v val=%q", err, otherVal)
	}

	// Canceled context propagates DB errors cleanly.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := p2.Set(canceledCtx, "spacelions", "notion", "token", []byte("x")); err == nil {
		t.Fatal("expected Set error on canceled context")
	}
	if _, err := p2.Get(canceledCtx, "spacelions", "notion", "token"); err == nil {
		t.Fatal("expected Get error on canceled context")
	}
	if _, err := p2.List(canceledCtx, "spacelions"); err == nil {
		t.Fatal("expected List error on canceled context")
	}
	if err := p2.Delete(canceledCtx, "spacelions", "notion", "token"); err == nil {
		t.Fatal("expected Delete error on canceled context")
	}
	if _, err := p2.RotateMasterKey(canceledCtx); err == nil {
		t.Fatal("expected RotateMasterKey error on canceled context")
	}
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
	if _, err := secrets.EncryptEnvelope(ctx, fw, "t", "c", "k", []byte("val")); err == nil {
		t.Fatal("expected EncryptEnvelope error when KeyWrapper.Wrap fails")
	}
}
