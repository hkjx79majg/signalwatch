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
	// metricRetention bounds how far back accepted samples stay queryable:
	// writes older than this relative to the receive instant are rejected,
	// and range queries trim records older than this relative to now.
	metricRetention = 24 * time.Hour
	// metricMaxFutureSkew bounds how far into the future an explicit sample
	// timestamp may lie relative to the receive instant.
	metricMaxFutureSkew = 5 * time.Minute

	maxRangeStepSec = 3600
	maxRangeWindows = 10000
)

// samplePoint is one accepted metric sample in a series' history. seq is the
// store-wide commit sequence and breaks ties between samples that share one
// timestamp: the later commit wins.
type samplePoint struct {
	timestamp time.Time
	value     float64
	seq       int64
}

// ---- wire shapes -------------------------------------------------------------

type rangePoint struct {
	Timestamp string         `json:"timestamp"`
	Value     *float64       `json:"value,omitempty"`
	Count     *int64         `json:"count,omitempty"`
	Sum       *float64       `json:"sum,omitempty"`
	Buckets   []bucketOutput `json:"buckets,omitempty"`
}

type rangeSeries struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels"`
	Points []rangePoint      `json:"points"`
}

// ---- query parameters --------------------------------------------------------

// rangeParams is the validated parameter set of GET /api/v1/metric-range.
type rangeParams struct {
	name     string
	selector map[string]string
	start    time.Time
	end      time.Time
	step     time.Duration
}

// parseRangeQuery validates the exact parameter vocabulary: name, start, end
// and step exactly once each, plus optional non-repeated label.<key> selectors.
// Any missing, duplicated or unknown parameter, malformed identifier, invalid
// time interval or step, or a window count above the bound is rejected.
func parseRangeQuery(values url.Values) (*rangeParams, bool) {
	p := &rangeParams{selector: map[string]string{}}
	var haveName, haveStart, haveEnd, haveStep bool
	var stepSec int

	for key, vals := range values {
		if len(vals) != 1 {
			return nil, false // missing value or repeated parameter
		}
		value := vals[0]
		switch {
		case key == "name":
			if haveName || !identPattern.MatchString(value) {
				return nil, false
			}
			haveName, p.name = true, value
		case key == "start":
			if haveStart {
				return nil, false
			}
			ts, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return nil, false
			}
			haveStart, p.start = true, ts
		case key == "end":
			if haveEnd {
				return nil, false
			}
			ts, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return nil, false
			}
			haveEnd, p.end = true, ts
		case key == "step":
			if haveStep || !allDigits(value) {
				return nil, false
			}
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > maxRangeStepSec {
				return nil, false
			}
			haveStep, stepSec = true, n
		case strings.HasPrefix(key, "label."):
			labelKey := strings.TrimPrefix(key, "label.")
			if !identPattern.MatchString(labelKey) {
				return nil, false
			}
			if _, dup := p.selector[labelKey]; dup {
				return nil, false
			}
			p.selector[labelKey] = value
		default:
			return nil, false
		}
	}
	if !haveName || !haveStart || !haveEnd || !haveStep {
		return nil, false
	}
	if !p.start.Before(p.end) {
		return nil, false
	}
	p.step = time.Duration(stepSec) * time.Second

	// Half-open windows tile [start, end) from start; the last one may be
	// clipped by end. The total count must stay within the bound.
	span := p.end.Sub(p.start)
	windows := span / p.step
	if span%p.step != 0 {
		windows++
	}
	if windows > maxRangeWindows {
		return nil, false
	}
	return p, true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ---- execution ---------------------------------------------------------------

