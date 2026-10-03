package server

import (
	"math"
	"net/http"
	"net/url"
	"sort"
	"time"
)

const queryRangePath = "/api/v1/query-range"

// ---- matrix execution ------------------------------------------------------

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

// windowValues reduces one counter/gauge series to the value it contributes to
// each half-open window [start+k*step, start+(k+1)*step). Counter windows sum
// the increments; gauge windows take the latest-timed observation, breaking
// timestamp ties by commit order (cur.history is commit ordered). Samples at or
// beyond end, before start, or older than the retention cutoff are ignored.
// Histograms never participate. The second result is false when a window value
// is not finite.
func windowValues(cur *series, start, end time.Time, step time.Duration, cutoff time.Time) (map[int]float64, bool) {
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
		best := make(map[int]historyPoint)
		for _, p := range cur.history {
			if !inRange(p.ts) {
				continue
			}
			k := windowOf(p.ts)
			if prev, ok := best[k]; !ok || !p.ts.Before(prev.ts) {
				best[k] = p // equal timestamps: the later commit wins
			}
		}
		values := make(map[int]float64, len(best))
		for k, p := range best {
			if math.IsNaN(p.value) || math.IsInf(p.value, 0) {
				return nil, false
			}
			values[k] = p.value
		}
		return values, true
	}
	return nil, true
}

