package storage

// MeasureBounds are the explicit histogram bucket boundaries (ms) of
// cli.measure, so len(MeasureBounds)+1 buckets: the first is (-inf, 1], the
// last (300000, +inf). They must equal explicit_bounds in
// schema/allowlist/v1.yaml, which ingest enforces, and the bkt_0..bkt_N
// columns of cagg_measures_* (migration 0014) are one per bucket; a test
// checks the first, and changing them takes a new migration.
var MeasureBounds = []float64{1, 5, 10, 50, 100, 500, 1000, 5000, 10000, 60000, 300000}

// MeasureBucketCount is how many buckets MeasureBounds makes.
const MeasureBucketCount = 12

// measureQuantile estimates the q-quantile (0 < q <= 1) of the observations
// behind counts, one per bucket of MeasureBounds, by interpolating linearly
// inside the bucket the rank falls in. The estimate is only as precise as the
// bucket is narrow: it can't be off by more than the bucket's width. The
// first bucket is taken to start at min and the last to end at max when
// those are known, else at 0 and at the last bound; the result is clamped to
// [min, max]. nil if there are no observations.
func measureQuantile(counts [MeasureBucketCount]int64, min, max *float64, q float64) *float64 {
	var total int64
	for _, c := range counts {
		total += c
	}
	if total <= 0 {
		return nil
	}
	rank := q * float64(total)
	var cum int64
	for i, c := range counts {
		if c == 0 {
			continue
		}
		if float64(cum+c) < rank && i < len(counts)-1 {
			cum += c
			continue
		}
		var lower, upper float64
		if i > 0 {
			lower = MeasureBounds[i-1]
		} else if min != nil && *min < MeasureBounds[0] {
			lower = *min
		}
		if i < len(MeasureBounds) {
			upper = MeasureBounds[i]
		} else {
			upper = lower
			if max != nil && *max > lower {
				upper = *max
			}
		}
		v := lower + (upper-lower)*(rank-float64(cum))/float64(c)
		if min != nil && v < *min {
			v = *min
		}
		if max != nil && v > *max {
			v = *max
		}
		return &v
	}
	return nil
}
