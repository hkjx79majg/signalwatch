package server

import (
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const metricRangePath = "/api/v1/metric-range"

const (
	// metricRetention is how long accepted samples stay queryable and how far
	// back from the receive instant a write timestamp may reach.
	metricRetention = 24 * time.Hour
	// maxFutureSkew is how far ahead of the receive instant a write timestamp
	// may lie.
	maxFutureSkew = 5 * time.Minute

	maxRangeStepSeconds = 3600
	maxRangeWindows     = 10000
)

// historyPoint is one accepted sample, kept per series in commit order.
type historyPoint struct {
	ts    time.Time
	value float64
}

type rangePointOutput struct {
	Timestamp string         `json:"timestamp"`
	Value     *float64       `json:"value,omitempty"`
	Count     *int64         `json:"count,omitempty"`
	Sum       *float64       `json:"sum,omitempty"`
	Buckets   []bucketOutput `json:"buckets,omitempty"`
}

type rangeSeriesOutput struct {
	Name   string             `json:"name"`
	Type   string             `json:"type"`
	Labels map[string]string  `json:"labels"`
	Points []rangePointOutput `json:"points"`
}

// rangeQuery aggregates retained history into the half-open windows
// [start+k*step, start+(k+1)*step) that cover [start, end). Samples older
// than now-metricRetention are treated as evicted. ok is false when an
// aggregation overflows to a non-finite number; no partial result is
// returned in that case.
func (s *metricStore) rangeQuery(name string, selector map[string]string, start, end time.Time, step time.Duration, now time.Time) (out []rangeSeriesOutput, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := now.Add(-metricRetention)
	matched := make([]*series, 0)
	for _, cur := range s.series {
		if cur.name != name {
			continue
		}
		fits := true
		for k, v := range selector {
			got, ok := cur.labels[k]
			if !ok || got != v {
				fits = false
				break
			}
		}
		if fits {
			matched = append(matched, cur)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		return compareLabels(matched[i].labels, matched[j].labels) < 0
	})

	out = make([]rangeSeriesOutput, 0, len(matched))
	for _, cur := range matched {
		points, finite := aggregateWindows(cur, start, end, step, cutoff)
		if !finite {
			return nil, false
		}
		if len(points) == 0 {
			continue // empty windows produce no points; point-less series are omitted
		}
		out = append(out, rangeSeriesOutput{
			Name:   cur.name,
			Type:   cur.typ,
			Labels: copyLabels(cur.labels),
			Points: points,
		})
	}
	return out, true
}

// aggregateWindows builds the points of one series. cur.history is in commit
// order, which is also the tie-break order for gauge observations that share
// a timestamp.
func aggregateWindows(cur *series, start, end time.Time, step time.Duration, cutoff time.Time) ([]rangePointOutput, bool) {
	inRange := func(ts time.Time) bool {
		return !ts.Before(cutoff) && !ts.Before(start) && ts.Before(end)
	}
	windowOf := func(ts time.Time) int {
		return int(ts.Sub(start) / step)
	}
	windowStart := func(k int) string {
		return start.Add(time.Duration(k) * step).UTC().Format(time.RFC3339Nano)
	}

	switch cur.typ {
	case "counter":
		sums := make(map[int]float64)
		for _, p := range cur.history {
			if inRange(p.ts) {
				sums[windowOf(p.ts)] += p.value
			}
		}
		points := make([]rangePointOutput, 0, len(sums))
		for _, k := range sortedWindowKeys(sums) {
			v := sums[k]
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, false
			}
			points = append(points, rangePointOutput{Timestamp: windowStart(k), Value: &v})
		}
		return points, true

	case "gauge":
		best := make(map[int]historyPoint)
		for _, p := range cur.history {
			if !inRange(p.ts) {
				continue
			}
			k := windowOf(p.ts)
			if cur2, ok := best[k]; !ok || !p.ts.Before(cur2.ts) {
				best[k] = p // equal timestamps: the later commit wins
			}
		}
		points := make([]rangePointOutput, 0, len(best))
		for _, k := range sortedWindowKeys(best) {
			v := best[k].value
			points = append(points, rangePointOutput{Timestamp: windowStart(k), Value: &v})
		}
		return points, true

	case "histogram":
		type histAgg struct {
			count   int64
			sum     float64
			buckets []int64
		}
		aggs := make(map[int]*histAgg)
		for _, p := range cur.history {
			if !inRange(p.ts) {
				continue
			}
			k := windowOf(p.ts)
			a, ok := aggs[k]
			if !ok {
				a = &histAgg{buckets: make([]int64, len(cur.bucketBounds))}
				aggs[k] = a
			}
			a.count++
			a.sum += p.value
			for i, bound := range cur.bucketBounds {
				if p.value <= bound {
					a.buckets[i]++
				}
			}
		}
		points := make([]rangePointOutput, 0, len(aggs))
		for _, k := range sortedWindowKeys(aggs) {
			a := aggs[k]
			if math.IsNaN(a.sum) || math.IsInf(a.sum, 0) {
				return nil, false
			}
			count, sum := a.count, a.sum
			buckets := make([]bucketOutput, len(cur.bucketBounds))
			for i, bound := range cur.bucketBounds {
				buckets[i] = bucketOutput{LE: bound, Count: a.buckets[i]}
			}
			points = append(points, rangePointOutput{
				Timestamp: windowStart(k),
				Count:     &count,
				Sum:       &sum,
				Buckets:   buckets,
			})
		}
		return points, true
	}
	return nil, true
}

func sortedWindowKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// parseStep validates the step parameter: integer seconds in [1, 3600].
func parseStep(raw string) (time.Duration, bool) {
	if raw == "" {
		return 0, false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxRangeStepSeconds {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

func registerMetricRangeHandler(mux *http.ServeMux) {
	mux.HandleFunc(metricRangePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		invalid := func() {
			writeAPIError(w, "invalid_metric_range", http.StatusBadRequest)
		}

		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			invalid()
			return
		}

		// Only name, label.<key>, start, end and step are accepted; every
		// parameter key may appear at most once.
		selector := make(map[string]string)
		var name, startRaw, endRaw, stepRaw string
		var haveName, haveStart, haveEnd, haveStep bool
		for key, vals := range values {
			if len(vals) != 1 {
				invalid()
				return
			}
			v := vals[0]
			switch {
			case key == "name":
				if !identPattern.MatchString(v) {
					invalid()
					return
				}
				name, haveName = v, true
			case key == "start":
				startRaw, haveStart = v, true
			case key == "end":
				endRaw, haveEnd = v, true
			case key == "step":
				stepRaw, haveStep = v, true
			case strings.HasPrefix(key, "label."):
				labelKey := strings.TrimPrefix(key, "label.")
				if !identPattern.MatchString(labelKey) {
					invalid()
					return
				}
				selector[labelKey] = v
			default:
				invalid()
				return
			}
		}
		if !haveName || !haveStart || !haveEnd || !haveStep {
			invalid()
			return
		}

		start, err := time.Parse(time.RFC3339Nano, startRaw)
		if err != nil {
			invalid()
			return
		}
		end, err := time.Parse(time.RFC3339Nano, endRaw)
		if err != nil {
			invalid()
			return
		}
		if !start.Before(end) {
			invalid()
			return
		}
		step, ok := parseStep(stepRaw)
		if !ok {
			invalid()
			return
		}

		// Half-open windows are cut consecutively from start; the last one
		// may extend past end but samples are only counted while ts < end.
		span := end.Sub(start)
		windows := int64(span) / int64(step)
		if int64(span)%int64(step) != 0 {
			windows++
		}
		if windows > maxRangeWindows {
			invalid()
			return
		}

		out, finite := tenantStore(r).rangeQuery(name, selector, start, end, step, time.Now())
		if !finite {
			writeAPIError(w, "invalid_metric_range_data", http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"series": out})
	})
}
