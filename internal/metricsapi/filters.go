package metricsapi

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getargvio/argvio/internal/storage"
)

// parseFilters builds a storage.Filters for an aggregate endpoint. Those read
// rollups and event queries that carry no attribute columns, so attr.*,
// attr_contains.* and attr_in.* are rejected rather than silently ignored,
// which would return unfiltered numbers that look filtered.
func (s *Server) parseFilters(r *http.Request) (storage.Filters, error) {
	if key := firstAttrParam(r.URL.Query()); key != "" {
		return storage.Filters{}, fmt.Errorf("%s is not supported on this endpoint: attribute filters only apply to /v1/traces", key)
	}
	return s.parseFiltersWithAttrs(r)
}

// firstAttrParam returns the alphabetically first attribute filter param
// present in q, or "" if there is none, so the error is deterministic.
func firstAttrParam(q map[string][]string) string {
	var found []string
	for key := range q {
		if isAttrParam(key) {
			found = append(found, key)
		}
	}
	if len(found) == 0 {
		return ""
	}
	return slices.Min(found)
}

func isAttrParam(key string) bool {
	return strings.HasPrefix(key, "attr.") ||
		strings.HasPrefix(key, "attr_contains.") ||
		strings.HasPrefix(key, "attr_in.")
}

// parseFiltersWithAttrs builds a storage.Filters from query parameters,
// enforcing MaxTimeRangeSpan (the "no 5-year raw scan" guard from
// docs/configuration.md) and page-size bounds before the request ever
// reaches internal/storage. Only /v1/traces, which reads raw span
// attributes, calls it directly.
func (s *Server) parseFiltersWithAttrs(r *http.Request) (storage.Filters, error) {
	q := r.URL.Query()
	var f storage.Filters

	from, to, err := parseTimeRange(q)
	if err != nil {
		return f, err
	}
	if to.Before(from) {
		return f, fmt.Errorf("to must not be before from")
	}
	if to.Sub(from) > s.MaxTimeRangeSpan {
		return f, fmt.Errorf("requested time range %s exceeds max_time_range_span %s", to.Sub(from), s.MaxTimeRangeSpan)
	}
	f.Time = storage.TimeRange{From: from, To: to}

	f.CLIVersions = multiParam(q, "cli_version")
	f.OSes = multiParam(q, "os")
	f.Arches = multiParam(q, "arch")
	f.CommandNames = multiParam(q, "command")
	f.IsCI = boolParam(q, "is_ci")

	if v := q.Get("exit_code"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return f, fmt.Errorf("exit_code must be an integer: %w", err)
		}
		n32 := int32(n)
		f.ExitCode = &n32
	}

	f.Attrs = map[string]string{}
	f.AttrContains = map[string]string{}
	f.AttrIn = map[string][]string{}
	for key, vals := range q {
		switch {
		case strings.HasPrefix(key, "attr."):
			if len(vals) > 0 {
				f.Attrs[strings.TrimPrefix(key, "attr.")] = vals[0]
			}
		case strings.HasPrefix(key, "attr_contains."):
			// An empty needle would match every row that has the key, so
			// treat it as absent, like an empty multi-value param.
			if len(vals) > 0 && vals[0] != "" {
				f.AttrContains[strings.TrimPrefix(key, "attr_contains.")] = vals[0]
			}
		case strings.HasPrefix(key, "attr_in."):
			if in := multiParam(q, key); len(in) > 0 {
				f.AttrIn[strings.TrimPrefix(key, "attr_in.")] = in
			}
		}
	}

	limit := s.DefaultResultPageSize
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return f, fmt.Errorf("limit must be a positive integer")
		}
		limit = n
	}
	if limit > s.MaxResultPageSize {
		limit = s.MaxResultPageSize
	}
	f.Limit = limit

	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return f, fmt.Errorf("offset must be a non-negative integer")
		}
		f.Offset = n
	}

	return f, nil
}

func parseTimeRange(q map[string][]string) (from, to time.Time, err error) {
	toStr := first(q, "to")
	fromStr := first(q, "from")
	if toStr == "" {
		to = time.Now()
	} else if to, err = time.Parse(time.RFC3339, toStr); err != nil {
		return from, to, fmt.Errorf("to must be RFC3339: %w", err)
	}
	if fromStr == "" {
		return from, to, fmt.Errorf("from is required (RFC3339 timestamp)")
	}
	if from, err = time.Parse(time.RFC3339, fromStr); err != nil {
		return from, to, fmt.Errorf("from must be RFC3339: %w", err)
	}
	return from, to, nil
}

func first(q map[string][]string, key string) string {
	if v, ok := q[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// multiParam collects every value of a multi-value filter param, accepting
// both repeated keys (`os=linux&os=darwin`) and comma-separated values
// (`os=linux,darwin`), or any mix. Empty entries are dropped, so `os=` is
// the same as omitting the param.
func multiParam(q map[string][]string, key string) []string {
	var out []string
	for _, raw := range q[key] {
		for _, v := range strings.Split(raw, ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

func boolParam(q map[string][]string, key string) *bool {
	v := first(q, key)
	if v == "" {
		return nil
	}
	b := v == "true" || v == "1"
	return &b
}

// parseBucket reads the `bucket` param, defaulting to def when absent and
// rejecting anything not in allowed — endpoints backed by a daily rollup
// can't serve hour, for instance.
func parseBucket(r *http.Request, def storage.Bucket, allowed ...storage.Bucket) (storage.Bucket, error) {
	v := storage.Bucket(r.URL.Query().Get("bucket"))
	if v == "" {
		return def, nil
	}
	if slices.Contains(allowed, v) {
		return v, nil
	}
	names := make([]string, len(allowed))
	for i, b := range allowed {
		names[i] = "'" + string(b) + "'"
	}
	return "", fmt.Errorf("bucket must be one of %s", strings.Join(names, ", "))
}
