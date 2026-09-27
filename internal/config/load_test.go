package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("ARGVIO_STORAGE__DSN", "postgres://user:pass@localhost:5432/argvio")
	c, err := Load(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Public.GRPCListenAddr != "0.0.0.0:4317" {
		t.Errorf("grpc addr = %q", c.Public.GRPCListenAddr)
	}
	if c.Public.MaxTimestampSkewPast != 24*time.Hour {
		t.Errorf("max_timestamp_skew_past = %v, want 24h", c.Public.MaxTimestampSkewPast)
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	t.Setenv("ARGVIO_STORAGE__DSN", "postgres://user:pass@localhost:5432/argvio")
	t.Setenv("ARGVIO_PUBLIC__GRPC_LISTEN_ADDR", "127.0.0.1:9999")
	t.Setenv("ARGVIO_STORAGE__PUBLIC_POOL__MAX_CONNS", "77")

	c, err := Load(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Public.GRPCListenAddr != "127.0.0.1:9999" {
		t.Errorf("grpc addr = %q, want env override", c.Public.GRPCListenAddr)
	}
	if c.Storage.DSN != "postgres://user:pass@localhost:5432/argvio" {
		t.Errorf("dsn = %q, want env override", c.Storage.DSN)
	}
	if c.Storage.PublicPool.MaxConns != 77 {
		t.Errorf("public_pool.max_conns = %d, want 77", c.Storage.PublicPool.MaxConns)
	}
}

func TestValidatePublic_MissingDSNFails(t *testing.T) {
	c, err := Load(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := c.ValidatePublic(); err == nil {
		t.Fatalf("expected validation error for missing storage.dsn")
	}
}

func TestValidateMetrics_DefaultAuthMethodsRequireSigningKey(t *testing.T) {
	t.Setenv("ARGVIO_STORAGE__DSN", "postgres://user:pass@localhost:5432/argvio")
	c, err := Load(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := c.ValidateMetrics(); err == nil {
		t.Fatalf("expected validation error: default auth.methods includes jwt, which requires jwt_signing_key")
	}

	t.Setenv("ARGVIO_AUTH__JWT_SIGNING_KEY", "test-secret")
	c, err = Load(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := c.ValidateMetrics(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.Auth.Has(AuthMethodJWT) || !c.Auth.Has(AuthMethodAPIKey) {
		t.Errorf("auth.methods = %v, want [api_key jwt] default", c.Auth.Methods)
	}
}

func TestValidateMetrics_OIDCMethodRequiresIssuerAndAudience(t *testing.T) {
	t.Setenv("ARGVIO_STORAGE__DSN", "postgres://user:pass@localhost:5432/argvio")
	t.Setenv("ARGVIO_AUTH__METHODS", "oidc")

	c, err := Load(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := c.ValidateMetrics(); err == nil {
		t.Fatalf("expected validation error: auth.methods=[oidc] requires issuer_url and audience")
	}

	t.Setenv("ARGVIO_AUTH__OIDC__ISSUER_URL", "https://idp.example.com")
	t.Setenv("ARGVIO_AUTH__OIDC__AUDIENCE", "argvio-metrics")
	c, err = Load(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := c.ValidateMetrics(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Auth.OIDC.JWKSCacheTTL != 15*time.Minute {
		t.Errorf("oidc.jwks_cache_ttl = %v, want 15m default", c.Auth.OIDC.JWKSCacheTTL)
	}
}

func TestLoad_FromYAMLFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/argvio.yaml"
	yamlContent := "public:\n  grpc_listen_addr: \"0.0.0.0:14317\"\nstorage:\n  dsn: \"postgres://x/y\"\n"
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load([]string{path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Public.GRPCListenAddr != "0.0.0.0:14317" {
		t.Errorf("grpc addr = %q", c.Public.GRPCListenAddr)
	}
	if c.Storage.DSN != "postgres://x/y" {
		t.Errorf("dsn = %q", c.Storage.DSN)
	}
}

func TestLoad_LayersMultipleFiles(t *testing.T) {
	dir := t.TempDir()
	base := dir + "/base.yaml"
	override := dir + "/override.yaml"
	if err := os.WriteFile(base, []byte("public:\n  grpc_listen_addr: \"0.0.0.0:1\"\nmetrics:\n  listen_addr: \"0.0.0.0:2\"\nstorage:\n  dsn: \"postgres://x/y\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte("public:\n  grpc_listen_addr: \"0.0.0.0:3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load([]string{base, override})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Public.GRPCListenAddr != "0.0.0.0:3" {
		t.Errorf("grpc addr = %q, want override.yaml to win", c.Public.GRPCListenAddr)
	}
	if c.Metrics.ListenAddr != "0.0.0.0:2" {
		t.Errorf("metrics listen addr = %q, want base.yaml value to survive", c.Metrics.ListenAddr)
	}
}
