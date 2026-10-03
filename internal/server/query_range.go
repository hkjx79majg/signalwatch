package server

import (
	"math"
	"net/http"
	"net/url"
	"sort"
	"time"
)

const queryRangePath = "/api/v1/query-range"

// matrixPoint is one window value of a result series.
type matrixPoint struct {
	Timestamp string  `json:"timestamp"`
	Value     float64 `json:"value"`
}

type matrixSeries struct {
	Labels map[string]string `json:"labels"`
	Points []matrixPoint     `json:"points"`
}

type matrixResponse struct {
	ResultType string         `json:"result_type"`
	Result     []matrixSeries `json:"result"`
}

// seriesWindowValues computes the per-window values of one counter/gauge
// series over the half-open windows cut from start. Histograms never
// participate. A window without a retained sample is absent from the map.
// ok is false when a counter window sum is not a finite number.
func seriesWindowValues(cur *series, start, end, cutoff time.Time, step time.Duration) (map[int]float64, bool) {
	inRange := func(ts time.Time) bool {
		return !ts.Before(cutoff) && !ts.Before(start) && ts.Before(end)
	}
	windowOf := func(ts time.Time) int {
		return int(ts.Sub(start) / step)
	}

	switch cur.typ {
	case "counter":
		sums := make(map[int]float64)
		for _, p := range cur.history {
			if inRange(p.ts) {
				sums[windowOf(p.ts)] += p.value
			}
		}
		for _, v := range sums {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, false
			}
		}
		return sums, true

	case "gauge":
		// history is commit order, so a strictly later iteration position on
		// equal timestamps wins; track the winning index explicitly.
		type pick struct {
			value float64
			ts    time.Time
			idx   int
		}
		best := make(map[int]pick)
		for i, p := range cur.history {
			if !inRange(p.ts) {
				continue
			}
			k := windowOf(p.ts)
			if b, ok := best[k]; !ok || p.ts.After(b.ts) || (p.ts.Equal(b.ts) && i > b.idx) {
				best[k] = pick{value: p.value, ts: p.ts, idx: i}
			}
		}
		out := make(map[int]float64, len(best))
		for k, b := range best {
			out[k] = b.value
		}
		return out, true
	}
	return nil, true
}

// executeQueryRange evaluates a parsed expression per fixed window against a
// single consistent store snapshot. Only counter and gauge series
// participate; histograms are ignored. Aggregations fold only the series
// that have a value in that window, and a group absent in a window emits no
// point. The third result is false when any window value or aggregate is
// non-finite, in which case no partial result is returned.
func (s *metricStore) executeQueryRange(e *queryExpr, start, end time.Time, step time.Duration, nWindows int, now time.Time) ([]matrixSeries, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := now.Add(-metricRetention)

	type matched struct {
		labels map[string]string
		values map[int]float64
	}
	all := make([]matched, 0)
	for _, cur := range s.series {
		if cur.typ != "counter" && cur.typ != "gauge" {
			continue
		}
		if cur.name != e.metric {
			continue
		}
		fits := true
		for k, v := range e.selector {
			got, ok := cur.labels[k]
			if !ok || got != v {
				fits = false
				break
			}
		}
		if !fits {
			continue
		}
		values, finite := seriesWindowValues(cur, start, end, cutoff, step)
		if !finite {
			return nil, false
		}
		all = append(all, matched{labels: copyLabels(cur.labels), values: values})
	}

	windowStamps := make([]string, nWindows)
	for k := 0; k < nWindows; k++ {
		windowStamps[k] = start.Add(time.Duration(k) * step).UTC().Format(time.RFC3339Nano)
	}

	if e.aggregate == "" {
		pointsOf := func(perWindow map[int]float64) []matrixPoint {
			keys := make([]int, 0, len(perWindow))
			for k := range perWindow {
				keys = append(keys, k)
			}
			sort.Ints(keys)
			points := make([]matrixPoint, 0, len(keys))
			for _, k := range keys {
				points = append(points, matrixPoint{Timestamp: windowStamps[k], Value: perWindow[k]})
			}
			return points
		}
		// Sort matched series by full labels once; their points are already
		// ascending by construction. Series without any point are omitted.
		sort.Slice(all, func(i, j int) bool {
			return compareLabels(all[i].labels, all[j].labels) < 0
		})
		out := make([]matrixSeries, 0, len(all))
		for _, m := range all {
			if len(m.values) == 0 {
				continue
			}
			out = append(out, matrixSeries{Labels: m.labels, Points: pointsOf(m.values)})
		}
		return out, true
	}

	// Aggregations: group by projected labels per window. A group exists in a
	// window only when at least one series has a value there.
	type groupAgg struct {
		sum   float64
		min   float64
		max   float64
		count int
	}
	type groupState struct {
		labels map[string]string
		// indexed by window; nil entries are empty windows.
		windows []*groupAgg
	}
	groups := make(map[string]*groupState)
	groupOrder := make([]string, 0)

	for _, m := range all {
		projected := make(map[string]string, len(e.by))
		for _, label := range e.by {
			if v, ok := m.labels[label]; ok {
				projected[label] = v
			}
		}
		key := seriesKey("", projected)
		g, seen := groups[key]
		if !seen {
			g = &groupState{labels: projected, windows: make([]*groupAgg, nWindows)}
			groups[key] = g
			groupOrder = append(groupOrder, key)
		}
		for k, v := range m.values {
			a := g.windows[k]
			if a == nil {
				a = &groupAgg{sum: v, min: v, max: v, count: 1}
				g.windows[k] = a
				continue
			}
			a.count++
			a.sum += v
			if v < a.min {
				a.min = v
			}
			if v > a.max {
				a.max = v
			}
		}
	}

	out := make([]matrixSeries, 0, len(groupOrder))
	for _, key := range groupOrder {
		g := groups[key]
		points := make([]matrixPoint, 0)
		for k, a := range g.windows {
			if a == nil {
				continue // no series had a value in this window; never zero-fill
			}
			var v float64
			switch e.aggregate {
			case "sum":
				v = a.sum
			case "avg":
				v = a.sum / float64(a.count)
			case "min":
				v = a.min
			case "max":
				v = a.max
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, false
			}
			points = append(points, matrixPoint{Timestamp: windowStamps[k], Value: v})
		}
		if len(points) > 0 {
			out = append(out, matrixSeries{Labels: g.labels, Points: points})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return compareLabels(out[i].Labels, out[j].Labels) < 0
	})
	return out, true
}

