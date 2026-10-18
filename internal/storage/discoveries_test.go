package storage

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDiscoveryEventsFromLog(t *testing.T) {
	tenant := uuid.New()
	at := time.Now()
	row := func(event string, attrs Attrs) LogRow {
		return LogRow{Time: at, TenantID: tenant, EventName: event, LogAttributes: attrs}
	}
	type want struct{ kind, name string }
	cases := []struct {
		name string
		row  LogRow
		want []want
	}{
		{"feature activation", row("cli.feature.activated", Attrs{"cli.install_id": "i", "cli.activation.feature": "deploy"}),
			[]want{{DiscoveryFeature, "deploy"}}},
		{"feature activation without install id (anonymous tier)", row("cli.feature.activated", Attrs{"cli.activation.feature": "deploy"}), nil},
		{"feature activation without a feature", row("cli.feature.activated", Attrs{"cli.install_id": "i"}), nil},
		{"flags from a live batch", row("cli.command.invoked", Attrs{"cli.install_id": "i", "cli.flag.names": []string{"--force", "-v"}}),
			[]want{{DiscoveryFlag, "--force"}, {DiscoveryFlag, "-v"}}},
		{"flags read back from JSON, deduplicated, empties dropped", row("cli.command.invoked", Attrs{"cli.install_id": "i", "cli.flag.names": []any{"--force", "", "--force", 7}}),
			[]want{{DiscoveryFlag, "--force"}}},
		{"invocation without install id", row("cli.command.invoked", Attrs{"cli.flag.names": []string{"--force"}}), nil},
		{"invocation without flags", row("cli.command.invoked", Attrs{"cli.install_id": "i"}), nil},
		{"unrelated event", row("cli.command.completed", Attrs{"cli.install_id": "i", "cli.flag.names": []string{"--force"}}), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []want
			for _, e := range DiscoveryEventsFromLog(c.row) {
				if e.TenantID != tenant || e.InstallID != "i" || !e.At.Equal(at) {
					t.Errorf("event = %+v, want tenant/install/time carried over", e)
				}
				got = append(got, want{e.Kind, e.Name})
			}
			sort.Slice(got, func(i, j int) bool { return got[i].name < got[j].name })
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
