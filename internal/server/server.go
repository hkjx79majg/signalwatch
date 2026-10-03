// Package server exposes the frozen public surface of SignalWatch.
//
// Besides the baseline process health check, the server accepts in-memory
// metric samples at /api/v1/metrics. All new responses use JSON. Metric state
// is process-local and is lost on restart.
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Version is the baseline release identifier.
const Version = "0.1.0"

const metricsPath = "/api/v1/metrics"

var identPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type health struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

// series is one uniquely identified time series: name + full label set.
type series struct {
	name   string
	typ    string
	labels map[string]string

	// counter/gauge state
	value float64

	// histogram state
	bucketBounds []float64 // strictly increasing, fixed at first write
	bucketCounts []int64   // cumulative counts per bound, len == len(bucketBounds)
	count        int64
	sum          float64
}

type metricStore struct {
	mu     sync.RWMutex
	series map[string]*series

	// alert rules keyed by rule id; guarded by mu so rule replacement and
	// alert evaluation are atomic with metric commits.
	rules map[string]*alertRule

	// silences keyed by silence id; same lock so alert evaluation reads
	// metrics, rules and silences from one consistent snapshot.
	silences map[string]*silence

	// inhibit rules keyed by rule id; same lock so alert evaluation also
	// reads inhibit rules from the same consistent snapshot.
	inhibitions map[string]*inhibitRule

	// notification routes keyed by route id; same lock so the notification
	// plan reads routes from the same snapshot as alert evaluation.
	notificationRoutes map[string]*notificationRoute

	// SLO definitions keyed by SLO id; same lock so status evaluation reads
	// definitions and counters from one consistent snapshot.
	slos map[string]*sloDefinition
}

func newMetricStore() *metricStore {
	return &metricStore{
		series:             make(map[string]*series),
		rules:              make(map[string]*alertRule),
		silences:           make(map[string]*silence),
		inhibitions:        make(map[string]*inhibitRule),
		notificationRoutes: make(map[string]*notificationRoute),
		slos:               make(map[string]*sloDefinition),
	}
}

// validatedSample is a sample that passed all format and value checks.
type validatedSample struct {
	key     string
	name    string
	typ     string
	labels  map[string]string
	value   float64
	buckets []float64 // non-nil for histograms
}

// checkAndApply validates the batch against the current state (and against
// itself) and then commits it in array order. A false result signals a type or
// bucket-boundary conflict; in that case no state is mutated.
func (s *metricStore) checkAndApply(batch []validatedSample) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// First pass: conflict detection against committed state and earlier
	// samples in the same batch.
	resolved := make(map[string]*series, len(batch))
	for _, sm := range batch {
		existing, ok := s.series[sm.key]
		if !ok {
			if shadow, seen := resolved[sm.key]; seen {
				existing, ok = shadow, true
			}
		}
		if ok {
			if existing.typ != sm.typ {
				return false
			}
			if sm.typ == "histogram" && !equalBuckets(existing.bucketBounds, sm.buckets) {
				return false
			}
			resolved[sm.key] = existing
			continue
		}
		resolved[sm.key] = &series{typ: sm.typ, bucketBounds: sm.buckets}
	}

	// Second pass: commit in order.
	for _, sm := range batch {
		cur, ok := s.series[sm.key]
		if !ok {
			cur = &series{
				name:         sm.name,
				typ:          sm.typ,
				labels:       sm.labels,
				bucketBounds: append([]float64(nil), sm.buckets...),
			}
			if sm.typ == "histogram" {
				cur.bucketCounts = make([]int64, len(sm.buckets))
			}
			s.series[sm.key] = cur
		}
		switch sm.typ {
		case "counter":
			cur.value += sm.value
		case "gauge":
			cur.value = sm.value
		case "histogram":
			cur.count++
			cur.sum += sm.value
			for i, bound := range cur.bucketBounds {
				if sm.value <= bound {
					cur.bucketCounts[i]++
				}
			}
		}
	}
	return true
}

