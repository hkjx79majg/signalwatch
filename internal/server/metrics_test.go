package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func postMetrics(t *testing.T, h http.Handler, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/metrics", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getMetrics(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	return payload
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	payload := decodeBody(t, rec)
	errObj, ok := payload["error"].(map[string]any)
	if !ok || errObj["code"] != code {
		t.Fatalf("error payload = %v, want code %q", payload, code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

func TestPostCounterAccumulates(t *testing.T) {
	h := Handler()
	rec := postMetrics(t, h, "application/json",
		`{"samples":[{"name":"requests_total","type":"counter","labels":{"job":"api"},"value":2}]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["accepted"]; got != float64(1) {
		t.Fatalf("accepted = %v, want 1", got)
	}
	rec = postMetrics(t, h, "application/json; charset=utf-8",
		`{"samples":[{"name":"requests_total","type":"counter","labels":{"job":"api"},"value":3}]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	rec = getMetrics(t, h, "/api/v1/metrics?name=requests_total")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	payload := decodeBody(t, rec)
	series := payload["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("series len = %d, want 1", len(series))
	}
	s := series[0].(map[string]any)
	if s["value"] != float64(5) || s["type"] != "counter" || s["name"] != "requests_total" {
		t.Fatalf("unexpected series: %v", s)
	}
	if s["labels"].(map[string]any)["job"] != "api" {
		t.Fatalf("unexpected labels: %v", s["labels"])
	}
}

func TestPostGaugeOverwritesAndLabelOrderIrrelevant(t *testing.T) {
	h := Handler()
	postMetrics(t, h, "application/json",
		`{"samples":[{"name":"temp","type":"gauge","labels":{"a":"1","b":"2"},"value":10}]}`)
	postMetrics(t, h, "application/json",
		`{"samples":[{"name":"temp","type":"gauge","labels":{"b":"2","a":"1"},"value":4}]}`)
	rec := getMetrics(t, h, "/api/v1/metrics?name=temp")
	series := decodeBody(t, rec)["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("series len = %d, want 1 (label order must not create new series)", len(series))
	}
	if got := series[0].(map[string]any)["value"]; got != float64(4) {
		t.Fatalf("gauge value = %v, want 4 (overwrite)", got)
	}
}

func TestPostHistogramAggregates(t *testing.T) {
	h := Handler()
	body := `{"samples":[
		{"name":"latency","type":"histogram","labels":{},"value":0.5,"buckets":[1,5,10]},
		{"name":"latency","type":"histogram","labels":{},"value":7,"buckets":[1,5,10]},
		{"name":"latency","type":"histogram","labels":{},"value":99,"buckets":[1,5,10]}
	]}`
	if rec := postMetrics(t, h, "application/json", body); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	rec := getMetrics(t, h, "/api/v1/metrics?name=latency")
	series := decodeBody(t, rec)["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("series len = %d, want 1", len(series))
	}
	s := series[0].(map[string]any)
	if s["count"] != float64(3) {
		t.Fatalf("count = %v, want 3 (observation above max bucket still counts)", s["count"])
	}
	if s["sum"] != 106.5 {
		t.Fatalf("sum = %v, want 106.5", s["sum"])
	}
	buckets := s["buckets"].([]any)
	want := []struct {
		le    float64
		count float64
	}{{1, 1}, {5, 1}, {10, 2}}
	if len(buckets) != len(want) {
		t.Fatalf("buckets = %v", buckets)
	}
	for i, b := range buckets {
		bm := b.(map[string]any)
		if bm["le"] != want[i].le || bm["count"] != want[i].count {
			t.Fatalf("bucket %d = %v, want le=%v count=%v", i, bm, want[i].le, want[i].count)
		}
	}
}

func TestPostRejectsInvalidSamples(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"empty samples":       `{"samples":[]}`,
		"missing samples":     `{}`,
		"not json":            `nope`,
		"trailing data":       `{"samples":[]} {}`,
		"bad name":            `{"samples":[{"name":"0bad","type":"gauge","labels":{},"value":1}]}`,
		"bad type":            `{"samples":[{"name":"m","type":"summary","labels":{},"value":1}]}`,
		"bad label key":       `{"samples":[{"name":"m","type":"gauge","labels":{"0k":"v"},"value":1}]}`,
		"missing value":       `{"samples":[{"name":"m","type":"gauge","labels":{}}]}`,
		"string value":        `{"samples":[{"name":"m","type":"gauge","labels":{},"value":"5"}]}`,
		"overflow value":      `{"samples":[{"name":"m","type":"gauge","labels":{},"value":1e999}]}`,
		"negative counter":    `{"samples":[{"name":"m","type":"counter","labels":{},"value":-1}]}`,
		"histogram no bucket": `{"samples":[{"name":"m","type":"histogram","labels":{},"value":1}]}`,
		"unsorted buckets":    `{"samples":[{"name":"m","type":"histogram","labels":{},"value":1,"buckets":[5,1]}]}`,
		"duplicate buckets":   `{"samples":[{"name":"m","type":"histogram","labels":{},"value":1,"buckets":[1,1]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := postMetrics(t, h, "application/json", body)
			wantError(t, rec, http.StatusBadRequest, "invalid_metrics")
		})
	}
}

func TestPostConflictAndAtomicity(t *testing.T) {
	h := Handler()
	postMetrics(t, h, "application/json",
		`{"samples":[{"name":"m","type":"counter","labels":{"a":"1"},"value":5}]}`)

	// Type conflict on the same series.
	rec := postMetrics(t, h, "application/json",
		`{"samples":[{"name":"m","type":"gauge","labels":{"a":"1"},"value":1}]}`)
	wantError(t, rec, http.StatusConflict, "metric_conflict")

	// Histogram boundary conflict.
	postMetrics(t, h, "application/json",
		`{"samples":[{"name":"h","type":"histogram","labels":{},"value":1,"buckets":[1,2]}]}`)
	rec = postMetrics(t, h, "application/json",
		`{"samples":[{"name":"h","type":"histogram","labels":{},"value":1,"buckets":[1,3]}]}`)
	wantError(t, rec, http.StatusConflict, "metric_conflict")

	// A failing batch must not mutate state, including its valid samples.
	rec = postMetrics(t, h, "application/json",
		`{"samples":[{"name":"m","type":"counter","labels":{"a":"1"},"value":10},{"name":"m","type":"gauge","labels":{"a":"1"},"value":1}]}`)
	wantError(t, rec, http.StatusConflict, "metric_conflict")
	series := decodeBody(t, getMetrics(t, h, "/api/v1/metrics?name=m"))["series"].([]any)
	if got := series[0].(map[string]any)["value"]; got != float64(5) {
		t.Fatalf("counter = %v after failed batch, want 5 (atomic)", got)
	}

	// Intra-batch accumulation of the same series works.
	rec = postMetrics(t, h, "application/json",
		`{"samples":[{"name":"m","type":"counter","labels":{"a":"1"},"value":1},{"name":"m","type":"counter","labels":{"a":"1"},"value":2}]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	series = decodeBody(t, getMetrics(t, h, "/api/v1/metrics?name=m"))["series"].([]any)
	if got := series[0].(map[string]any)["value"]; got != float64(8) {
		t.Fatalf("counter = %v, want 8", got)
	}
}

func TestPostUnsupportedMediaType(t *testing.T) {
	h := Handler()
	for _, ct := range []string{"", "text/plain", "application/xml"} {
		rec := postMetrics(t, h, ct, `{"samples":[]}`)
		wantError(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")
	}
}

func TestMetricsMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/v1/metrics", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, POST" {
			t.Fatalf("%s: Allow = %q, want %q", method, allow, "GET, POST")
		}
	}
}

func TestGetFiltersAndSorting(t *testing.T) {
	h := Handler()
	postMetrics(t, h, "application/json", `{"samples":[
		{"name":"up","type":"gauge","labels":{"job":"b","inst":"2"},"value":1},
		{"name":"up","type":"gauge","labels":{"job":"a"},"value":1},
		{"name":"up","type":"gauge","labels":{"job":"b","inst":"1"},"value":1},
		{"name":"up","type":"gauge","labels":{},"value":1},
		{"name":"other","type":"gauge","labels":{"job":"a"},"value":1}
	]}`)

	// Exact name match: "other" excluded; sorted by canonical label pairs.
	rec := getMetrics(t, h, "/api/v1/metrics?name=up")
	series := decodeBody(t, rec)["series"].([]any)
	if len(series) != 4 {
		t.Fatalf("series len = %d, want 4", len(series))
	}
	var got []string
	for _, s := range series {
		labels := s.(map[string]any)["labels"].(map[string]any)
		got = append(got, fmt.Sprintf("job=%v,inst=%v", labels["job"], labels["inst"]))
	}
	// Canonical pair sequences are sorted by key first, so "inst=..." pairs
	// order before "job=..." pairs.
	want := []string{"job=<nil>,inst=<nil>", "job=b,inst=1", "job=b,inst=2", "job=a,inst=<nil>"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}

	// Label selector filters to series containing the label.
	rec = getMetrics(t, h, "/api/v1/metrics?name=up&label.job=b")
	series = decodeBody(t, rec)["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("filtered series len = %d, want 2", len(series))
	}

	// No match returns an empty array, not an error.
	rec = getMetrics(t, h, "/api/v1/metrics?name=missing")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"series":[]}` {
		t.Fatalf("body = %q, want %q", body, `{"series":[]}`)
	}
}

func TestGetInvalidQueries(t *testing.T) {
	h := Handler()
	cases := []string{
		"/api/v1/metrics",                            // missing name
		"/api/v1/metrics?name=",                      // empty name
		"/api/v1/metrics?name=0bad",                  // illegal name
		"/api/v1/metrics?name=a&name=b",              // conflicting names
		"/api/v1/metrics?name=m&label.0k=v",          // illegal selector key
		"/api/v1/metrics?name=m&label.=v",            // empty selector key
		"/api/v1/metrics?name=m&label.a=1&label.a=2", // conflicting label values
	}
	for _, target := range cases {
		rec := getMetrics(t, h, target)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", target, rec.Code)
		}
		wantError(t, rec, http.StatusBadRequest, "invalid_query")
	}
}

func TestPostConcurrentCounterIncrements(t *testing.T) {
	h := Handler()
	const workers = 8
	const perWorker = 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				rec := postMetrics(t, h, "application/json",
					`{"samples":[{"name":"c","type":"counter","labels":{},"value":1}]}`)
				if rec.Code != http.StatusAccepted {
					t.Errorf("status = %d, want 202", rec.Code)
					return
				}
			}
		}()
	}
	wg.Wait()
	series := decodeBody(t, getMetrics(t, h, "/api/v1/metrics?name=c"))["series"].([]any)
	if got := series[0].(map[string]any)["value"]; got != float64(workers*perWorker) {
		t.Fatalf("counter = %v, want %d (no lost updates)", got, workers*perWorker)
	}
}

func TestHealthzUnchanged(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("health payload is not JSON: %v", err)
	}
	if payload["status"] != "ok" || payload["service"] != "signalwatch" || payload["version"] != Version {
		t.Fatalf("unexpected payload: %v", payload)
	}
}
