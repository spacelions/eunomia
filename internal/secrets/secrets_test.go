package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
)

func TestLocalKeyWrapper_ParseErrors(t *testing.T) {
	shortKey := base64.StdEncoding.EncodeToString([]byte("too-short"))
	valid, err := GenerateLocalKey("v1")
	if err != nil {
		t.Fatalf("GenerateLocalKey: %v", err)
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
			if _, err := ParseLocalKeyWrapper(tc.spec); err == nil {
				t.Fatalf("expected error for spec %q", tc.spec)
			}
		})
	}
}

func TestEnvelope_RoundTrip_AADTamper_AndKeyRotation(t *testing.T) {
	ctx := context.Background()

	k1, err := GenerateLocalKey("v1")
	if err != nil {
		t.Fatalf("GenerateLocalKey v1: %v", err)
	}
	k2, err := GenerateLocalKey("v2")
	if err != nil {
		t.Fatalf("GenerateLocalKey v2: %v", err)
	}

	w1, err := ParseLocalKeyWrapper(k1)
	if err != nil {
		t.Fatalf("ParseLocalKeyWrapper v1: %v", err)
	}

	plaintext := []byte(`{"type":"service_account","private_key":"secret-pem-body"}`)
	env, err := EncryptEnvelope(ctx, w1, "spacelions", "google_workspace", "service_account_json", plaintext)
	if err != nil {
		t.Fatalf("EncryptEnvelope: %v", err)
	}
	if env.KeyID != "v1" {
		t.Fatalf("expected key_id v1, got %q", env.KeyID)
	}

	// Normal decrypt succeeds.
	got, err := DecryptEnvelope(ctx, w1, "spacelions", "google_workspace", "service_account_json", *env)
	if err != nil {
		t.Fatalf("DecryptEnvelope: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("plaintext mismatch: got %q, want %q", got, plaintext)
	}

	// Cross-tenant AAD tamper must fail.
	if _, err := DecryptEnvelope(ctx, w1, "other-tenant", "google_workspace", "service_account_json", *env); err == nil {
		t.Fatal("expected AAD authentication failure when decrypting under a different tenant")
	}

	// Cross-connector AAD tamper must fail.
	if _, err := DecryptEnvelope(ctx, w1, "spacelions", "notion", "service_account_json", *env); err == nil {
		t.Fatal("expected AAD authentication failure when decrypting under a different connector")
	}

	// Zero-downtime rotation: w21 has v2 active, v1 still available for unwrap.
	w21, err := ParseLocalKeyWrapper(k2 + "," + k1)
	if err != nil {
		t.Fatalf("ParseLocalKeyWrapper v2+v1: %v", err)
	}
	if w21.ActiveKeyID() != "v2" {
		t.Fatalf("expected active key v2, got %q", w21.ActiveKeyID())
	}

	// Old v1 envelope decrypts cleanly under w21.
	got, err = DecryptEnvelope(ctx, w21, "spacelions", "google_workspace", "service_account_json", *env)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("decrypt old v1 row with v2+v1 keyring failed: %v", err)
	}

	// Re-wrap only the 32-byte DEK to v2, then retire v1 completely.
	dek, err := w21.Unwrap(ctx, env.KeyID, env.EncryptedDEK)
	if err != nil {
		t.Fatalf("Unwrap v1 DEK: %v", err)
	}
	newKeyID, newWrappedDEK, err := w21.Wrap(ctx, dek)
	if err != nil {
		t.Fatalf("Wrap v2 DEK: %v", err)
	}
	env.KeyID = newKeyID
	env.EncryptedDEK = newWrappedDEK

	w2Only, err := ParseLocalKeyWrapper(k2)
	if err != nil {
		t.Fatalf("ParseLocalKeyWrapper v2 only: %v", err)
	}
	got, err = DecryptEnvelope(ctx, w2Only, "spacelions", "google_workspace", "service_account_json", *env)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("decrypt rotated envelope with v2-only keyring failed: %v", err)
	}
}
