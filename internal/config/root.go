package config

// Root is the single configuration document for the argvio binary: one
// YAML file (or set of layered files, see Load) covers `public:`,
// `metrics:`, and `storage:` sections together, mirroring how ory/hydra
// ships one hydra.yml even though `hydra serve public` and `hydra serve
// admin` are separate processes. `argvio serve public` and `serve metrics`
// each read the whole document but only validate the sections they use —
// see ValidatePublic/ValidateMetrics/ValidateStorage.
type Root struct {
	LogLevel string        `koanf:"log_level"`
	Auth     AuthConfig    `koanf:"auth"`
	Public   PublicConfig  `koanf:"public"`
	Metrics  MetricsConfig `koanf:"metrics"`
	Storage  StorageConfig `koanf:"storage"`
}

func defaults() map[string]any {
	d := map[string]any{
		"log_level": "info",
	}
	for k, v := range authDefaults() {
		d[k] = v
	}
	for k, v := range storageDefaults() {
		d[k] = v
	}
	for k, v := range publicDefaults() {
		d[k] = v
	}
	for k, v := range metricsDefaults() {
		d[k] = v
	}
	return d
}

// ValidatePublic fails fast with every problem found (not just the first)
// in the sections `argvio serve public` needs: log_level, public.*, and
// storage.*.
func (r Root) ValidatePublic() error {
	var errs []string
	if r.Public.GRPCListenAddr == "" {
		errs = append(errs, "public.grpc_listen_addr must not be empty")
	}
	if r.Public.HTTPListenAddr == "" {
		errs = append(errs, "public.http_listen_addr must not be empty")
	}
	if r.Public.TLS.Enabled && (r.Public.TLS.CertFile == "" || r.Public.TLS.KeyFile == "") {
		errs = append(errs, "public.tls.cert_file and key_file are required when public.tls.enabled = true")
	}
	if r.Public.MaxBatchSize <= 0 {
		errs = append(errs, "public.max_batch_size must be > 0")
	}
	if r.Public.MaxAttributeCount <= 0 {
		errs = append(errs, "public.max_attribute_count must be > 0")
	}
	if r.Public.MaxAttributeKeyLength <= 0 {
		errs = append(errs, "public.max_attribute_key_length must be > 0")
	}
	if r.Public.MaxAttributeStringValueLength <= 0 {
		errs = append(errs, "public.max_attribute_string_value_length must be > 0")
	}
	if r.Public.MaxTimestampSkewPast <= 0 || r.Public.MaxTimestampSkewFuture <= 0 {
		errs = append(errs, "public.max_timestamp_skew_past and max_timestamp_skew_future must be > 0")
	}
	if r.Public.APIKeyCacheTTL <= 0 {
		errs = append(errs, "public.api_key_cache_ttl must be > 0")
	}
	if r.Public.RateLimitRequestsPerSecond <= 0 {
		errs = append(errs, "public.rate_limit_requests_per_second must be > 0")
	}
	if r.Public.AllowlistSchemaPath == "" {
		errs = append(errs, "public.allowlist_schema_path must not be empty")
	}
	errs = append(errs, r.Auth.validate()...)
	errs = append(errs, r.Storage.validate()...)
	return joinErrors(errs)
}

// ValidateMetrics fails fast with every problem found in the sections
// `argvio serve metrics` needs: log_level, metrics.*, and storage.*.
func (r Root) ValidateMetrics() error {
	var errs []string
	if r.Metrics.ListenAddr == "" {
		errs = append(errs, "metrics.listen_addr must not be empty")
	}
	if r.Metrics.TLS.Enabled && (r.Metrics.TLS.CertFile == "" || r.Metrics.TLS.KeyFile == "") {
		errs = append(errs, "metrics.tls.cert_file and key_file are required when metrics.tls.enabled = true")
	}
	errs = append(errs, r.Auth.validate()...)
	if r.Metrics.QueryTimeout <= 0 {
		errs = append(errs, "metrics.query_timeout must be > 0")
	}
	if r.Metrics.MaxResultPageSize <= 0 || r.Metrics.DefaultResultPageSize <= 0 {
		errs = append(errs, "metrics.max_result_page_size and default_result_page_size must be > 0")
	}
	if r.Metrics.DefaultResultPageSize > r.Metrics.MaxResultPageSize {
		errs = append(errs, "metrics.default_result_page_size must not exceed max_result_page_size")
	}
	if r.Metrics.MaxTimeRangeSpan <= 0 {
		errs = append(errs, "metrics.max_time_range_span must be > 0")
	}
	errs = append(errs, r.Storage.validate()...)
	return joinErrors(errs)
}

// ValidateStorage checks only storage.* — used by `argvio policies apply`,
// which touches Postgres/Timescale directly but starts neither server.
func (r Root) ValidateStorage() error {
	return joinErrors(r.Storage.validate())
}