type seriesOutput struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Labels  map[string]string `json:"labels"`
	Value   *float64          `json:"value,omitempty"`
	Count   *int64            `json:"count,omitempty"`
	Sum     *float64          `json:"sum,omitempty"`
	Buckets []bucketOutput    `json:"buckets,omitempty"`
}

type bucketOutput struct {
	LE    float64 `json:"le"`
	Count int64   `json:"count"`
}

// query returns matching series as ready-to-encode outputs. A series matches
// when its name equals name exactly and it contains every selector label.
func (s *metricStore) query(name string, selector map[string]string) []seriesOutput {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]seriesOutput, 0)
	for _, cur := range s.series {
		if cur.name != name {
			continue
		}
		matched := true
		for k, v := range selector {
			got, ok := cur.labels[k]
			if !ok || got != v {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		item := seriesOutput{
			Name:   cur.name,
			Type:   cur.typ,
			Labels: copyLabels(cur.labels),
		}
		switch cur.typ {
		case "counter", "gauge":
			v := cur.value
			item.Value = &v
		case "histogram":
			c, sum := cur.count, cur.sum
			item.Count = &c
			item.Sum = &sum
			item.Buckets = make([]bucketOutput, len(cur.bucketBounds))
			for i, bound := range cur.bucketBounds {
				item.Buckets[i] = bucketOutput{LE: bound, Count: cur.bucketCounts[i]}
			}
		}
		out = append(out, item)
	}

	sort.Slice(out, func(i, j int) bool {
		return compareLabels(out[i].Labels, out[j].Labels) < 0
	})
	return out
}

// compareLabels imposes a normalized lexicographic order on full label sets:
// keys are sorted, then (key, value) pairs are compared element-wise, and a
// shorter pair list sorts first when it is a prefix of a longer one.
func compareLabels(a, b map[string]string) int {
	ka := sortedKeys(a)
	kb := sortedKeys(b)
	n := len(ka)
	if len(kb) < n {
		n = len(kb)
	}
	for i := 0; i < n; i++ {
		if ka[i] != kb[i] {
			return strings.Compare(ka[i], kb[i])
		}
		if a[ka[i]] != b[kb[i]] {
			return strings.Compare(a[ka[i]], b[kb[i]])
		}
	}
	switch {
	case len(ka) < len(kb):
		return -1
	case len(ka) > len(kb):
		return 1
	default:
		return 0
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// seriesKey builds a collision-free identity from name + unordered labels.
// Name and label keys are restricted to [a-zA-Z0-9_] and cannot contain NUL,
// so length-prefixed values remove any ambiguity from arbitrary label values.
func seriesKey(name string, labels map[string]string) string {
	var b strings.Builder
	b.WriteString(name)
	for _, k := range sortedKeys(labels) {
		b.WriteByte(0)
		b.WriteString(k)
		b.WriteByte(0)
		v := labels[k]
		b.WriteString(strconv.Itoa(len(v)))
		b.WriteByte(0)
		b.WriteString(v)
	}
	return b.String()
}

func equalBuckets(a, b []float64) bool {
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

// Handler returns the HTTP surface served by SignalWatch.
func Handler() http.Handler {
	return newHandler(newTenantRegistry())
}

func newHandler(registry *tenantRegistry) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, `{"error":{"code":"method_not_allowed"}}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(health{Status: "ok", Service: "signalwatch", Version: Version})
	})

	mux.HandleFunc(metricsPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleMetricsGet(w, r)
		case http.MethodPost:
			handleMetricsPost(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(queryPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		handleQueryGet(w, r)
	})

	registerAlertHandlers(mux)
	registerSilenceHandlers(mux)
	registerInhibitHandlers(mux)
	registerNotificationRouteHandlers(mux)
	registerSLOHandlers(mux)

	return withTenants(mux, registry)
}

func handleMetricsPost(w http.ResponseWriter, r *http.Request) {
	store := tenantStore(r)
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, "invalid_metrics", http.StatusBadRequest)
		return
	}

	batch, ok := decodeMetricsBatch(body)
	if !ok {
		writeAPIError(w, "invalid_metrics", http.StatusBadRequest)
		return
	}

	if !store.checkAndApply(batch) {
		writeAPIError(w, "metric_conflict", http.StatusConflict)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]int{"accepted": len(batch)})
}

type metricsEnvelope struct {
	Samples *[]json.RawMessage `json:"samples"`
}

type rawSample struct {
	Name    *string            `json:"name"`
	Type    *string            `json:"type"`
	Labels  *map[string]string `json:"labels"`
	Value   *float64           `json:"value"`
	Buckets *[]float64         `json:"buckets"`
}

// decodeMetricsBatch performs every format and value check. It never returns a
// partially validated batch.
func decodeMetricsBatch(body []byte) ([]validatedSample, bool) {
	var env metricsEnvelope
	if !strictDecode(body, &env) || env.Samples == nil || len(*env.Samples) == 0 {
		return nil, false
	}

	batch := make([]validatedSample, 0, len(*env.Samples))
	for _, raw := range *env.Samples {
		var sm rawSample
		if !strictDecode(raw, &sm) {
			return nil, false
		}
		if sm.Name == nil || sm.Type == nil || sm.Labels == nil || sm.Value == nil {
			return nil, false
		}
		name, typ := *sm.Name, *sm.Type
		if !identPattern.MatchString(name) {
			return nil, false
		}
		if typ != "counter" && typ != "gauge" && typ != "histogram" {
			return nil, false
		}
		labels := *sm.Labels
		if labels == nil {
			return nil, false // explicit null labels
		}
		for k := range labels {
			if !identPattern.MatchString(k) {
				return nil, false
			}
		}
		value := *sm.Value
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, false // encoding/json already rejects NaN/Inf tokens
		}
		if typ == "counter" && value < 0 {
			return nil, false
		}

		var buckets []float64
		if typ == "histogram" {
			if sm.Buckets == nil || !validBuckets(*sm.Buckets) {
				return nil, false
			}
			buckets = append([]float64(nil), (*sm.Buckets)...)
		} else if sm.Buckets != nil {
			return nil, false // buckets only valid on histograms
		}

		batch = append(batch, validatedSample{
			key:     seriesKey(name, labels),
			name:    name,
			typ:     typ,
			labels:  copyLabels(labels),
			value:   value,
			buckets: buckets,
		})
	}
	return batch, true
}

func validBuckets(buckets []float64) bool {
	if len(buckets) == 0 {
		return false
	}
	prev := math.Inf(-1)
	for _, b := range buckets {
		if math.IsNaN(b) || math.IsInf(b, 0) || b <= prev {
			return false
		}
		prev = b
	}
	return true
}

// strictDecode decodes JSON, rejecting unknown fields, multiple/ trailing
// values and non-object payloads for struct targets.
func strictDecode(data []byte, target any) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return false
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return false
	}
	return true
}

func handleMetricsGet(w http.ResponseWriter, r *http.Request) {
	store := tenantStore(r)
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeAPIError(w, "invalid_query", http.StatusBadRequest)
		return
	}

	names := values["name"]
	if len(names) == 0 || !identPattern.MatchString(names[0]) {
		writeAPIError(w, "invalid_query", http.StatusBadRequest)
		return
	}

	selector := make(map[string]string)
	for key, vals := range values {
		if !strings.HasPrefix(key, "label.") {
			continue
		}
		labelKey := strings.TrimPrefix(key, "label.")
		if !identPattern.MatchString(labelKey) {
			writeAPIError(w, "invalid_query", http.StatusBadRequest)
			return
		}
		for _, v := range vals[1:] {
			if v != vals[0] {
				writeAPIError(w, "invalid_query", http.StatusBadRequest)
				return
			}
		}
		selector[labelKey] = vals[0]
	}

	seriesList := store.query(names[0], selector)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"series": seriesList})
}

func writeAPIError(w http.ResponseWriter, code string, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
}
