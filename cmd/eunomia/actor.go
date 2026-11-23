package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/spacelions/eunomia/internal/actors"
	"github.com/spacelions/eunomia/internal/config"
	"github.com/spacelions/eunomia/internal/db"
)

func newActorCmd(v *viper.Viper) *cobra.Command {
	actorCmd := &cobra.Command{
		Use:   "actor",
		Short: "Manage tenant actor directory entries used by lookup_people & RecipientGuard",
	}

	actorCmd.AddCommand(
		newActorSetCmd(v),
		newActorListCmd(v),
		newActorDeleteCmd(v),
	)

	return actorCmd
}

func parseHandleFlags(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for _, entry := range raw {
		provider, externalID, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(externalID) == "" {
			return nil, fmt.Errorf("invalid --handle %q (expected provider=id, e.g. slack=U123)", entry)
		}
		out[strings.TrimSpace(provider)] = strings.TrimSpace(externalID)
	}
	return out, nil
}

func formatHandles(handles map[string]string) string {
	if len(handles) == 0 {
		return ""
	}
	providers := make([]string, 0, len(handles))
	for p := range handles {
		providers = append(providers, p)
	}
	slices.Sort(providers)
	parts := make([]string, 0, len(providers))
	for _, p := range providers {
		parts = append(parts, p+"="+handles[p])
	}
	return strings.Join(parts, ",")
}

func newActorSetCmd(v *viper.Viper) *cobra.Command {
	var tenant, userID, name, email string
	var roles, handleFlags []string

	cmd := &cobra.Command{
		Use:   "set",
		Short: "Upsert an actor in the tenant directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if userID == "" || name == "" || email == "" {
				return errors.New("--user-id, --name, and --email are required")
			}
			handles, err := parseHandleFlags(handleFlags)
			if err != nil {
				return err
			}
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

			if err := actors.NewStore(database).Upsert(cmd.Context(), actors.Actor{
				TenantID: tenant,
				UserID:   userID,
				Name:     name,
				Email:    email,
				Roles:    roles,
				Handles:  handles,
			}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Saved actor %s/%s (%s)\n", tenant, userID, email)
			return err
		},
	}

	cmd.Flags().StringVar(&tenant, "tenant", "", "Tenant ID (defaults to EUNOMIA_DEFAULT_TENANT)")
	cmd.Flags().StringVar(&userID, "user-id", "", "Unique user identifier within tenant (e.g. fu)")
	cmd.Flags().StringVar(&name, "name", "", "Display name")
	cmd.Flags().StringVar(&email, "email", "", "Email address")
	cmd.Flags().StringArrayVar(&roles, "role", nil, "Role tag (repeatable, e.g. --role it_ops)")
	cmd.Flags().StringArrayVar(&handleFlags, "handle", nil, "Connector identity handle as provider=id (repeatable, e.g. --handle slack=U123 --handle linear=LIN_1)")
	return cmd
}

func newActorListCmd(v *viper.Viper) *cobra.Command {
	var tenant, query string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List or search actors in the tenant directory",
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

			store := actors.NewStore(database)
			var list []actors.Actor
			if strings.TrimSpace(query) != "" {
				list, err = store.LookupPeople(cmd.Context(), tenant, query)
			} else {
				list, err = store.List(cmd.Context(), tenant)
			}
			if err != nil {
				return err
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "USER_ID\tNAME\tEMAIL\tROLES\tHANDLES")
			for _, a := range list {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					a.UserID, a.Name, a.Email, strings.Join(a.Roles, ","), formatHandles(a.Handles))
			}
			return tw.Flush()
		},
	}

	cmd.Flags().StringVar(&tenant, "tenant", "", "Tenant ID (defaults to EUNOMIA_DEFAULT_TENANT)")
	cmd.Flags().StringVar(&query, "query", "", "Filter by role, name, user_id, or email")
	return cmd
}

func newActorDeleteCmd(v *viper.Viper) *cobra.Command {
	var tenant, userID string

	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Remove an actor from the tenant directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if userID == "" {
				return errors.New("--user-id is required")
			}
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

			if err := actors.NewStore(database).Delete(cmd.Context(), tenant, userID); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Deleted actor %s/%s\n", tenant, userID)
			return err
		},
	}

	cmd.Flags().StringVar(&tenant, "tenant", "", "Tenant ID (defaults to EUNOMIA_DEFAULT_TENANT)")
	cmd.Flags().StringVar(&userID, "user-id", "", "User ID to delete")
	return cmd
}