// ---- HTTP handler ----------------------------------------------------------

func registerQueryRangeHandler(mux *http.ServeMux) {
	mux.HandleFunc(queryRangePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		invalidRange := func() {
			writeAPIError(w, "invalid_query_range", http.StatusBadRequest)
		}

		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			invalidRange()
			return
		}

		// Exactly expr, start, end and step, each present exactly once.
		if len(values) != 4 {
			invalidRange()
			return
		}
		single := func(key string) (string, bool) {
			vals := values[key]
			if len(vals) != 1 {
				return "", false
			}
			return vals[0], true
		}
		exprRaw, ok := single("expr")
		if !ok {
			invalidRange()
			return
		}
		startRaw, ok := single("start")
		if !ok {
			invalidRange()
			return
		}
		endRaw, ok := single("end")
		if !ok {
			invalidRange()
			return
		}
		stepRaw, ok := single("step")
		if !ok {
			invalidRange()
			return
		}

		// The expression grammar is shared verbatim with GET /api/v1/query.
		expr, ok := parseQueryExpr(exprRaw)
		if !ok {
			writeAPIError(w, "invalid_expression", http.StatusBadRequest)
			return
		}

		start, err := time.Parse(time.RFC3339Nano, startRaw)
		if err != nil {
			invalidRange()
			return
		}
		end, err := time.Parse(time.RFC3339Nano, endRaw)
		if err != nil {
			invalidRange()
			return
		}
		if !start.Before(end) {
			invalidRange()
			return
		}
		step, ok := parseStep(stepRaw)
		if !ok {
			invalidRange()
			return
		}

		// Windows are cut consecutively from start over [start, end); the
		// last window may extend past end but only samples with ts < end
		// participate.
		span := end.Sub(start)
		windows := int64(span) / int64(step)
		if int64(span)%int64(step) != 0 {
			windows++
		}
		if windows > maxRangeWindows {
			invalidRange()
			return
		}

		result, finite := tenantStore(r).executeQueryRange(expr, start, end, step, int(windows), time.Now())
		if !finite {
			writeAPIError(w, "invalid_query_range_data", http.StatusUnprocessableEntity)
			return
		}
		if result == nil {
			result = []matrixSeries{}
		}
		writeJSON(w, http.StatusOK, matrixResponse{ResultType: "matrix", Result: result})
	})
}
