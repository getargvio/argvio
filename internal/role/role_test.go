package role

import "testing"

func TestKnown(t *testing.T) {
	for r, want := range map[string]bool{
		Viewer: true, Admin: true,
		"": false, "Admin": false, "VIEWER": false, "viewer ": false, "owner": false,
	} {
		if got := Known(r); got != want {
			t.Errorf("Known(%q) = %v, want %v", r, got, want)
		}
	}
}
