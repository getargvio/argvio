package storage

import (
	"strconv"
	"strings"
)

// semver is the subset of semantic versioning install tracking needs: an
// optional leading "v", up to three numeric dot-separated parts (missing
// ones are zero), an optional prerelease after "-", and build metadata after
// "+" which is ignored.
type semver struct {
	nums [3]uint64
	pre  []string
}

func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v semver
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre := s[i+1:]
		s = s[:i]
		if pre == "" {
			return v, false
		}
		v.pre = strings.Split(pre, ".")
	}
	parts := strings.Split(s, ".")
	if s == "" || len(parts) > 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return v, false
		}
		v.nums[i] = n
	}
	return v, true
}

// compareVersions orders a and b: negative if a < b, positive if a > b, zero
// if they are equal as versions. ok is false if either doesn't parse.
func compareVersions(a, b string) (cmp int, ok bool) {
	va, okA := parseSemver(a)
	vb, okB := parseSemver(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range va.nums {
		if va.nums[i] != vb.nums[i] {
			if va.nums[i] < vb.nums[i] {
				return -1, true
			}
			return 1, true
		}
	}
	// A version with a prerelease sorts before the same version without one.
	switch {
	case len(va.pre) == 0 && len(vb.pre) == 0:
		return 0, true
	case len(va.pre) == 0:
		return 1, true
	case len(vb.pre) == 0:
		return -1, true
	}
	for i := 0; i < len(va.pre) && i < len(vb.pre); i++ {
		if c := comparePreIdent(va.pre[i], vb.pre[i]); c != 0 {
			return c, true
		}
	}
	switch {
	case len(va.pre) < len(vb.pre):
		return -1, true
	case len(va.pre) > len(vb.pre):
		return 1, true
	}
	return 0, true
}

// comparePreIdent compares two prerelease identifiers: numeric ones
// numerically and below alphanumeric ones, which compare as strings.
func comparePreIdent(a, b string) int {
	na, errA := strconv.ParseUint(a, 10, 64)
	nb, errB := strconv.ParseUint(b, 10, 64)
	switch {
	case errA == nil && errB == nil:
		switch {
		case na < nb:
			return -1
		case na > nb:
			return 1
		}
		return 0
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}