// queryMetricRange aggregates each matching series' retained history into
// step-sized windows over [start, end). Records older than the retention
// window relative to now are trimmed first. The boolean is false when any
// aggregation produced a non-finite number; no partial result is returned.
func (s *metricStore) queryMetricRange(p *rangeParams, now time.Time) ([]rangeSeries, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := now.Add(-metricRetention)
	out := make([]rangeSeries, 0)
	for _, cur := range s.series {
		if cur.name != p.name {
			continue
		}
		matched := true
		for k, v := range p.selector {
			got, ok := cur.labels[k]
			if !ok || got != v {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}

		// Accumulate retained samples into their window, keyed by window index.
		type windowAcc struct {
			sum          float64 // counter increment sum / histogram observation sum
			count        int64   // histogram observation count
			bucketCounts []int64 // histogram cumulative counts per bound
			gauge        samplePoint
			hasGauge     bool
		}
		windows := make(map[int64]*windowAcc)
		for _, pt := range cur.history {
			if pt.timestamp.Before(cutoff) {
				continue
			}
			if pt.timestamp.Before(p.start) || !pt.timestamp.Before(p.end) {
				continue
			}
			idx := int64(pt.timestamp.Sub(p.start) / p.step)
			acc, ok := windows[idx]
			if !ok {
				acc = &windowAcc{}
				if cur.typ == "histogram" {
					acc.bucketCounts = make([]int64, len(cur.bucketBounds))
				}
				windows[idx] = acc
			}
			switch cur.typ {
			case "counter":
				acc.sum += pt.value
			case "gauge":
				if !acc.hasGauge || pt.timestamp.After(acc.gauge.timestamp) ||
					(pt.timestamp.Equal(acc.gauge.timestamp) && pt.seq > acc.gauge.seq) {
					acc.gauge, acc.hasGauge = pt, true
				}
			case "histogram":
				acc.count++
				acc.sum += pt.value
				for i, bound := range cur.bucketBounds {
					if pt.value <= bound {
						acc.bucketCounts[i]++
					}
				}
			}
		}
		if len(windows) == 0 {
			continue // series without points is not returned
		}

		idxs := make([]int64, 0, len(windows))
		for idx := range windows {
			idxs = append(idxs, idx)
		}
		sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })

		rs := rangeSeries{
			Name:   cur.name,
			Type:   cur.typ,
			Labels: copyLabels(cur.labels),
			Points: make([]rangePoint, 0, len(idxs)),
		}
		for _, idx := range idxs {
			acc := windows[idx]
			point := rangePoint{
				Timestamp: p.start.Add(time.Duration(idx) * p.step).UTC().Format(time.RFC3339Nano),
			}
			switch cur.typ {
			case "counter":
				if math.IsNaN(acc.sum) || math.IsInf(acc.sum, 0) {
					return nil, false
				}
				v := acc.sum
				point.Value = &v
			case "gauge":
				v := acc.gauge.value
				point.Value = &v
			case "histogram":
				if math.IsNaN(acc.sum) || math.IsInf(acc.sum, 0) {
					return nil, false
				}
				c, sum := acc.count, acc.sum
				point.Count = &c
				point.Sum = &sum
				point.Buckets = make([]bucketOutput, len(cur.bucketBounds))
				for i, bound := range cur.bucketBounds {
					point.Buckets[i] = bucketOutput{LE: bound, Count: acc.bucketCounts[i]}
				}
			}
			rs.Points = append(rs.Points, point)
		}
		out = append(out, rs)
	}

	sort.Slice(out, func(i, j int) bool {
		return compareLabels(out[i].Labels, out[j].Labels) < 0
	})
	return out, true
}

// ---- HTTP handler ------------------------------------------------------------

func registerMetricRangeHandler(mux *http.ServeMux) {
	mux.HandleFunc(metricRangePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}

		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			writeAPIError(w, "invalid_metric_range", http.StatusBadRequest)
			return
		}
		params, ok := parseRangeQuery(values)
		if !ok {
			writeAPIError(w, "invalid_metric_range", http.StatusBadRequest)
			return
		}

		seriesList, finite := tenantStore(r).queryMetricRange(params, time.Now())
		if !finite {
			writeAPIError(w, "invalid_metric_range_data", http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"series": seriesList})
	})
}
