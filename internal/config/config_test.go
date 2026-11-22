package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/spacelions/eunomia/internal/secrets"
)

func TestLoadFromViper_SuccessAndRedaction(t *testing.T) {
	key, err := secrets.GenerateLocalKey("v1")
	if err != nil {
		t.Fatalf("GenerateLocalKey: %v", err)
	}

	v := NewViper()
	v.Set("database-url", "postgres://postgres:supersecretpw@127.0.0.1:54329/eunomia?sslmode=disable")
	v.Set("master-keys", key)
	v.Set("deepseek-api-key", "sk-secret-deepseek-token")

	cfg, err := LoadFromViper(v)
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if cfg.GRPCAddr != "127.0.0.1:8080" {
		t.Fatalf("expected default grpc addr, got %q", cfg.GRPCAddr)
	}
	if cfg.DefaultTenant != "spacelions" {
		t.Fatalf("expected default tenant spacelions, got %q", cfg.DefaultTenant)
	}

	// Ensure secrets are never leaked via String() or slog.
	if strings.Contains(cfg.String(), "supersecretpw") || strings.Contains(cfg.String(), key) || strings.Contains(cfg.String(), "sk-secret") {
		t.Fatalf("Config.String() leaked secret material: %s", cfg.String())
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("boot", "config", cfg)
	logged := buf.String()
	if strings.Contains(logged, "supersecretpw") || strings.Contains(logged, key) || strings.Contains(logged, "sk-secret") {
		t.Fatalf("slog output leaked secret material: %s", logged)
	}
}

func TestLoadFromViper_FailFastValidation(t *testing.T) {
	v := NewViper()
	v.Set("database-url", "")
	v.Set("master-keys", "")

	_, err := LoadFromViper(v)
	if err == nil {
		t.Fatal("expected error when required fields are missing")
	}
	msg := err.Error()
	if !strings.Contains(msg, "EUNOMIA_DATABASE_URL") {
		t.Fatalf("expected missing EUNOMIA_DATABASE_URL error, got: %s", msg)
	}
	if !strings.Contains(msg, "EUNOMIA_MASTER_KEYS") {
		t.Fatalf("expected missing EUNOMIA_MASTER_KEYS error, got: %s", msg)
	}
}

func TestLoadFromViper_InvalidMasterKey(t *testing.T) {
	v := NewViper()
	v.Set("database-url", "postgres://postgres@127.0.0.1:54329/eunomia?sslmode=disable")
	v.Set("master-keys", "v1:not-valid-base64!!!")

	if _, err := LoadFromViper(v); err == nil {
		t.Fatal("expected error for malformed EUNOMIA_MASTER_KEYS")
	}
}

func TestLoadForMigrateFromViper_DoesNotRequireMasterKeys(t *testing.T) {
	v := NewViper()
	v.Set("database-url", "postgres://postgres@127.0.0.1:54329/eunomia?sslmode=disable")
	v.Set("master-keys", "")

	if _, err := LoadForMigrateFromViper(v); err != nil {
		t.Fatalf("migrate config should not require master keys: %v", err)
	}
}
