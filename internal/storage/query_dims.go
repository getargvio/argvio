package storage

import (
	"fmt"
	"regexp"
	"sort"
)

// MaxDimGroupBy is how many dimension keys a query may group by at once (a
// breakdown, or a two-key table).
const MaxDimGroupBy = 2

// MaxDimWhere is how many dimension keys a query may filter on at once.
const MaxDimWhere = 8

var dimIdentifier = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidDimKey reports whether s can be a dimension key or value: the
// identifier rule the allowlist enforces at ingest.
func ValidDimKey(s string) bool {
	return dimIdentifier.MatchString(s)
}

// DimQuery narrows a vendor counter or measure series by dimension values
// (Where: key -> accepted values, ANDed across keys, ORed within one) and
// breaks it down by up to MaxDimGroupBy keys (GroupBy).
type DimQuery struct {
	GroupBy []string
	Where   map[string][]string
	// Reach also counts the installs behind the series (Reach, and
	// InstallsReached per bucket and group).
	Reach bool
}

// Validate checks the keys and values against the identifier rule and the
// bounds above.
func (d DimQuery) Validate() error {
	if len(d.GroupBy) > MaxDimGroupBy {
		return fmt.Errorf("group_by takes at most %d dimension keys", MaxDimGroupBy)
	}
	seen := map[string]bool{}
	for _, k := range d.GroupBy {
		if !ValidDimKey(k) {
			return fmt.Errorf("group_by %q is not a valid dimension key", k)
		}
		if seen[k] {
			return fmt.Errorf("group_by %q is repeated", k)
		}
		seen[k] = true
	}
	if len(d.Where) > MaxDimWhere {
		return fmt.Errorf("where takes at most %d dimension keys", MaxDimWhere)
	}
	for k, vs := range d.Where {
		if !ValidDimKey(k) {
			return fmt.Errorf("where key %q is not a valid dimension key", k)
		}
		for _, v := range vs {
			if !ValidDimKey(v) {
				return fmt.Errorf("where value %q for %s is not a valid dimension value", v, k)
			}
		}
	}
	return nil
}

// dimExpr is the SQL expression extracting key's value from the label column
// col, as DimValue does: a whole tuple folded to "other" has every key as
// "other"; a plain label is the key "label"; NULL when the label lacks key.
// The key goes into a bound regular expression, never into the SQL text.
func dimExpr(pb *predicateBuilder, col, key string) string {
	pattern := pb.nextPlaceholder(`(?:^|;)` + regexp.QuoteMeta(key) + `=([^;]*)`)
	plain := "NULL"
	if key == DimLabelKey {
		plain = col
	}
	return fmt.Sprintf(`(CASE WHEN %[1]s = 'other' THEN 'other' WHEN strpos(%[1]s, '=') = 0 THEN %[2]s ELSE substring(%[1]s from %[3]s::text) END)`,
		col, plain, pattern)
}

// addWhere adds d.Where's predicates on the label column col to pb.
func (d DimQuery) addWhere(pb *predicateBuilder, col string) {
	keys := make([]string, 0, len(d.Where))
	for k := range d.Where {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(d.Where[k]) == 0 {
			continue
		}
		pb.add(dimExpr(pb, col, k)+" = ANY($%d)", d.Where[k])
	}
}

// groupExprs returns one dimExpr per d.GroupBy key.
func (d DimQuery) groupExprs(pb *predicateBuilder, col string) []string {
	out := make([]string, len(d.GroupBy))
	for i, k := range d.GroupBy {
		out[i] = dimExpr(pb, col, k)
	}
	return out
}

// dimsOf maps d.GroupBy to the scanned group values (nil: the label lacks
// that key).
func (d DimQuery) dimsOf(values []*string) map[string]*string {
	m := make(map[string]*string, len(d.GroupBy))
	for i, k := range d.GroupBy {
		m[k] = values[i]
	}
	return m
}

// groupKey is a comparable identity for a group's values.
func groupKey(values []*string) string {
	var b []byte
	for _, v := range values {
		if v == nil {
			b = append(b, 0)
		} else {
			b = append(b, 1)
			b = append(b, *v...)
		}
		b = append(b, 0xff)
	}
	return string(b)
}

// groupKeyOf is groupKey for a group's Dims, in dq.GroupBy order.
func (d DimQuery) groupKeyOf(dims map[string]*string) string {
	values := make([]*string, len(d.GroupBy))
	for i, k := range d.GroupBy {
		values[i] = dims[k]
	}
	return groupKey(values)
}
