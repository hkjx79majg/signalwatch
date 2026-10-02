// Metrics ingestion and querying for SignalWatch.
//
// State lives only in memory; restarting the process clears every series.
// A series is identified by the metric name plus the full (unordered) label
// set. All handlers on /api/v1/metrics respond with application/json.
package server

import (
	"encoding/json"
	"io"
	"math"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	typeCounter   = "counter"
	typeGauge     = "gauge"
	typeHistogram = "histogram"
)

// identifierRe matches metric names and label keys.
var identifierRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// series holds the aggregate state of one metric series.
type series struct {
	name   string
	typ    string
	labels map[string]string

	value float64 // counter (accumulated) and gauge (last write)

	count        uint64    // histogram observation count
	sum          float64   // histogram observation sum
	buckets      []float64 // histogram upper bounds, strictly increasing
	bucketCounts []uint64  // per-boundary (non-cumulative) observation counts
}

// metricsStore is the in-memory registry of all series.
type metricsStore struct {
	mu     sync.Mutex
	series map[string]*series
}

func newMetricsStore() *metricsStore {
	return &metricsStore{series: make(map[string]*series)}
}

// seriesKey canonicalizes name + labels so that label order does not matter.
func seriesKey(name string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(strconv.Quote(name))
	for _, k := range keys {
		b.WriteByte(0)
		b.WriteString(strconv.Quote(k))
		b.WriteByte(0)
		b.WriteString(strconv.Quote(labels[k]))
	}
	return b.String()
}

// --- ingestion ---

type ingestBody struct {
	Samples []ingestSample `json:"samples"`
}

type ingestSample struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Labels  map[string]string `json:"labels"`
	Value   json.RawMessage   `json:"value"`
	Buckets []float64         `json:"buckets"`
}

// validSample is a fully validated sample ready to be applied.
type validSample struct {
	name    string
	typ     string
	labels  map[string]string
	value   float64
	buckets []float64
}

// parseFiniteNumber accepts only a JSON number literal that fits in a finite
// float64. Strings, booleans, null, arrays, objects, and overflowing
// exponents (e.g. 1e999) are rejected.
func parseFiniteNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return 0, false
	}
	num, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := num.Float64()
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// validateSample checks one sample's shape and values (the 400 rules).
func validateSample(s *ingestSample) (validSample, bool) {
	var out validSample
	if !identifierRe.MatchString(s.Name) {
		return out, false
	}
	if s.Type != typeCounter && s.Type != typeGauge && s.Type != typeHistogram {
		return out, false
	}
	labels := s.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	for k := range labels {
		if !identifierRe.MatchString(k) {
			return out, false
		}
	}
	value, ok := parseFiniteNumber(s.Value)
	if !ok {
		return out, false
	}
	switch s.Type {
	case typeCounter:
		if value < 0 {
			return out, false
		}
	case typeHistogram:
		if s.Buckets == nil {
			return out, false
		}
		for i, b := range s.Buckets {
			if math.IsInf(b, 0) || math.IsNaN(b) {
				return out, false
			}
			if i > 0 && b <= s.Buckets[i-1] {
				return out, false
			}
		}
	}
	return validSample{
		name:    s.Name,
		typ:     s.Type,
		labels:  labels,
		value:   value,
		buckets: s.Buckets,
	}, true
}

func equalBoundaries(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// apply commits a validated batch atomically. It reports whether the batch
// conflicts with existing (or intra-batch) series type or histogram
// boundaries; on conflict nothing is mutated.
func (st *metricsStore) apply(batch []validSample) (conflict bool) {
	st.mu.Lock()
	defer st.mu.Unlock()

	type proposed struct {
		typ     string
		buckets []float64
	}
	seen := make(map[string]proposed, len(batch))
	for _, s := range batch {
		key := seriesKey(s.name, s.labels)
		typ, buckets := "", []float64(nil)
		if existing, ok := st.series[key]; ok {
			typ, buckets = existing.typ, existing.buckets
		}
		if p, ok := seen[key]; ok {
			typ, buckets = p.typ, p.buckets
		}
		if typ != "" {
			if typ != s.typ {
				return true
			}
			if s.typ == typeHistogram && !equalBoundaries(buckets, s.buckets) {
				return true
			}
		}
		seen[key] = proposed{typ: s.typ, buckets: s.buckets}
	}

	for _, s := range batch {
		key := seriesKey(s.name, s.labels)
		sr, ok := st.series[key]
		if !ok {
			sr = &series{name: s.name, typ: s.typ, labels: s.labels}
			if s.typ == typeHistogram {
				sr.buckets = s.buckets
				sr.bucketCounts = make([]uint64, len(s.buckets))
			}
			st.series[key] = sr
		}
		switch s.typ {
		case typeCounter:
			sr.value += s.value
		case typeGauge:
			sr.value = s.value
		case typeHistogram:
			sr.count++
			sr.sum += s.value
			idx := sort.Search(len(sr.buckets), func(i int) bool {
				return s.value <= sr.buckets[i]
			})
			if idx < len(sr.buckets) {
				sr.bucketCounts[idx]++
			}
		}
	}
	return false
}

// --- query ---

type bucketOut struct {
	Le    float64 `json:"le"`
	Count uint64  `json:"count"`
}

type seriesOut struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Labels  map[string]string `json:"labels"`
	Value   *float64          `json:"value,omitempty"`
	Count   *uint64           `json:"count,omitempty"`
	Sum     *float64          `json:"sum,omitempty"`
	Buckets []bucketOut       `json:"buckets,omitempty"`

	pairs []labelPair // canonical label ordering, used for sorting
}

