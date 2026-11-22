package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/spacelions/eunomia/internal/config"
	"github.com/spacelions/eunomia/internal/connectorstore"
	"github.com/spacelions/eunomia/internal/db"
)

func newConnectorCmd(v *viper.Viper) *cobra.Command {
	connectorCmd := &cobra.Command{
		Use:   "connector",
		Short: "Manage non-secret per-tenant connector settings & resource aliases",
	}

	connectorCmd.AddCommand(
		newConnectorSetCmd(v),
		newConnectorListCmd(v),
		newConnectorDisableCmd(v),
	)

	return connectorCmd
}

func newConnectorSetCmd(v *viper.Viper) *cobra.Command {
	var tenant, connType string
	var enabled bool
	var settingsFlags, resourceFlags []string

	cmd := &cobra.Command{
		Use:   "set",
		Short: "Upsert non-secret connector settings and resource aliases for a tenant",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadForMigrateFromViper(v)
			if err != nil {
				return err
			}
			if tenant == "" {
				tenant = cfg.DefaultTenant
			}
			if connType == "" {
				return errors.New("--type is required")
			}

			settings, err := parseKVPairs(settingsFlags)
			if err != nil {
				return fmt.Errorf("invalid --setting: %w", err)
			}
			resources, err := parseKVPairs(resourceFlags)
			if err != nil {
				return fmt.Errorf("invalid --resource: %w", err)
			}

			database, err := db.Open(cmd.Context(), cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer func() { _ = database.Close() }()

			store := connectorstore.New(database)
			// Merge into existing settings/resources if record already exists.
			existing, getErr := store.Get(cmd.Context(), tenant, connType)
			if getErr != nil && !errors.Is(getErr, connectorstore.ErrConnectorNotFound) {
				return getErr
			}

			mergedSettings := map[string]string{}
			mergedResources := map[string]string{}
			if existing != nil {
				for k, val := range existing.Settings {
					mergedSettings[k] = val
				}
				for k, val := range existing.Resources {
					mergedResources[k] = val
				}
			}
			for k, val := range settings {
				mergedSettings[k] = val
			}
			for k, val := range resources {
				mergedResources[k] = val
			}

			if err := store.Upsert(cmd.Context(), connectorstore.Record{
				TenantID:      tenant,
				ConnectorType: connType,
				Enabled:       enabled,
				Settings:      mergedSettings,
				Resources:     mergedResources,
			}); err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Saved connector %s/%s (enabled=%v)\n", tenant, connType, enabled)
			return err
		},
	}

	cmd.Flags().StringVar(&tenant, "tenant", "", "Tenant ID (defaults to EUNOMIA_DEFAULT_TENANT)")
	cmd.Flags().StringVar(&connType, "type", "", "Connector type (e.g. google_workspace, notion, slack)")
	cmd.Flags().BoolVar(&enabled, "enabled", true, "Whether this connector is enabled for the tenant")
	cmd.Flags().StringArrayVar(&settingsFlags, "setting", nil, "Non-secret setting key=value (repeatable)")
	cmd.Flags().StringArrayVar(&resourceFlags, "resource", nil, "Resource alias=id mapping (repeatable)")
	return cmd
}

func newConnectorListCmd(v *viper.Viper) *cobra.Command {
	var tenant string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured connectors for a tenant",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadForMigrateFromViper(v)
			if err != nil {
				return err
			}
			if tenant == "" {
				tenant = cfg.DefaultTenant
			}

			database, err := db.Open(cmd.Context(), cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer func() { _ = database.Close() }()

			recs, err := connectorstore.New(database).List(cmd.Context(), tenant)
			if err != nil {
				return err
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "CONNECTOR\tENABLED\tSETTINGS\tRESOURCES")
			for _, r := range recs {
				sb, _ := json.Marshal(r.Settings)
				rb, _ := json.Marshal(r.Resources)
				_, _ = fmt.Fprintf(tw, "%s\t%v\t%s\t%s\n", r.ConnectorType, r.Enabled, string(sb), string(rb))
			}
			return tw.Flush()
		},
	}

	cmd.Flags().StringVar(&tenant, "tenant", "", "Tenant ID (defaults to EUNOMIA_DEFAULT_TENANT)")
	return cmd
}

func newConnectorDisableCmd(v *viper.Viper) *cobra.Command {
	var tenant, connType string

	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Disable a connector for a tenant",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadForMigrateFromViper(v)
			if err != nil {
				return err
			}
			if tenant == "" {
				tenant = cfg.DefaultTenant
			}
			if connType == "" {
				return errors.New("--type is required")
			}

			database, err := db.Open(cmd.Context(), cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer func() { _ = database.Close() }()

			if err := connectorstore.New(database).SetEnabled(cmd.Context(), tenant, connType, false); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Disabled connector %s/%s\n", tenant, connType)
			return err
		},
	}

	cmd.Flags().StringVar(&tenant, "tenant", "", "Tenant ID (defaults to EUNOMIA_DEFAULT_TENANT)")
	cmd.Flags().StringVar(&connType, "type", "", "Connector type")
	return cmd
}

func parseKVPairs(pairs []string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, val, ok := strings.Cut(p, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("%q must be in key=value format", p)
		}
		out[k] = val
	}
	return out, nil
}
