package allowlist

import (
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

// Loader owns the currently active Schema and can hot-reload it from disk.
// Safe for concurrent use: Current() is a lock-free atomic load, so the hot
// ingest path never blocks on a reload in progress.
type Loader struct {
	path    string
	current atomic.Pointer[Schema]
	log     *slog.Logger
	watcher *fsnotify.Watcher
}

// Parse decodes and validates raw YAML bytes into a Schema without touching
// disk. Exposed separately so tests can construct schemas from literals.
func Parse(data []byte) (*Schema, error) {
	var s Schema
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("allowlist: parse: %w", err)
	}
	if err := s.finalize(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Load reads path, parses and validates it, and returns a Loader holding the
// result. Returns an error (fail-fast at startup) if the initial load fails
// — there is no "start with an empty schema" mode, since that would silently
// accept everything.
func Load(path string, log *slog.Logger) (*Loader, error) {
	if log == nil {
		log = slog.Default()
	}
	l := &Loader{path: path, log: log}
	if err := l.reload(); err != nil {
		return nil, err
	}
	return l, nil
}

// Current returns the presently active schema. Never returns nil once Load
// has succeeded.
func (l *Loader) Current() *Schema {
	return l.current.Load()
}

func (l *Loader) reload() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("allowlist: read %s: %w", l.path, err)
	}
	s, err := Parse(data)
	if err != nil {
		return err
	}
	l.current.Store(s)
	return nil
}

// Watch starts an fsnotify watch on the schema file and hot-reloads it on
// write/create/rename events (covers atomic-rename-based editors and plain
// in-place writes). A reload that fails to parse/validate is logged and
// discarded — the previously active Schema keeps serving traffic, so a bad
// edit to the allowlist file never takes ingest down or opens it up.
//
// Watch blocks until stopCh is closed; run it in its own goroutine.
func (l *Loader) Watch(stopCh <-chan struct{}) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("allowlist: watch: %w", err)
	}
	l.watcher = w
	if err := w.Add(l.path); err != nil {
		w.Close()
		return fmt.Errorf("allowlist: watch %s: %w", l.path, err)
	}

	for {
		select {
		case <-stopCh:
			return w.Close()
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			if err := l.reload(); err != nil {
				l.log.Error("allowlist: reload failed, keeping previous schema", "error", err, "path", l.path)
				continue
			}
			l.log.Info("allowlist: schema reloaded", "path", l.path, "schema_version", l.Current().SchemaVersion)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			l.log.Error("allowlist: watcher error", "error", err)
		}
	}
}
