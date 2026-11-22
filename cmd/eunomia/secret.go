package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/uptrace/bun"

	"github.com/spacelions/eunomia/internal/config"
	"github.com/spacelions/eunomia/internal/db"
	"github.com/spacelions/eunomia/internal/secrets"
)

func openSecretContext(ctx context.Context, v *viper.Viper, tenantFlag string) (tenant string, database *bun.DB, wrapper *secrets.LocalKeyWrapper, provider *secrets.DBSecretProvider, err error) {
	cfg, err := config.LoadForSecretsFromViper(v)
	if err != nil {
		return "", nil, nil, nil, err
	}
	tenant = tenantFlag
	if tenant == "" {
		tenant = cfg.DefaultTenant
	}
	wrapper, err = secrets.ParseLocalKeyWrapper(cfg.MasterKeys)
	if err != nil {
		return "", nil, nil, nil, err
	}
	database, err = db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return "", nil, nil, nil, err
	}
	return tenant, database, wrapper, secrets.NewDBSecretProvider(database, wrapper), nil
}

func newSecretCmd(v *viper.Viper) *cobra.Command {
	secretCmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage envelope-encrypted tenant secrets in PostgreSQL",
	}

	secretCmd.AddCommand(
		newSecretKeygenCmd(),
		newSecretSetCmd(v),
		newSecretListCmd(v),
		newSecretDeleteCmd(v),
		newSecretRotateCmd(v),
	)

	return secretCmd
}

func newSecretKeygenCmd() *cobra.Command {
	var keyID string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Generate a random 32-byte master key entry for EUNOMIA_MASTER_KEYS",
		RunE: func(cmd *cobra.Command, _ []string) error {
			entry, err := secrets.GenerateLocalKey(keyID)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), entry)
			return err
		},
	}
	cmd.Flags().StringVar(&keyID, "id", "v1", "Master key identifier (e.g. v1, v2)")
	return cmd
}

func newSecretSetCmd(v *viper.Viper) *cobra.Command {
	var tenant, connector, key, filePath string
	var raw bool

	cmd := &cobra.Command{
		Use:   "set",
		Short: "Store or update a tenant secret (reads value from --file or stdin, never CLI args)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if connector == "" || key == "" {
				return errors.New("--connector and --key are required")
			}

			var val []byte
			var err error
			if filePath != "" {
				val, err = os.ReadFile(filePath)
				if err != nil {
					return fmt.Errorf("reading secret file %q: %w", filePath, err)
				}
			} else {
				val, err = io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("reading secret from stdin: %w", err)
				}
				if !raw {
					val = bytes.TrimRight(val, "\r\n")
				}
			}
			if len(val) == 0 {
				return errors.New("secret value is empty")
			}

			resolvedTenant, database, wrapper, provider, err := openSecretContext(cmd.Context(), v, tenant)
			if err != nil {
				return err
			}
			defer func() { _ = database.Close() }()

			if err := provider.Set(cmd.Context(), resolvedTenant, connector, key, val); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Stored secret %s/%s/%s (key_id=%s)\n", resolvedTenant, connector, key, wrapper.ActiveKeyID())
			return err
		},
	}

	cmd.Flags().StringVar(&tenant, "tenant", "", "Tenant ID (defaults to EUNOMIA_DEFAULT_TENANT)")
	cmd.Flags().StringVar(&connector, "connector", "", "Connector type (e.g. google_workspace, notion, slack)")
	cmd.Flags().StringVar(&key, "key", "", "Secret key name (e.g. token, service_account_json)")
	cmd.Flags().StringVar(&filePath, "file", "", "Read secret bytes from file path instead of stdin")
	cmd.Flags().BoolVar(&raw, "raw", false, "Preserve trailing newlines when reading from stdin")
	return cmd
}

func newSecretListCmd(v *viper.Viper) *cobra.Command {
	var tenant string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List stored secret metadata for a tenant (never prints values)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolvedTenant, database, _, provider, err := openSecretContext(cmd.Context(), v, tenant)
			if err != nil {
				return err
			}
			defer func() { _ = database.Close() }()

			items, err := provider.List(cmd.Context(), resolvedTenant)
			if err != nil {
				return err
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "CONNECTOR\tKEY\tKEY_ID\tUPDATED_AT")
			for _, item := range items {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", item.Connector, item.Key, item.KeyID, item.UpdatedAt.Format("2006-01-02T15:04:05Z"))
			}
			return tw.Flush()
		},
	}

	cmd.Flags().StringVar(&tenant, "tenant", "", "Tenant ID (defaults to EUNOMIA_DEFAULT_TENANT)")
	return cmd
}

func newSecretDeleteCmd(v *viper.Viper) *cobra.Command {
	var tenant, connector, key string

	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a stored tenant secret",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if connector == "" || key == "" {
				return errors.New("--connector and --key are required")
			}

			resolvedTenant, database, _, provider, err := openSecretContext(cmd.Context(), v, tenant)
			if err != nil {
				return err
			}
			defer func() { _ = database.Close() }()

			if err := provider.Delete(cmd.Context(), resolvedTenant, connector, key); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Deleted secret %s/%s/%s\n", resolvedTenant, connector, key)
			return err
		},
	}

	cmd.Flags().StringVar(&tenant, "tenant", "", "Tenant ID (defaults to EUNOMIA_DEFAULT_TENANT)")
	cmd.Flags().StringVar(&connector, "connector", "", "Connector type")
	cmd.Flags().StringVar(&key, "key", "", "Secret key name")
	return cmd
}

func newSecretRotateCmd(v *viper.Viper) *cobra.Command {
	return &cobra.Command{
		Use:   "rotate-master-key",
		Short: "Re-wrap all stored secret DEKs to the active key in EUNOMIA_MASTER_KEYS",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, database, wrapper, provider, err := openSecretContext(cmd.Context(), v, "")
			if err != nil {
				return err
			}
			defer func() { _ = database.Close() }()

			count, err := provider.RotateMasterKey(cmd.Context())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Re-wrapped %d secret(s) to active key %q\n", count, wrapper.ActiveKeyID())
			return err
		},
	}
}
