package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetricValidate(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(good, []byte("version: 1\nservice: vulnapi\nmetrics:\n  - {name: vulns.found, kind: counter, title: Vulns, dims: [severity]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("version: 1\nservice: vulnapi\nmetrics:\n  - {name: vulns.found, kind: gauge, title: Vulns}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) (string, error) {
		cmd := NewRootCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	out, err := run("metric", "validate", "-f", good)
	if err != nil || !strings.Contains(out, "1 metrics for vulnapi, valid") {
		t.Errorf("good file: %q err=%v", out, err)
	}
	if _, err := run("metric", "validate", "-f", bad); err == nil || !strings.Contains(err.Error(), "kind must be") {
		t.Errorf("bad file: err=%v, want the kind problem", err)
	}
	if _, err := run("metric", "validate", "-f", filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a missing file must fail")
	}
}
