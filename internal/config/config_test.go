package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/spf13/pflag"

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

func TestRegisterFlags_OverridesEnv(t *testing.T) {
	key, err := secrets.GenerateLocalKey("v1")
	if err != nil {
		t.Fatalf("GenerateLocalKey: %v", err)
	}

	v := NewViper()
	fs := pflag.NewFlagSet("eunomia", pflag.ContinueOnError)
	RegisterFlags(fs, v)
	v.Set("master-keys", key)

	if err := fs.Parse([]string{
		"--grpc-addr=127.0.0.1:9090",
		"--database-url=postgresql://postgres@127.0.0.1:54329/eunomia",
		"--temporal-addr=127.0.0.1:7234",
		"--temporal-namespace=prod",
		"--temporal-task-queue=eunomia-prod",
		"--default-tenant=acme",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}

	cfg, err := LoadFromViper(v)
	if err != nil {
		t.Fatalf("LoadFromViper: %v", err)
	}
	if cfg.GRPCAddr != "127.0.0.1:9090" || cfg.TemporalNamespace != "prod" || cfg.TemporalTaskQueue != "eunomia-prod" || cfg.DefaultTenant != "acme" {
		t.Fatalf("flag overrides not applied: %+v", cfg)
	}
}

func TestLoadFromViper_FailFastValidation(t *testing.T) {
	v := NewViper()
	v.Set("database-url", "")
	v.Set("master-keys", "")
	v.Set("grpc-addr", "invalid-no-port")
	v.Set("temporal-addr", "invalid-no-port")
	v.Set("default-tenant", "")

	_, err := LoadFromViper(v)
	if err == nil {
		t.Fatal("expected error when required fields are missing/invalid")
	}
	msg := err.Error()
	for _, want := range []string{
		"EUNOMIA_DATABASE_URL",
		"EUNOMIA_GRPC_ADDR",
		"EUNOMIA_TEMPORAL_ADDR",
		"EUNOMIA_DEFAULT_TENANT",
		"EUNOMIA_MASTER_KEYS",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error mentioning %s, got: %s", want, msg)
		}
	}
}

func TestLoadFromViper_InvalidURLAndMasterKey(t *testing.T) {
	v := NewViper()
	v.Set("database-url", "mysql://localhost/eunomia")
	v.Set("master-keys", "v1:not-valid-base64!!!")

	err := (&Config{DatabaseURL: "mysql://localhost/eunomia", MasterKeys: "v1:not-valid-base64!!!"}).ValidateForServe()
	if err == nil || !strings.Contains(err.Error(), "postgres://") {
		t.Fatalf("expected invalid postgres URL error, got: %v", err)
	}

	if _, err := LoadFromViper(v); err == nil {
		t.Fatal("expected error for invalid URL and master key")
	}

	if got := redactDatabaseURL("://bad-url"); got != "[INVALID_URL]" {
		t.Fatalf("expected [INVALID_URL], got %q", got)
	}
}

func TestLoadForMigrateAndSecrets(t *testing.T) {
	key, err := secrets.GenerateLocalKey("v1")
	if err != nil {
		t.Fatalf("GenerateLocalKey: %v", err)
	}

	v := NewViper()
	v.Set("database-url", "")
	if _, err := LoadForMigrateFromViper(v); err == nil {
		t.Fatal("expected migrate error when database URL is missing")
	}

	v.Set("database-url", "postgres://postgres@127.0.0.1:54329/eunomia?sslmode=disable")
	v.Set("master-keys", "")
	if _, err := LoadForMigrateFromViper(v); err != nil {
		t.Fatalf("migrate config should not require master keys: %v", err)
	}
	if _, err := LoadForSecretsFromViper(v); err == nil {
		t.Fatal("secrets config must require master keys")
	}

	v.Set("master-keys", key)
	if _, err := LoadForSecretsFromViper(v); err != nil {
		t.Fatalf("expected valid secrets config, got: %v", err)
	}

	vBadDB := NewViper()
	vBadDB.Set("database-url", "")
	vBadDB.Set("master-keys", key)
	if _, err := LoadForSecretsFromViper(vBadDB); err == nil {
		t.Fatal("secrets config must require database URL")
	}
}
