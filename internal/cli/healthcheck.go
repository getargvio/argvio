package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/getargvio/argvio/internal/config"
)

const healthcheckTimeout = 3 * time.Second

// newHealthcheckCommand probes a running server's /healthz from inside its
// own container: the image is distroless (no shell, no curl), so a compose
// `healthcheck: test: ["CMD", "/app/argvio", "healthcheck", "metrics"]` needs
// the binary itself to do the request. Exits 0 on HTTP 200, non-zero
// otherwise. It reads the same config as `serve`, so it follows
// listen_addr overrides from the file or env.
func newHealthcheckCommand() *cobra.Command {
	return &cobra.Command{
		Use:       "healthcheck <metrics|public>",
		Short:     "Probe a running server's /healthz (for container healthchecks)",
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{serverMetrics, serverPublic},
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := config.Load(configPaths)
			if err != nil {
				return fmt.Errorf("config error:\n%w", err)
			}
			addr := root.Metrics.ListenAddr
			if args[0] == serverPublic {
				addr = root.Public.HTTPListenAddr
			}
			url, err := healthURL(addr)
			if err != nil {
				return err
			}
			return probe(cmd.Context(), url)
		},
	}
}

// healthURL turns a listen address into a loopback URL: a wildcard bind
// (0.0.0.0, ::, or no host) isn't dialable, but 127.0.0.1 always reaches it
// from inside the container.
func healthURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q: %w", listenAddr, err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

func probe(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s returned %s", url, resp.Status)
	}
	return nil
}
