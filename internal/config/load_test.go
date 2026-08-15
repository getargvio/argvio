package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadPublic_Defaults(t *testing.T) {
	t.Setenv("ARGVIO_STORAGE__DSN", "postgres://user:pass@localhost:5432/argvio")
	c, err := LoadPublic("")
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

func TestLoadPublic_EnvOverride(t *testing.T) {
	t.Setenv("ARGVIO_STORAGE__DSN", "postgres://user:pass@localhost:5432/argvio")
	t.Setenv("ARGVIO_PUBLIC__GRPC_LISTEN_ADDR", "127.0.0.1:9999")
	t.Setenv("ARGVIO_STORAGE__PUBLIC_POOL__MAX_CONNS", "77")

	c, err := LoadPublic("")
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

func TestLoadPublic_MissingDSNFailsValidation(t *testing.T) {
	if _, err := LoadPublic(""); err == nil {
		t.Fatalf("expected validation error for missing storage.dsn")
	}
}

func TestLoadMetrics_DefaultAuthModeRequiresSigningKey(t *testing.T) {
	t.Setenv("ARGVIO_STORAGE__DSN", "postgres://user:pass@localhost:5432/argvio")
	if _, err := LoadMetrics(""); err == nil {
		t.Fatalf("expected validation error: default auth_mode=jwt requires jwt_signing_key")
	}
	t.Setenv("ARGVIO_METRICS__JWT_SIGNING_KEY", "test-secret")
	c, err := LoadMetrics("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Metrics.AuthMode != AuthModeJWT {
		t.Errorf("auth_mode = %q, want jwt", c.Metrics.AuthMode)
	}
}

func TestLoadPublic_FromYAMLFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/public.yaml"
	yamlContent := "public:\n  grpc_listen_addr: \"0.0.0.0:14317\"\nstorage:\n  dsn: \"postgres://x/y\"\n"
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadPublic(path)
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
