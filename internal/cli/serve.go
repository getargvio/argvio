package cli

import (
	"github.com/spf13/cobra"
)

// newServeCommand groups the two long-lived servers under `argvio serve`,
// mirroring ory/hydra's `hydra serve public|admin` — see docs/architecture.md
// for why public and metrics stay independent processes/subcommands rather
// than a single `serve all`.
func newServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run a server",
	}
	cmd.AddCommand(newServePublicCommand(), newServeMetricsCommand())
	return cmd
}
