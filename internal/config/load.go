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

// loadInto layers defaults -> zero or more YAML files, in the order given
// (each one merges over the last) -> environment variables (highest
// precedence) into dst. Mirrors ory/hydra's repeatable `-c/--config` flag:
// `argvio serve public -c base.yaml -c prod.yaml` layers prod.yaml over
// base.yaml.
func loadInto(paths []string, defaults map[string]any, dst any) error {
	k := koanf.New(".")

	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		return fmt.Errorf("config: load defaults: %w", err)
	}

	for _, path := range paths {
		if path == "" {
			continue
		}
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

// Load reads the single argvio configuration document — one set of layered
// YAML files covering `public:`, `metrics:`, and `storage:` sections, the
// same file(s) regardless of which subcommand is running — plus env var
// overrides. paths may be empty to rely on defaults + env only. Callers
// validate only the sections their subcommand needs: see
// Root.ValidatePublic, Root.ValidateMetrics, Root.ValidateStorage.
func Load(paths []string) (*Root, error) {
	var c Root
	if err := loadInto(paths, defaults(), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Defaults exposes the layered-config defaults (see loadInto) for tooling —
// specifically docs/configuration.md's generator (tools/gendocs), so the
// documented defaults can never drift from what Load actually uses.
func Defaults() map[string]any { return defaults() }

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