type labelPair struct {
	key, value string
}

func canonicalPairs(labels map[string]string) []labelPair {
	pairs := make([]labelPair, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, labelPair{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key != pairs[j].key {
			return pairs[i].key < pairs[j].key
		}
		return pairs[i].value < pairs[j].value
	})
	return pairs
}

// lessPairs orders series by the normalized lexicographic order of their full
// label key/value pairs.
func lessPairs(a, b []labelPair) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i].key != b[i].key {
			return a[i].key < b[i].key
		}
		if a[i].value != b[i].value {
			return a[i].value < b[i].value
		}
	}
	return len(a) < len(b)
}

// query returns the series matching an exact name and label selectors.
func (st *metricsStore) query(name string, selectors map[string]string) []seriesOut {
	st.mu.Lock()
	defer st.mu.Unlock()

	out := make([]seriesOut, 0)
	for _, sr := range st.series {
		if sr.name != name {
			continue
		}
		match := true
		for k, v := range selectors {
			if lv, ok := sr.labels[k]; !ok || lv != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		row := seriesOut{
			Name:   sr.name,
			Type:   sr.typ,
			Labels: sr.labels,
			pairs:  canonicalPairs(sr.labels),
		}
		if row.Labels == nil {
			row.Labels = map[string]string{}
		}
		switch sr.typ {
		case typeCounter, typeGauge:
			v := sr.value
			row.Value = &v
		case typeHistogram:
			count, sum := sr.count, sr.sum
			row.Count, row.Sum = &count, &sum
			row.Buckets = make([]bucketOut, len(sr.buckets))
			var cumulative uint64
			for i, le := range sr.buckets {
				cumulative += sr.bucketCounts[i]
				row.Buckets[i] = bucketOut{Le: le, Count: cumulative}
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return lessPairs(out[i].pairs, out[j].pairs) })
	return out
}

// --- HTTP surface ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}

func (st *metricsStore) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		st.handlePost(w, r)
	case http.MethodGet:
		st.handleGet(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

func (st *metricsStore) handlePost(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}

	var body ingestBody
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_metrics")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_metrics")
		return
	}
	if len(body.Samples) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_metrics")
		return
	}

	batch := make([]validSample, 0, len(body.Samples))
	for i := range body.Samples {
		vs, ok := validateSample(&body.Samples[i])
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_metrics")
			return
		}
		batch = append(batch, vs)
	}

	if st.apply(batch) {
		writeError(w, http.StatusConflict, "metric_conflict")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": len(batch)})
}

func (st *metricsStore) handleGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	names := map[string]struct{}{}
	for _, n := range q["name"] {
		names[n] = struct{}{}
	}
	if len(names) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_query")
		return
	}
	var name string
	for n := range names {
		name = n
	}
	if !identifierRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_query")
		return
	}

	selectors := map[string]string{}
	for key, values := range q {
		if !strings.HasPrefix(key, "label.") {
			continue
		}
		labelKey := key[len("label."):]
		if !identifierRe.MatchString(labelKey) {
			writeError(w, http.StatusBadRequest, "invalid_query")
			return
		}
		distinct := map[string]struct{}{}
		for _, v := range values {
			distinct[v] = struct{}{}
		}
		if len(distinct) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_query")
			return
		}
		selectors[labelKey] = values[0]
	}

	writeJSON(w, http.StatusOK, map[string]any{"series": st.query(name, selectors)})
}
