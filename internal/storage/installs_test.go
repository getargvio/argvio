package storage

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b   string
		want   int
		wantOK bool
	}{
		{"1.0.0", "1.0.0", 0, true},
		{"1.0.0", "v1.0.0", 0, true},
		{"1.2", "1.2.0", 0, true},
		{"1.9.0", "1.10.0", -1, true},
		{"2.0.0", "1.99.99", 1, true},
		{"1.0.0-rc.1", "1.0.0", -1, true},
		{"1.0.0-rc.2", "1.0.0-rc.10", -1, true},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1, true},
		{"1.0.0-1", "1.0.0-alpha", -1, true},
		{"1.0.0+build5", "1.0.0", 0, true},
		{"dev", "1.0.0", 0, false},
		{"1.0.0", "", 0, false},
		{"1.0.0-", "1.0.0", 0, false},
		{"1.2.3.4", "1.2.3", 0, false},
	}
	for _, c := range cases {
		got, ok := compareVersions(c.a, c.b)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("compareVersions(%q, %q) = (%d, %v), want (%d, %v)", c.a, c.b, got, ok, c.want, c.wantOK)
		}
	}
}

func sp(s string) *string { return &s }

func ev(at time.Time, version string) InstallEvent {
	return InstallEvent{InstallID: "i", At: at, Version: sp(version)}
}

func completed(at time.Time, version string, exit int32) InstallEvent {
	e := ev(at, version)
	e.Completed, e.ExitCode = true, &exit
	return e
}

func TestPlanInstall(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }

	t.Run("new install takes first and last from its events regardless of order", func(t *testing.T) {
		st, changes := planInstall(nil, []InstallEvent{ev(h(5), "1.1.0"), ev(h(1), "1.0.0")})
		if !st.FirstSeen.Equal(h(1)) || !st.LastSeen.Equal(h(5)) {
			t.Errorf("seen = %v..%v", st.FirstSeen, st.LastSeen)
		}
		if *st.FirstVersion != "1.0.0" || *st.LastVersion != "1.1.0" {
			t.Errorf("versions = %s..%s", *st.FirstVersion, *st.LastVersion)
		}
		if len(changes) != 1 || !changes[0].Upgrade || changes[0].From != "1.0.0" || changes[0].To != "1.1.0" || !changes[0].At.Equal(h(5)) {
			t.Errorf("changes = %+v", changes)
		}
	})

	t.Run("downgrades and prerelease moves are classified", func(t *testing.T) {
		_, changes := planInstall(nil, []InstallEvent{ev(h(1), "2.0.0"), ev(h(2), "1.9.0"), ev(h(3), "1.9.1-rc.1"), ev(h(4), "1.9.1")})
		want := []bool{false, true, true} // 2.0.0>1.9.0, 1.9.0<1.9.1-rc.1, rc<final
		if len(changes) != len(want) {
			t.Fatalf("changes = %+v", changes)
		}
		for i, c := range changes {
			if c.Upgrade != want[i] {
				t.Errorf("change %d upgrade = %v, want %v", i, c.Upgrade, want[i])
			}
		}
	})

	t.Run("a stale batch moves first_seen only and never records a change", func(t *testing.T) {
		existing := installState{FirstSeen: h(10), LastSeen: h(20), FirstVersion: sp("1.0.0"), LastVersion: sp("2.0.0")}
		st, changes := planInstall(&existing, []InstallEvent{ev(h(5), "0.9.0"), ev(h(15), "1.5.0")})
		if len(changes) != 0 {
			t.Errorf("changes = %+v, want none", changes)
		}
		if !st.FirstSeen.Equal(h(5)) || *st.FirstVersion != "0.9.0" {
			t.Errorf("first = %v %s", st.FirstSeen, *st.FirstVersion)
		}
		if !st.LastSeen.Equal(h(20)) || *st.LastVersion != "2.0.0" {
			t.Errorf("last = %v %s", st.LastSeen, *st.LastVersion)
		}
	})

	t.Run("a newer batch continues from the stored version", func(t *testing.T) {
		existing := installState{FirstSeen: h(1), LastSeen: h(2), FirstVersion: sp("1.0.0"), LastVersion: sp("1.0.0")}
		st, changes := planInstall(&existing, []InstallEvent{ev(h(3), "1.0.0"), ev(h(4), "1.1.0")})
		if len(changes) != 1 || changes[0].From != "1.0.0" || changes[0].To != "1.1.0" {
			t.Errorf("changes = %+v", changes)
		}
		if *st.LastVersion != "1.1.0" {
			t.Errorf("last version = %s", *st.LastVersion)
		}
	})

	t.Run("unparseable versions update last_version without a change record", func(t *testing.T) {
		st, changes := planInstall(nil, []InstallEvent{ev(h(1), "1.0.0"), ev(h(2), "dev")})
		if len(changes) != 0 || *st.LastVersion != "dev" {
			t.Errorf("changes = %+v last = %s", changes, *st.LastVersion)
		}
	})

	t.Run("first command and first success are the earliest of each", func(t *testing.T) {
		st, _ := planInstall(nil, []InstallEvent{
			completed(h(4), "1.0.0", 0),
			completed(h(2), "1.0.0", 1),
			completed(h(3), "1.0.0", 0),
			ev(h(1), "1.0.0"),
		})
		if st.FirstCommandAt == nil || !st.FirstCommandAt.Equal(h(2)) {
			t.Errorf("first command = %v", st.FirstCommandAt)
		}
		if st.FirstSuccessAt == nil || !st.FirstSuccessAt.Equal(h(3)) {
			t.Errorf("first success = %v", st.FirstSuccessAt)
		}
	})

	t.Run("a session with no completed command has neither", func(t *testing.T) {
		st, _ := planInstall(nil, []InstallEvent{ev(h(1), "1.0.0")})
		if st.FirstCommandAt != nil || st.FirstSuccessAt != nil {
			t.Errorf("got %v %v", st.FirstCommandAt, st.FirstSuccessAt)
		}
	})
}

func TestInstallEventFromLog(t *testing.T) {
	tenant := uuid.New()
	row := func(event string, attrs Attrs) LogRow {
		return LogRow{Time: time.Now(), TenantID: tenant, EventName: event, LogAttributes: attrs}
	}
	if _, ok := InstallEventFromLog(row("cli.help.viewed", Attrs{"cli.install_id": "i"})); ok {
		t.Error("help events must not take part")
	}
	if _, ok := InstallEventFromLog(row("cli.session.started", Attrs{})); ok {
		t.Error("anonymous sessions have no install id")
	}
	e, ok := InstallEventFromLog(row("cli.session.started", Attrs{"cli.install_id": "i", "cli.install.method": "brew"}))
	if !ok || e.InstallID != "i" || e.InstallMeth != "brew" || e.Completed || e.TenantID != tenant {
		t.Errorf("got %+v, %v", e, ok)
	}
	e, ok = InstallEventFromLog(row("cli.command.completed", Attrs{"cli.install_id": "i"}))
	if !ok || !e.Completed {
		t.Errorf("got %+v, %v", e, ok)
	}
}
