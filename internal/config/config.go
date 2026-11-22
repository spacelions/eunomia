package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/spacelions/eunomia/internal/secrets"
)

// Config holds runtime engine boot configuration loaded via Viper (flags + env vars).
// Connector settings and credentials live in PostgreSQL (tenant_connectors / tenant_secrets).
type Config struct {
	GRPCAddr          string
	DatabaseURL       string
	TemporalAddr      string
	TemporalNamespace string
	TemporalTaskQueue string
	DefaultTenant     string
	MasterKeys        string
	DeepSeekAPIKey    string
}

// NewViper creates an isolated Viper instance configured for Eunomia's env vars and defaults.
func NewViper() *viper.Viper {
	v := viper.New()
	v.SetEnvPrefix("EUNOMIA")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()

	v.SetDefault("grpc-addr", "127.0.0.1:8080")
	v.SetDefault("temporal-addr", "127.0.0.1:7233")
	v.SetDefault("temporal-namespace", "default")
	v.SetDefault("temporal-task-queue", "eunomia")
	v.SetDefault("default-tenant", "spacelions")

	_ = v.BindEnv("grpc-addr", "EUNOMIA_GRPC_ADDR")
	_ = v.BindEnv("database-url", "EUNOMIA_DATABASE_URL")
	_ = v.BindEnv("temporal-addr", "EUNOMIA_TEMPORAL_ADDR")
	_ = v.BindEnv("temporal-namespace", "EUNOMIA_TEMPORAL_NAMESPACE")
	_ = v.BindEnv("temporal-task-queue", "EUNOMIA_TEMPORAL_TASK_QUEUE")
	_ = v.BindEnv("default-tenant", "EUNOMIA_DEFAULT_TENANT")
	_ = v.BindEnv("master-keys", "EUNOMIA_MASTER_KEYS")
	_ = v.BindEnv("deepseek-api-key", "DEEPSEEK_API_KEY")

	return v
}

// RegisterFlags registers runtime flags on a Cobra pflag.FlagSet and binds them to v.
func RegisterFlags(fs *pflag.FlagSet, v *viper.Viper) {
	fs.String("grpc-addr", "127.0.0.1:8080", "ConnectRPC listen address (env: EUNOMIA_GRPC_ADDR)")
	fs.String("database-url", "", "PostgreSQL connection URL (required, env: EUNOMIA_DATABASE_URL)")
	fs.String("temporal-addr", "127.0.0.1:7233", "Temporal server address (env: EUNOMIA_TEMPORAL_ADDR)")
	fs.String("temporal-namespace", "default", "Temporal namespace (env: EUNOMIA_TEMPORAL_NAMESPACE)")
	fs.String("temporal-task-queue", "eunomia", "Temporal task queue (env: EUNOMIA_TEMPORAL_TASK_QUEUE)")
	fs.String("default-tenant", "spacelions", "Default tenant ID (env: EUNOMIA_DEFAULT_TENANT)")

	for _, name := range []string{
		"grpc-addr",
		"database-url",
		"temporal-addr",
		"temporal-namespace",
		"temporal-task-queue",
		"default-tenant",
	} {
		_ = v.BindPFlag(name, fs.Lookup(name))
	}
}

