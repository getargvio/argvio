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

// configPaths backs the root command's repeatable --config/-c flag, shared
// by every subcommand that reads configuration (`serve public`, `serve
// metrics`, `policies apply`) — one config document for the whole binary,
// same as ory/hydra's `-c/--config hydra.yml`. Passing it more than once
// layers files in order, each overriding the last.
var configPaths []string

func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "argvio",
		Short:         "argvio: OTLP ingest and query platform",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringArrayVarP(&configPaths, "config", "c", nil, "path to a config YAML (defaults + env still apply). Repeat to layer multiple files, later ones taking precedence")

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