// matrixPoints turns window index -> value into timestamped points ordered by
// ascending window start. ok is false when any value is not finite.
func matrixPoints(values map[int]float64, start time.Time, step time.Duration) ([]matrixPoint, bool) {
	keys := make([]int, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	points := make([]matrixPoint, 0, len(keys))
	for _, k := range keys {
		v := values[k]
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		points = append(points, matrixPoint{
			Timestamp: start.Add(time.Duration(k) * step).UTC().Format(time.RFC3339Nano),
			Value:     v,
		})
	}
	return points, true
}

// seriesWindow pairs one matching series' full labels with its per-window
// values; series without any window value are absent.
type seriesWindow struct {
	labels  map[string]string
	windows map[int]float64
}

// executeQueryRange evaluates a parsed expression over history against a single
// consistent store snapshot. Only retained counter/gauge samples participate;
// histograms are ignored. A bare selector yields one series per matching series
// under its full label set. An aggregation folds, independently in every window,
// only the series that have a value in that window, grouped by the projected by
// labels; a group with no participant in a window emits no point there and no
// zero is filled in. The result is false when any window value or aggregate is
// not a finite number.
func (s *metricStore) executeQueryRange(e *queryExpr, start, end time.Time, step time.Duration, now time.Time) ([]matrixSeries, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := now.Add(-metricRetention)
	matched := make([]*series, 0)
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
		if fits {
			matched = append(matched, cur)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		return compareLabels(matched[i].labels, matched[j].labels) < 0
	})

	seriesWindows := make([]seriesWindow, 0, len(matched))
	for _, cur := range matched {
		values, finite := windowValues(cur, start, end, step, cutoff)
		if !finite {
			return nil, false
		}
		if len(values) == 0 {
			continue // empty windows produce no points; point-less series are omitted
		}
		seriesWindows = append(seriesWindows, seriesWindow{labels: copyLabels(cur.labels), windows: values})
	}

	if e.aggregate == "" {
		out := make([]matrixSeries, 0, len(seriesWindows))
		for _, sw := range seriesWindows {
			points, finite := matrixPoints(sw.windows, start, step)
			if !finite {
				return nil, false
			}
			out = append(out, matrixSeries{Labels: sw.labels, Points: points})
		}
		sort.Slice(out, func(i, j int) bool {
			return compareLabels(out[i].Labels, out[j].Labels) < 0
		})
		return out, true
	}

	// Window-local accumulators per projected group. Groups are created only
	// from series that carry at least one window value, but a group still emits
	// no point for a window in which none of its series participated.
	type groupAcc struct {
		value float64
		count int
	}
	groupLabels := make(map[string]map[string]string)
	groupOrder := make([]string, 0)
	windowGroups := make(map[int]map[string]*groupAcc)
	for _, sw := range seriesWindows {
		projected := make(map[string]string, len(e.by))
		for _, label := range e.by {
			if v, ok := sw.labels[label]; ok {
				projected[label] = v
			}
		}
		gkey := seriesKey("", projected)
		if _, seen := groupLabels[gkey]; !seen {
			groupLabels[gkey] = projected
			groupOrder = append(groupOrder, gkey)
		}
		for k, v := range sw.windows {
			groups, ok := windowGroups[k]
			if !ok {
				groups = make(map[string]*groupAcc)
				windowGroups[k] = groups
			}
			g, seen := groups[gkey]
			if !seen {
				g = &groupAcc{value: v, count: 1}
				groups[gkey] = g
				continue
			}
			g.count++
			switch e.aggregate {
			case "sum", "avg":
				g.value += v
			case "min":
				if v < g.value {
					g.value = v
				}
			case "max":
				if v > g.value {
					g.value = v
				}
			}
		}
	}

	windowKeys := make([]int, 0, len(windowGroups))
	for k := range windowGroups {
		windowKeys = append(windowKeys, k)
	}
	sort.Ints(windowKeys)

	sort.Slice(groupOrder, func(i, j int) bool {
		return compareLabels(groupLabels[groupOrder[i]], groupLabels[groupOrder[j]]) < 0
	})
	out := make([]matrixSeries, 0, len(groupOrder))
	for _, gkey := range groupOrder {
		points := make([]matrixPoint, 0, len(windowKeys))
		for _, k := range windowKeys {
			g, ok := windowGroups[k][gkey]
			if !ok {
				continue // the group had no participating series in this window
			}
			value := g.value
			if e.aggregate == "avg" {
				value /= float64(g.count)
			}
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, false
			}
			points = append(points, matrixPoint{
				Timestamp: start.Add(time.Duration(k) * step).UTC().Format(time.RFC3339Nano),
				Value:     value,
			})
		}
		if len(points) > 0 {
			out = append(out, matrixSeries{Labels: groupLabels[gkey], Points: points})
		}
	}
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

		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			writeAPIError(w, "invalid_query_range", http.StatusBadRequest)
			return
		}

		// Exactly expr, start, end and step: four distinct parameter keys,
		// each present exactly once. A duplicate value shows up as a slice
		// longer than one; an unknown or missing key breaks the count.
		if len(values) != 4 {
			writeAPIError(w, "invalid_query_range", http.StatusBadRequest)
			return
		}
		for _, key := range []string{"expr", "start", "end", "step"} {
			if len(values[key]) != 1 {
				writeAPIError(w, "invalid_query_range", http.StatusBadRequest)
				return
			}
		}

		// Time parameters reuse the metric-range contract: timezoned
		// RFC3339Nano, a half-open [start,end) interval, 1..3600s integer
		// steps and at most 10000 windows.
		start, err := time.Parse(time.RFC3339Nano, values.Get("start"))
		if err != nil {
			writeAPIError(w, "invalid_query_range", http.StatusBadRequest)
			return
		}
		end, err := time.Parse(time.RFC3339Nano, values.Get("end"))
		if err != nil {
			writeAPIError(w, "invalid_query_range", http.StatusBadRequest)
			return
		}
		if !start.Before(end) {
			writeAPIError(w, "invalid_query_range", http.StatusBadRequest)
			return
		}
		step, ok := parseStep(values.Get("step"))
		if !ok {
			writeAPIError(w, "invalid_query_range", http.StatusBadRequest)
			return
		}
		span := end.Sub(start)
		windows := int64(span) / int64(step)
		if int64(span)%int64(step) != 0 {
			windows++
		}
		if windows > maxRangeWindows {
			writeAPIError(w, "invalid_query_range", http.StatusBadRequest)
			return
		}

		// The expression language is the one served by /api/v1/query.
		expr, ok := parseQueryExpr(values.Get("expr"))
		if !ok {
			writeAPIError(w, "invalid_expression", http.StatusBadRequest)
			return
		}

		result, finite := tenantStore(r).executeQueryRange(expr, start, end, step, time.Now())
		if !finite {
			writeAPIError(w, "invalid_query_range_data", http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, http.StatusOK, matrixResponse{ResultType: "matrix", Result: result})
	})
}
