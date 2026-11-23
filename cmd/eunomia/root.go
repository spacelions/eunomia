package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/spacelions/eunomia/internal/config"
	"github.com/spacelions/eunomia/internal/connectorstore"
	"github.com/spacelions/eunomia/internal/db"
	"github.com/spacelions/eunomia/internal/secrets"
	"github.com/spacelions/eunomia/internal/server"
)

func newRootCmd() *cobra.Command {
	v := config.NewViper()

	rootCmd := &cobra.Command{
		Use:           "eunomia",
		Short:         "Project Eunomia — Generic Agentic Workflow Engine",
		SilenceUsage:  true,
		SilenceErrors: false,
		// Running `eunomia` with no subcommand runs `serve`.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), v)
		},
	}

	config.RegisterFlags(rootCmd.PersistentFlags(), v)

	rootCmd.AddCommand(
		newServeCmd(v),
		newMigrateCmd(v),
		newSecretCmd(v),
		newConnectorCmd(v),
		newActorCmd(v),
	)

	return rootCmd
}

func newServeCmd(v *viper.Viper) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the Eunomia ConnectRPC server (never runs migrations automatically)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), v)
		},
	}
}

func newMigrateCmd(v *viper.Viper) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending PostgreSQL migrations explicitly",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMigrate(cmd.Context(), v)
		},
	}
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

func runServe(parentCtx context.Context, v *viper.Viper) error {
	logger := newLogger()

	cfg, err := config.LoadFromViper(v)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	logger.Info("configuration loaded", "config", cfg)

	wrapper, err := secrets.ParseLocalKeyWrapper(cfg.MasterKeys)
	if err != nil {
		return fmt.Errorf("initializing key wrapper: %w", err)
	}

	ctx, stop := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := database.Close(); closeErr != nil {
			logger.Error("closing database", "err", closeErr)
		}
	}()

	// Verify DB-backed secret provider & connector store are ready for the default tenant.
	_ = secrets.NewDBSecretProvider(database, wrapper)
	connStore := connectorstore.New(database)
	enabledConns, err := connStore.ListEnabled(ctx, cfg.DefaultTenant)
	if err != nil {
		return fmt.Errorf("loading tenant connectors (did you run `eunomia migrate`?): %w", err)
	}
	connNames := make([]string, 0, len(enabledConns))
	for _, c := range enabledConns {
		connNames = append(connNames, c.ConnectorType)
	}
	logger.Info("boot checks passed",
		"active_master_key_id", wrapper.ActiveKeyID(),
		"default_tenant", cfg.DefaultTenant,
		"enabled_connectors", connNames,
	)

	handler := server.NewHandler(func(checkCtx context.Context) error {
		return database.PingContext(checkCtx)
	})
	srv := server.NewHTTPServer(cfg.GRPCAddr, handler)

	ln, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.GRPCAddr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("eunomia ConnectRPC server listening", "addr", ln.Addr().String())
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
		close(errCh)
	}()

	select {
	case serveErr := <-errCh:
		return serveErr
	case <-ctx.Done():
		logger.Info("shutting down eunomia gracefully")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func runMigrate(ctx context.Context, v *viper.Viper) error {
	logger := newLogger()

	cfg, err := config.LoadForMigrateFromViper(v)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() {
		_ = database.Close()
	}()

	group, err := db.Migrate(ctx, database)
	if err != nil {
		return err
	}
	if group.IsZero() {
		logger.Info("database is already up to date")
		return nil
	}
	logger.Info("applied database migrations", "group", group.String())
	return nil
}
