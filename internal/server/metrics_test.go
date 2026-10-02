package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func postMetrics(t *testing.T, h http.Handler, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	if contentType == "" {
		contentType = "application/json"
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, metricsPath, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
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
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%d): %v body=%q", rec.Code, err, rec.Body.String())
	}
	return out
}

func expectErrorCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d, body=%q", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	payload := decodeBody(t, rec)
	errObj, ok := payload["error"].(map[string]any)
	if !ok || errObj["code"] != code {
		t.Fatalf("error = %v, want code %q", payload["error"], code)
	}
}

func TestPostCounterAccumulatesAndGaugeOverwrites(t *testing.T) {
	h := Handler()

	rec := postMetrics(t, h, `{"samples":[{"name":"hits","type":"counter","labels":{"route":"/a"},"value":3}]}`, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["accepted"]; got != float64(1) {
		t.Fatalf("accepted = %v, want 1", got)
	}

	rec = postMetrics(t, h, `{"samples":[
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":2.5},
		{"name":"temp","type":"gauge","labels":{},"value":10},
		{"name":"temp","type":"gauge","labels":{},"value":7}
	]}`, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["accepted"]; got != float64(3) {
		t.Fatalf("accepted = %v, want 3", got)
	}

	rec = getMetrics(t, h, metricsPath+"?name=hits&label.route=/a")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	series := decodeBody(t, rec)["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("series len = %d, want 1", len(series))
	}
	s := series[0].(map[string]any)
	if s["name"] != "hits" || s["type"] != "counter" || s["value"] != 5.5 {
		t.Fatalf("unexpected counter series: %v", s)
	}

	rec = getMetrics(t, h, metricsPath+"?name=temp")
	s = decodeBody(t, rec)["series"].([]any)[0].(map[string]any)
	if s["value"] != float64(7) {
		t.Fatalf("gauge value = %v, want 7", s["value"])
	}
}

func TestPostHistogramBuckets(t *testing.T) {
	h := Handler()
	body := `{"samples":[
		{"name":"lat","type":"histogram","labels":{"route":"r"},"value":1,"buckets":[1,5,10]},
		{"name":"lat","type":"histogram","labels":{"route":"r"},"value":6,"buckets":[1,5,10]},
		{"name":"lat","type":"histogram","labels":{"route":"r"},"value":99,"buckets":[1,5,10]}
	]}`
	rec := postMetrics(t, h, body, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}

	rec = getMetrics(t, h, metricsPath+"?name=lat")
	s := decodeBody(t, rec)["series"].([]any)[0].(map[string]any)
	if s["count"] != float64(3) || s["sum"] != float64(106) {
		t.Fatalf("histogram aggregate = %v", s)
	}
	buckets := s["buckets"].([]any)
	want := []map[string]float64{
		{"le": 1, "count": 1},
		{"le": 5, "count": 1},
		{"le": 10, "count": 2},
	}
	for i, w := range want {
		got := buckets[i].(map[string]any)
		if got["le"] != w["le"] || got["count"] != float64(w["count"]) {
			t.Fatalf("bucket %d = %v, want %v", i, got, w)
		}
	}
}

func TestPostInvalidMetrics(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"not json":              `{`,
		"missing samples":       `{}`,
		"empty samples":         `{"samples":[]}`,
		"sample missing name":   `{"samples":[{"type":"gauge","labels":{},"value":1}]}`,
		"sample missing labels": `{"samples":[{"name":"m","type":"gauge","value":1}]}`,
		"sample missing value":  `{"samples":[{"name":"m","type":"gauge","labels":{}}]}`,
		"sample missing type":   `{"samples":[{"name":"m","labels":{},"value":1}]}`,
		"bad name":              `{"samples":[{"name":"1m","type":"gauge","labels":{},"value":1}]}`,
		"bad label key":         `{"samples":[{"name":"m","type":"gauge","labels":{"a-b":"1"},"value":1}]}`,
		"unknown type":          `{"samples":[{"name":"m","type":"summary","labels":{},"value":1}]}`,
		"negative counter":      `{"samples":[{"name":"m","type":"counter","labels":{},"value":-1}]}`,
		"string value":          `{"samples":[{"name":"m","type":"gauge","labels":{},"value":"1"}]}`,
		"null labels":           `{"samples":[{"name":"m","type":"gauge","labels":null,"value":1}]}`,
		"unknown field":         `{"samples":[],"extra":1}`,
		"hist missing buckets":  `{"samples":[{"name":"m","type":"histogram","labels":{},"value":1}]}`,
		"hist empty buckets":    `{"samples":[{"name":"m","type":"histogram","labels":{},"value":1,"buckets":[]}]}`,
		"hist unsorted buckets": `{"samples":[{"name":"m","type":"histogram","labels":{},"value":1,"buckets":[2,1]}]}`,
		"hist equal buckets":    `{"samples":[{"name":"m","type":"histogram","labels":{},"value":1,"buckets":[1,1]}]}`,
		"buckets on counter":    `{"samples":[{"name":"m","type":"counter","labels":{},"value":1,"buckets":[1]}]}`,
		"two json values":       `{"samples":[]}{}`,
		"samples not array":     `{"samples":{}}`,
		"envelope not object":   `[1,2]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			expectErrorCode(t, postMetrics(t, h, body, ""), http.StatusBadRequest, "invalid_metrics")
		})
	}
}

func TestPostConflictIsAtomic(t *testing.T) {
	h := Handler()

	// Establish a counter series.
	rec := postMetrics(t, h, `{"samples":[{"name":"m","type":"counter","labels":{"k":"v"},"value":1}]}`, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("setup status = %d", rec.Code)
	}

	// Same series, conflicting type -> 409 and no state change.
	rec = postMetrics(t, h, `{"samples":[
		{"name":"other","type":"gauge","labels":{},"value":1},
		{"name":"m","type":"gauge","labels":{"k":"v"},"value":9}
	]}`, "")
	expectErrorCode(t, rec, http.StatusConflict, "metric_conflict")

	rec = getMetrics(t, h, metricsPath+"?name=other")
	if len(decodeBody(t, rec)["series"].([]any)) != 0 {
		t.Fatal("conflicting batch must not create series")
	}
	rec = getMetrics(t, h, metricsPath+"?name=m")
	s := decodeBody(t, rec)["series"].([]any)[0].(map[string]any)
	if s["type"] != "counter" || s["value"] != float64(1) {
		t.Fatalf("state changed by failed batch: %v", s)
	}

	// Bucket-boundary conflict, also within a single batch.
	rec = postMetrics(t, h, `{"samples":[{"name":"h","type":"histogram","labels":{},"value":1,"buckets":[1,2]}]}`, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("setup status = %d", rec.Code)
	}
	rec = postMetrics(t, h, `{"samples":[{"name":"h","type":"histogram","labels":{},"value":1,"buckets":[1,3]}]}`, "")
	expectErrorCode(t, rec, http.StatusConflict, "metric_conflict")
	rec = postMetrics(t, h, `{"samples":[
		{"name":"h2","type":"histogram","labels":{},"value":1,"buckets":[1,2]},
		{"name":"h2","type":"histogram","labels":{},"value":1,"buckets":[1,3]}
	]}`, "")
	expectErrorCode(t, rec, http.StatusConflict, "metric_conflict")
}

func TestPostStatusCodesAndAllow(t *testing.T) {
	h := Handler()

	rec := postMetrics(t, h, `{"samples":[]}`, "text/plain")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")

	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, metricsPath, nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != "GET, POST" {
			t.Fatalf("%s Allow = %q, want %q", method, got, "GET, POST")
		}
	}
}

func TestGetQueryValidationAndMatching(t *testing.T) {
	h := Handler()
	seed := `{"samples":[
		{"name":"m","type":"gauge","labels":{"a":"1","b":"2"},"value":1},
		{"name":"m","type":"gauge","labels":{"a":"1","b":"3"},"value":2},
		{"name":"m","type":"gauge","labels":{"a":"9"},"value":3},
		{"name":"n","type":"gauge","labels":{},"value":4}
	]}`
	if rec := postMetrics(t, h, seed, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("seed status = %d", rec.Code)
	}

	// No match is still 200 with an empty (non-null) array.
	rec := getMetrics(t, h, metricsPath+"?name=zzz")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"series":[]}` {
		t.Fatalf("no-match response = %d %q", rec.Code, rec.Body.String())
	}

	// Selector filters to series containing the labels.
	rec = getMetrics(t, h, metricsPath+"?name=m&label.a=1")
	series := decodeBody(t, rec)["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("filtered len = %d, want 2", len(series))
	}
	// Sorted by normalized full label set: keys [a,b] then b value 2 before 3.
	first := series[0].(map[string]any)
	if first["labels"].(map[string]any)["b"] != "2" {
		t.Fatalf("series order = %v", series)
	}

	// Series without b sorts after series with an extra pair? Here verify full
	// ordering explicitly for a=9 (single label) vs a=1,b=2/3.
	rec = getMetrics(t, h, metricsPath+"?name=m")
	series = decodeBody(t, rec)["series"].([]any)
	gotOrder := make([]string, len(series))
	for i, item := range series {
		l := item.(map[string]any)["labels"].(map[string]any)
		gotOrder[i] = fmt.Sprintf("%v", l)
	}
	if gotOrder[0] != "map[a:1 b:2]" || gotOrder[1] != "map[a:1 b:3]" || gotOrder[2] != "map[a:9]" {
		t.Fatalf("series order = %v", gotOrder)
	}

	// Invalid queries.
	for _, target := range []string{
		metricsPath,
		metricsPath + "?name=",
		metricsPath + "?name=1bad",
		metricsPath + "?name=m&label.=x",
		metricsPath + "?name=m&label.a-b=x",
		metricsPath + "?name=m&label.a=1&label.a=2",
		metricsPath + "?name=" + "%zz",
	} {
		rec := getMetrics(t, h, target)
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_query")
	}
}

func TestSeriesKeyLabelOrderDoesNotMatter(t *testing.T) {
	h := Handler()
	r1 := postMetrics(t, h, `{"samples":[{"name":"m","type":"counter","labels":{"a":"1","b":"2"},"value":1}]}`, "")
	r2 := postMetrics(t, h, `{"samples":[{"name":"m","type":"counter","labels":{"b":"2","a":"1"},"value":1}]}`, "")
	if r1.Code != http.StatusAccepted || r2.Code != http.StatusAccepted {
		t.Fatalf("status = %d/%d", r1.Code, r2.Code)
	}
	rec := getMetrics(t, h, metricsPath+"?name=m")
	if len(decodeBody(t, rec)["series"].([]any)) != 1 {
		t.Fatal("label order should not create separate series")
	}
}

func TestConcurrentCountersDoNotLoseUpdates(t *testing.T) {
	h := Handler()
	const goroutines = 32
	const perG = 100
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, metricsPath,
					bytes.NewReader([]byte(`{"samples":[{"name":"c","type":"counter","labels":{},"value":1}]}`)))
				req.Header.Set("Content-Type", "application/json")
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusAccepted {
					t.Errorf("status = %d", rec.Code)
					return
				}
			}
		}()
	}
	wg.Wait()
	rec := getMetrics(t, h, metricsPath+"?name=c")
	s := decodeBody(t, rec)["series"].([]any)[0].(map[string]any)
	if s["value"] != float64(goroutines*perG) {
		t.Fatalf("counter value = %v, want %d", s["value"], goroutines*perG)
	}
}

func TestUnknownPathStill404(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
