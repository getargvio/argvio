package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

const envPrefix = "ARGVIO_"

// envKeyTransform maps ARGVIO_STORAGE__PUBLIC_POOL__MAX_CONNS to
// storage.public_pool.max_conns: "__" becomes the "." nesting delimiter,
// single "_" stays literal (many of our keys contain underscores), and the
// whole thing is lowercased.
func envKeyTransform(s string) string {
	trimmed := strings.TrimPrefix(s, envPrefix)
	dotted := strings.ReplaceAll(trimmed, "__", ".")
	return strings.ToLower(dotted)
}

// durationDecodeHook lets koanf.Unmarshal populate time.Duration fields from
// YAML/env string values like "24h" or "15s".
func durationDecodeHook() mapstructure.DecodeHookFunc {
	return mapstructure.StringToTimeDurationHookFunc()
}

// loadInto layers defaults -> optional YAML file at path -> environment
// variables (highest precedence) into dst. path may be empty to skip the
// file layer (defaults + env only — useful for tests/containers that are
// fully env-configured).
func loadInto(path string, defaults map[string]any, dst any) error {
	k := koanf.New(".")

	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		return fmt.Errorf("config: load defaults: %w", err)
	}

	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return fmt.Errorf("config: load file %s: %w", path, err)
		}
	}

	if err := k.Load(env.Provider(envPrefix, ".", envKeyTransform), nil); err != nil {
		return fmt.Errorf("config: load env: %w", err)
	}

	if err := k.UnmarshalWithConf("", dst, koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			Result:           dst,
			WeaklyTypedInput: true,
			DecodeHook:       durationDecodeHook(),
			TagName:          "koanf",
		},
	}); err != nil {
		return fmt.Errorf("config: unmarshal: %w", err)
	}

	return nil
}

// LoadPublic loads the public server's config. path may be "" to rely on
// defaults + env only.
func LoadPublic(path string) (*PublicRoot, error) {
	var c PublicRoot
	if err := loadInto(path, publicDefaults(), &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadMetrics loads the metrics server's config. path may be "" to rely on
// defaults + env only.
func LoadMetrics(path string) (*MetricsRoot, error) {
	var c MetricsRoot
	if err := loadInto(path, metricsDefaults(), &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// PublicDefaults and MetricsDefaults expose the layered-config defaults
// (see loadInto) for tooling — specifically docs/configuration.md's
// generator (tools/gendocs), so the documented defaults can never drift
// from what LoadPublic/LoadMetrics actually use.
func PublicDefaults() map[string]any  { return publicDefaults() }
func MetricsDefaults() map[string]any { return metricsDefaults() }

func joinErrors(msgs []string) error {
	if len(msgs) == 0 {
		return nil
	}
	errs := make([]error, len(msgs))
	for i, m := range msgs {
		errs[i] = errors.New(m)
	}
	return fmt.Errorf("config: %d validation error(s):\n%w", len(errs), errors.Join(errs...))
}