// LoadFromViper reads Config from v and runs strict fail-fast validation for `eunomia serve`.
func LoadFromViper(v *viper.Viper) (*Config, error) {
	cfg := readFromViper(v)
	if err := cfg.ValidateForServe(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadForMigrateFromViper reads Config from v and validates only EUNOMIA_DATABASE_URL.
func LoadForMigrateFromViper(v *viper.Viper) (*Config, error) {
	cfg := readFromViper(v)
	if err := cfg.ValidateDatabase(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadForSecretsFromViper reads Config from v and validates DB + master keys (for `eunomia secret`).
func LoadForSecretsFromViper(v *viper.Viper) (*Config, error) {
	cfg := readFromViper(v)
	var errs []error
	if err := cfg.ValidateDatabase(); err != nil {
		errs = append(errs, err)
	}
	if err := cfg.ValidateMasterKeys(); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

func readFromViper(v *viper.Viper) *Config {
	return &Config{
		GRPCAddr:          strings.TrimSpace(v.GetString("grpc-addr")),
		DatabaseURL:       strings.TrimSpace(v.GetString("database-url")),
		TemporalAddr:      strings.TrimSpace(v.GetString("temporal-addr")),
		TemporalNamespace: strings.TrimSpace(v.GetString("temporal-namespace")),
		TemporalTaskQueue: strings.TrimSpace(v.GetString("temporal-task-queue")),
		DefaultTenant:     strings.TrimSpace(v.GetString("default-tenant")),
		MasterKeys:        strings.TrimSpace(v.GetString("master-keys")),
		DeepSeekAPIKey:    strings.TrimSpace(v.GetString("deepseek-api-key")),
	}
}

// ValidateDatabase ensures EUNOMIA_DATABASE_URL is present and syntactically valid.
func (c *Config) ValidateDatabase() error {
	if c.DatabaseURL == "" {
		return errors.New("EUNOMIA_DATABASE_URL is required")
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return errors.New("EUNOMIA_DATABASE_URL must be a valid postgres:// URL")
	}
	return nil
}

// ValidateMasterKeys ensures EUNOMIA_MASTER_KEYS is present and parses as a valid 32-byte keyring.
func (c *Config) ValidateMasterKeys() error {
	if c.MasterKeys == "" {
		return errors.New("EUNOMIA_MASTER_KEYS is required (generate one with: eunomia secret keygen)")
	}
	if _, err := secrets.ParseLocalKeyWrapper(c.MasterKeys); err != nil {
		return fmt.Errorf("invalid EUNOMIA_MASTER_KEYS: %w", err)
	}
	return nil
}

// ValidateForServe runs strict fail-fast validation for booting `eunomia serve`.
func (c *Config) ValidateForServe() error {
	var errs []error

	if err := c.ValidateDatabase(); err != nil {
		errs = append(errs, err)
	}
	if _, _, err := net.SplitHostPort(c.GRPCAddr); err != nil {
		errs = append(errs, fmt.Errorf("EUNOMIA_GRPC_ADDR %q must be host:port: %w", c.GRPCAddr, err))
	}
	if _, _, err := net.SplitHostPort(c.TemporalAddr); err != nil {
		errs = append(errs, fmt.Errorf("EUNOMIA_TEMPORAL_ADDR %q must be host:port: %w", c.TemporalAddr, err))
	}
	if c.DefaultTenant == "" {
		errs = append(errs, errors.New("EUNOMIA_DEFAULT_TENANT must not be empty"))
	}
	if err := c.ValidateMasterKeys(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// LogValue implements slog.LogValuer with all secret fields redacted.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("grpc_addr", c.GRPCAddr),
		slog.String("database_url", redactDatabaseURL(c.DatabaseURL)),
		slog.String("temporal_addr", c.TemporalAddr),
		slog.String("temporal_namespace", c.TemporalNamespace),
		slog.String("temporal_task_queue", c.TemporalTaskQueue),
		slog.String("default_tenant", c.DefaultTenant),
		slog.Bool("master_keys_set", c.MasterKeys != ""),
		slog.Bool("deepseek_api_key_set", c.DeepSeekAPIKey != ""),
	)
}

func (c Config) String() string {
	return fmt.Sprintf("Config{GRPCAddr:%s DatabaseURL:%s TemporalAddr:%s DefaultTenant:%s MasterKeys:[REDACTED] DeepSeekAPIKey:[REDACTED]}",
		c.GRPCAddr, redactDatabaseURL(c.DatabaseURL), c.TemporalAddr, c.DefaultTenant)
}

func redactDatabaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[INVALID_URL]"
	}
	return u.Redacted()
}
