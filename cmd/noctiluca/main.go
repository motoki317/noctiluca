package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
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
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), root.Version)
			return err
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
