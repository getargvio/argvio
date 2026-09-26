// Command argvio is the single entrypoint for every argvio process: the
// two long-lived servers (`argvio serve public`, `argvio serve metrics`)
// and the operator CLI (`argvio migrate`, `tenant`, `apikey`, `policies`,
// `retention`). See internal/cli for the subcommand implementations and
// docs/architecture.md for why public and metrics stay independent
// processes even though they now ship from one binary.
package main

import (
	"os"

	"github.com/getargvio/argvio/internal/cli"
)

// version, commit, and date are set via -ldflags at build time (see
// .goreleaser.yaml).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	cli.Version = version
	cli.Commit = commit
	cli.Date = date
	os.Exit(cli.Execute())
}
