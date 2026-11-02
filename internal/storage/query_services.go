package storage

import (
	"context"
	"fmt"
)

// VendorService is one CLI (OTel service.name) that sent vendor counters or
// measures over the query window, with how many distinct names of each it
// sent. Name is nil for data points sent without a service.name, which the
// SDKs always set but a hand-rolled exporter may not.
type VendorService struct {
	Name     *string `json:"name"`
	Counters int64   `json:"counters"`
	Measures int64   `json:"measures"`
}

// VendorServices lists the CLIs a tenant sent vendor metrics for over f.Time,
// the one with the most counter and measure names first. It reads the hourly
// rollups as well as the daily ones, so a CLI that only started sending today
// is listed before the daily rollup has materialized its first day. f.Services
// is ignored: it is what this lists.
func (q *QueryBuilder) VendorServices(ctx context.Context, scope TenantScope, f Filters) ([]VendorService, error) {
	f.Services = nil
	pb := vendorPredicates(scope, f)
	where := pb.where()
	sql := fmt.Sprintf(`
WITH names AS (
    SELECT service_name, 'counter' AS kind, counter_name AS name FROM %[1]s WHERE %[5]s
    UNION
    SELECT service_name, 'counter', counter_name FROM %[2]s WHERE %[5]s
    UNION
    SELECT service_name, 'measure', measure_name FROM %[3]s WHERE %[5]s
    UNION
    SELECT service_name, 'measure', measure_name FROM %[4]s WHERE %[5]s
)
SELECT service_name,
       count(*) FILTER (WHERE kind = 'counter') AS counters,
       count(*) FILTER (WHERE kind = 'measure') AS measures
FROM names
GROUP BY service_name
ORDER BY count(*) DESC, service_name ASC NULLS LAST`,
		caggCountersHourly, caggCountersDaily, caggMeasuresHourly, caggMeasuresDaily, where)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: vendor services: %w", err)
	}
	defer rows.Close()
	out := []VendorService{}
	for rows.Next() {
		var s VendorService
		if err := rows.Scan(&s.Name, &s.Counters, &s.Measures); err != nil {
			return nil, fmt.Errorf("storage: scan vendor services: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
