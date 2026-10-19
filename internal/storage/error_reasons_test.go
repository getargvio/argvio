package storage

import (
	"slices"
	"testing"
)

func TestPlanErrorReasons(t *testing.T) {
	counts := map[string]int64{"a": 1, "b": 5, "c": 2, "d": 2, "known": 9}
	names := []string{"a", "b", "c", "d", "known"}
	known := map[string]bool{"known": true}

	cases := []struct {
		name       string
		room       int
		wantAdd    []string
		wantFolded int64
	}{
		{"room for all", 10, []string{"b", "c", "d", "a"}, 0},
		{"most frequent first, ties by name", 2, []string{"b", "c"}, 3},
		{"full", 0, nil, 10},
		{"over the cap (lowered limit)", -3, nil, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			add, folded := planErrorReasons(names, known, counts, c.room)
			if !slices.Equal(add, c.wantAdd) || folded != c.wantFolded {
				t.Errorf("got add=%v folded=%d, want add=%v folded=%d", add, folded, c.wantAdd, c.wantFolded)
			}
		})
	}

	if add, folded := planErrorReasons([]string{"known"}, known, counts, 0); add != nil || folded != 0 {
		t.Errorf("known reasons never fold: add=%v folded=%d", add, folded)
	}
}
