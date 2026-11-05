package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/motoki317/noctiluca/pkg/infrastructure/mcpclient"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

var (
	version  = "dev"
	revision = "unknown"
)

func newRootCommand() *cobra.Command {
	var configPath string
	root := &cobra.Command{
		Use:           "noctiluca",
		Short:         "A personal agent harness",
		Version:       fmt.Sprintf("%s (%s)", version, revision),
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "", "YAML config file (default: config.yaml, if present)")
	root.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Run noctiluca",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := loadConfig(configPath)
			if err != nil {
				return err
			}
			logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: config.logLevel}))
			previous := slog.Default()
			slog.SetDefault(logger)
			defer slog.SetDefault(previous)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			app, err := newApplication(ctx, config, logger)
			if err != nil {
				return err
			}
			logger.InfoContext(ctx, "Starting noctiluca", "version", version, "revision", revision)
			return app.Start(ctx)
		},
	}, &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), root.Version)
			return err
		},
	})
	root.AddCommand(&cobra.Command{
		Use: "mcp-login <server>", Short: "Authorize a configured MCP server", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := loadConfig(configPath)
			if err != nil {
				return err
			}
			for _, server := range config.MCPServers {
				if server.Name != args[0] {
					continue
				}
				db, err := repository.New(cmd.Context(), config.Database)
				if err != nil {
					return err
				}
				defer db.Close()
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				if err := mcpclient.Login(ctx, server, repository.NewOAuthTokenRepository(db), cmd.OutOrStdout()); err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.ErrOrStderr(), "MCP login saved")
				return err
			}
			return errors.New("MCP server is not configured")
		},
	})
	return root
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := newRootCommand().Execute(); err != nil {
		slog.Error("Command failed", "error", err)
		os.Exit(1)
	}
}
