package config

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spacelions/eunomia/internal/secrets"
)

func TestLoadFromViper_SuccessAndRedaction(t *testing.T) {
	key, err := secrets.GenerateLocalKey("v1")
	require.NoError(t, err)

	v := NewViper()
	v.Set("database-url", "postgres://postgres:supersecretpw@127.0.0.1:54329/eunomia?sslmode=disable")
	v.Set("master-keys", key)
	v.Set("deepseek-api-key", "sk-secret-deepseek-token")

	cfg, err := LoadFromViper(v)
	require.NoError(t, err)

	want := &Config{
		GRPCAddr:          "127.0.0.1:8080",
		DatabaseURL:       "postgres://postgres:supersecretpw@127.0.0.1:54329/eunomia?sslmode=disable",
		TemporalAddr:      "127.0.0.1:7233",
		TemporalNamespace: "default",
		TemporalTaskQueue: "eunomia",
		DefaultTenant:     "spacelions",
		MasterKeys:        key,
		DeepSeekAPIKey:    "sk-secret-deepseek-token",
	}
	assert.Empty(t, cmp.Diff(want, cfg))

	// Ensure secrets are never leaked via String() or slog.
	for _, secret := range []string{"supersecretpw", key, "sk-secret"} {
		assert.NotContains(t, cfg.String(), secret)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("boot", "config", cfg)
	for _, secret := range []string{"supersecretpw", key, "sk-secret"} {
		assert.NotContains(t, buf.String(), secret)
	}
}

func TestRegisterFlags_OverridesEnv(t *testing.T) {
	key, err := secrets.GenerateLocalKey("v1")
	require.NoError(t, err)

	v := NewViper()
	fs := pflag.NewFlagSet("eunomia", pflag.ContinueOnError)
	RegisterFlags(fs, v)
	v.Set("master-keys", key)

	require.NoError(t, fs.Parse([]string{
		"--grpc-addr=127.0.0.1:9090",
		"--database-url=postgresql://postgres@127.0.0.1:54329/eunomia",
		"--temporal-addr=127.0.0.1:7234",
		"--temporal-namespace=prod",
		"--temporal-task-queue=eunomia-prod",
		"--default-tenant=acme",
	}))

	cfg, err := LoadFromViper(v)
	require.NoError(t, err)

	want := &Config{
		GRPCAddr:          "127.0.0.1:9090",
		DatabaseURL:       "postgresql://postgres@127.0.0.1:54329/eunomia",
		TemporalAddr:      "127.0.0.1:7234",
		TemporalNamespace: "prod",
		TemporalTaskQueue: "eunomia-prod",
		DefaultTenant:     "acme",
		MasterKeys:        key,
	}
	assert.Empty(t, cmp.Diff(want, cfg))
}

func TestLoadFromViper_FailFastValidation(t *testing.T) {
	v := NewViper()
	v.Set("database-url", "")
	v.Set("master-keys", "")
	v.Set("grpc-addr", "invalid-no-port")
	v.Set("temporal-addr", "invalid-no-port")
	v.Set("default-tenant", "")

	_, err := LoadFromViper(v)
	require.Error(t, err)
	for _, want := range []string{
		"EUNOMIA_DATABASE_URL",
		"EUNOMIA_GRPC_ADDR",
		"EUNOMIA_TEMPORAL_ADDR",
		"EUNOMIA_DEFAULT_TENANT",
		"EUNOMIA_MASTER_KEYS",
	} {
		assert.ErrorContains(t, err, want)
	}
}

func TestLoadFromViper_InvalidURLAndMasterKey(t *testing.T) {
	v := NewViper()
	v.Set("database-url", "mysql://localhost/eunomia")
	v.Set("master-keys", "v1:not-valid-base64!!!")

	err := (&Config{
		DatabaseURL: "mysql://localhost/eunomia",
		MasterKeys:  "v1:not-valid-base64!!!",
	}).ValidateForServe()
	require.ErrorContains(t, err, "postgres://")

	_, err = LoadFromViper(v)
	require.Error(t, err)
	assert.Equal(t, "[INVALID_URL]", redactDatabaseURL("://bad-url"))
}

func TestLoadForMigrateAndSecrets(t *testing.T) {
	key, err := secrets.GenerateLocalKey("v1")
	require.NoError(t, err)

	v := NewViper()
	v.Set("database-url", "")
	_, err = LoadForMigrateFromViper(v)
	require.Error(t, err)

	v.Set("database-url", "postgres://postgres@127.0.0.1:54329/eunomia?sslmode=disable")
	v.Set("master-keys", "")
	_, err = LoadForMigrateFromViper(v)
	require.NoError(t, err)
	_, err = LoadForSecretsFromViper(v)
	require.Error(t, err)

	v.Set("master-keys", key)
	_, err = LoadForSecretsFromViper(v)
	require.NoError(t, err)

	vBadDB := NewViper()
	vBadDB.Set("database-url", "")
	vBadDB.Set("master-keys", key)
	_, err = LoadForSecretsFromViper(vBadDB)
	require.Error(t, err)
}
