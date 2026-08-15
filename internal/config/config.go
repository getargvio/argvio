// Package config provides layered configuration (defaults -> YAML file ->
// environment variable overrides) for both the public and metrics servers,
// backed by koanf. Env vars use "ARGVIO_" as prefix and "__" as the nesting
// delimiter (single underscores are legal inside a key name, e.g.
// grpc_listen_addr), so:
//
//	ARGVIO_PUBLIC__GRPC_LISTEN_ADDR   -> public.grpc_listen_addr
//	ARGVIO_STORAGE__PUBLIC_POOL__MAX_CONNS -> storage.public_pool.max_conns
//
// See docs/configuration.md (generated from these structs) for the full key
// reference.
package config

import "time"

// defaultDayDuration is reused wherever a config default is "one day" in
// koanf's duration-string form (chunk intervals, timestamp skew bounds).
const defaultDayDuration = "24h"

// TLSConfig is shared by both servers' listeners.
type TLSConfig struct {
	Enabled  bool   `koanf:"enabled"`
	CertFile string `koanf:"cert_file"`
	KeyFile  string `koanf:"key_file"`
}

// StoragePoolConfig sizes one server's pgx connection pool. public and
// metrics get independent pools (and, in production, independent Postgres
// roles) so a runaway analytical query can never starve ingest of
// connections.
type StoragePoolConfig struct {
	MaxConns int32 `koanf:"max_conns"`
	MinConns int32 `koanf:"min_conns"`
}

// PerSignalPolicy captures the Timescale chunk/compression/retention knobs
// that can reasonably differ by signal type (e.g. traces are typically
// retained for a shorter window than aggregated metrics).
//
// ChunkInterval is applied at hypertable-creation time (see migrations/) —
// changing it here after the hypertable exists does not retroactively
// rechunk existing data, only new chunks. CompressionAfter/RetentionAfter
// are reconciled at runtime by `cmd/admin policies apply`, which calls
// Timescale's add_compression_policy/add_retention_policy (idempotent:
// altering an existing job) so operators can tune them without hand-writing
// SQL.
type PerSignalPolicy struct {
	ChunkInterval    time.Duration `koanf:"chunk_interval"`
	CompressionAfter time.Duration `koanf:"compression_after"`
	RetentionAfter   time.Duration `koanf:"retention_after"`
}

// StorageConfig is shared between both server root configs — both talk to
// the same Postgres/Timescale instance, but with separate pools.
type StorageConfig struct {
	DSN              string            `koanf:"dsn"`
	PublicPool       StoragePoolConfig `koanf:"public_pool"`
	MetricsPool      StoragePoolConfig `koanf:"metrics_pool"`
	StatementTimeout time.Duration     `koanf:"statement_timeout"`
	Traces           PerSignalPolicy   `koanf:"traces"`
	Logs             PerSignalPolicy   `koanf:"logs"`
	Metrics          PerSignalPolicy   `koanf:"metrics"`
}

func storageDefaults() map[string]any {
	return map[string]any{
		"storage.public_pool.max_conns":  int32(50),
		"storage.public_pool.min_conns":  int32(5),
		"storage.metrics_pool.max_conns": int32(20),
		"storage.metrics_pool.min_conns": int32(2),
		"storage.statement_timeout":      "30s",

		"storage.traces.chunk_interval":    defaultDayDuration,
		"storage.traces.compression_after": "168h", // 7d
		"storage.traces.retention_after":   "720h", // 30d

		"storage.logs.chunk_interval":    defaultDayDuration,
		"storage.logs.compression_after": "168h",  // 7d
		"storage.logs.retention_after":   "2160h", // 90d

		"storage.metrics.chunk_interval":    defaultDayDuration,
		"storage.metrics.compression_after": "720h",  // 30d
		"storage.metrics.retention_after":   "9600h", // ~400d
	}
}

func (s StorageConfig) validate() []string {
	var errs []string
	if s.DSN == "" {
		errs = append(errs, "storage.dsn must not be empty")
	}
	if s.PublicPool.MaxConns <= 0 {
		errs = append(errs, "storage.public_pool.max_conns must be > 0")
	}
	if s.MetricsPool.MaxConns <= 0 {
		errs = append(errs, "storage.metrics_pool.max_conns must be > 0")
	}
	if s.StatementTimeout <= 0 {
		errs = append(errs, "storage.statement_timeout must be > 0")
	}
	for name, p := range map[string]PerSignalPolicy{"traces": s.Traces, "logs": s.Logs, "metrics": s.Metrics} {
		if p.ChunkInterval <= 0 {
			errs = append(errs, "storage."+name+".chunk_interval must be > 0")
		}
		if p.RetentionAfter > 0 && p.CompressionAfter > 0 && p.CompressionAfter > p.RetentionAfter {
			errs = append(errs, "storage."+name+".compression_after must not exceed retention_after")
		}
	}
	return errs
}
