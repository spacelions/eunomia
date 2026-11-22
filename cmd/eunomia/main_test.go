package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spacelions/eunomia/internal/secrets"
)

func TestSecretKeygenCommand(t *testing.T) {
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"secret", "keygen", "--id", "v2"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("secret keygen failed: %v", err)
	}

	out := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(out, "v2:") {
		t.Fatalf("expected v2: prefix, got %q", out)
	}
	if _, err := secrets.ParseLocalKeyWrapper(out); err != nil {
		t.Fatalf("generated key failed to parse: %v", err)
	}
}

func TestParseKVPairs(t *testing.T) {
	got, err := parseKVPairs([]string{"domain=spacelions.com", "impersonate_email=ops@spacelions.com"})
	if err != nil {
		t.Fatalf("parseKVPairs: %v", err)
	}
	if got["domain"] != "spacelions.com" || got["impersonate_email"] != "ops@spacelions.com" {
		t.Fatalf("unexpected map: %+v", got)
	}

	if _, err := parseKVPairs([]string{"invalid-no-equals"}); err == nil {
		t.Fatal("expected error for missing =")
	}
}
