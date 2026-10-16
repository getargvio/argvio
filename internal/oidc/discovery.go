package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	maxDiscoveryBodyBytes = 1 << 20 // 1 MiB
	maxJWKSBodyBytes      = 1 << 20 // 1 MiB

	// fetchTimeout bounds one provider round trip. Shared fetches run
	// detached from the triggering request's context, so this is what stops
	// them hanging.
	fetchTimeout = 10 * time.Second
)

// discovery is the subset of the OIDC discovery document (and RFC 8414
// metadata) we rely on.
type discovery struct {
	Issuer                string `json:"issuer"`
	JWKSURI               string `json:"jwks_uri"`
	IntrospectionEndpoint string `json:"introspection_endpoint"`
}

// discover returns the provider's discovery document, fetching it once and
// caching it for the life of the Verifier. Failures are not cached.
func (v *Verifier) discover(ctx context.Context) (*discovery, error) {
	v.metaMu.RLock()
	d := v.meta
	v.metaMu.RUnlock()
	if d != nil {
		return d, nil
	}

	res, err, _ := v.metaFlight.Do("discovery", func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()

		var doc discovery
		// OIDC Discovery §4.1: a trailing slash on the issuer is dropped
		// before appending the well-known path. The issuer itself is
		// compared exactly as configured and as the document states it.
		url := strings.TrimSuffix(v.IssuerURL, "/") + "/.well-known/openid-configuration"
		if err := fetchJSON(ctx, v.client(), url, maxDiscoveryBodyBytes, &doc); err != nil {
			return nil, unavailable(fmt.Errorf("fetching discovery document: %w", err))
		}
		if doc.Issuer != v.IssuerURL {
			return nil, unavailable(fmt.Errorf("discovery issuer %q does not match configured issuer %q", doc.Issuer, v.IssuerURL))
		}
		if doc.JWKSURI == "" {
			return nil, unavailable(errors.New("discovery document has no jwks_uri"))
		}
		v.metaMu.Lock()
		v.meta = &doc
		v.metaMu.Unlock()
		return &doc, nil
	})
	if err != nil {
		return nil, err
	}
	return res.(*discovery), nil
}

// CheckStartup fails fast on a configuration the provider cannot honour:
// introspection enabled but no endpoint, configured or discovered.
func (v *Verifier) CheckStartup(ctx context.Context) error {
	if !v.Introspection.Enabled || v.Introspection.Endpoint != "" {
		return nil
	}
	d, err := v.discover(ctx)
	if err != nil {
		return fmt.Errorf("auth.oidc.introspection.endpoint is not set and discovery failed: %w", err)
	}
	if d.IntrospectionEndpoint == "" {
		return errors.New("auth.oidc.introspection.enabled is set but auth.oidc.introspection.endpoint is empty and the provider's discovery document has no introspection_endpoint")
	}
	return nil
}

func (v *Verifier) client() *http.Client {
	if v.HTTPClient != nil {
		return v.HTTPClient
	}
	return http.DefaultClient
}

// readCapped reads at most limit bytes, erroring if the body is larger.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return b, nil
}

func fetchJSON(ctx context.Context, client *http.Client, url string, limit int64, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}
	b, err := readCapped(resp.Body, limit)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}
