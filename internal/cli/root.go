// Package cli wires up the argvio binary's subcommands: `serve public`,
// `serve metrics` run the two long-lived servers, everything else
// (`migrate`, `tenant`, `apikey`, `policies`, `retention`) is the operator
// CLI formerly known as cmd/admin. One binary, one entrypoint — see
// docs/architecture.md for why public and metrics remain separate
// processes even though they now ship from the same executable.
package cli

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

// Version, Commit, and Date are set via -ldflags at build time (see
// .goreleaser.yaml).
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "argvio",
		Short:         "argvio: OTLP ingest and query platform",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newServeCommand(),
		newMigrateCommand(),
		newTenantCommand(),
		newAPIKeyCommand(),
		newPoliciesCommand(),
		newRetentionCommand(),
	)

	return root
}

// Execute runs the root command and returns the process exit code.
func Execute() int {
	if err := NewRootCommand().Execute(); err != nil {
		slog.Error(err.Error())
		return 1
	}
	return 0
}

func newLogger(level string) *slog.Logger {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(level)}))
	slog.SetDefault(log)
	return log
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
