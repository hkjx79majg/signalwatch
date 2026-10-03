package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func getMetricRange(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func rangeTarget(name string, start, end time.Time, step int, extra ...string) string {
	v := url.Values{}
	v.Set("name", name)
	v.Set("start", start.UTC().Format(time.RFC3339Nano))
	v.Set("end", end.UTC().Format(time.RFC3339Nano))
	v.Set("step", fmt.Sprintf("%d", step))
	q := v.Encode()
	for _, e := range extra {
		q += "&" + e
	}
	return metricRangePath + "?" + q
}

func ts(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func TestMetricRangeCounterWindows(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)

	// Three windows of 60s; window 1 gets two samples, window 2 none.
	body := fmt.Sprintf(`{"samples":[
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":3,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":2.5,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":4,"timestamp":%q}
	]}`, ts(base.Add(5*time.Second)), ts(base.Add(125*time.Second)), ts(base.Add(10*time.Second)))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
	}

	rec := getMetricRange(t, h, rangeTarget("hits", base, base.Add(3*time.Minute), 60, "label.route=/a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	series := decodeBody(t, rec)["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("series len = %d, want 1", len(series))
	}
	s := series[0].(map[string]any)
	if s["name"] != "hits" || s["type"] != "counter" {
		t.Fatalf("unexpected series: %v", s)
	}
	points := s["points"].([]any)
	if len(points) != 2 {
		t.Fatalf("points len = %d, want 2 (empty window must not emit): %v", len(points), points)
	}
	p0 := points[0].(map[string]any)
	if p0["timestamp"] != ts(base) || p0["value"] != float64(7) {
		t.Fatalf("point 0 = %v, want {%s 7}", p0, ts(base))
	}
	p1 := points[1].(map[string]any)
	if p1["timestamp"] != ts(base.Add(2*time.Minute)) || p1["value"] != 2.5 {
		t.Fatalf("point 1 = %v, want {%s 2.5}", p1, ts(base.Add(2*time.Minute)))
	}

	// The range query must not disturb the cumulative current value.
	rec = getMetrics(t, h, metricsPath+"?name=hits")
	cur := decodeBody(t, rec)["series"].([]any)[0].(map[string]any)
	if cur["value"] != 9.5 {
		t.Fatalf("current value = %v, want 9.5", cur["value"])
	}
}

func TestMetricRangeGaugeLatestAndCommitTie(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	// Same instant twice: the later commit wins. A later timestamp in the
	// same window wins regardless of commit order.
	body := fmt.Sprintf(`{"samples":[
		{"name":"temp","type":"gauge","labels":{},"value":1,"timestamp":%q},
		{"name":"temp","type":"gauge","labels":{},"value":2,"timestamp":%q},
		{"name":"temp","type":"gauge","labels":{},"value":3,"timestamp":%q}
	]}`, ts(base.Add(10*time.Second)), ts(base.Add(10*time.Second)), ts(base.Add(5*time.Second)))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	rec := getMetricRange(t, h, rangeTarget("temp", base, base.Add(time.Minute), 60))
	points := decodeBody(t, rec)["series"].([]any)[0].(map[string]any)["points"].([]any)
	if len(points) != 1 {
		t.Fatalf("points len = %d, want 1", len(points))
	}
	if got := points[0].(map[string]any)["value"]; got != float64(2) {
		t.Fatalf("gauge point = %v, want 2 (latest instant, later commit on tie)", got)
	}
}

func TestMetricRangeHistogramWindows(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	body := fmt.Sprintf(`{"samples":[
		{"name":"lat","type":"histogram","labels":{},"value":1,"buckets":[1,5,10],"timestamp":%q},
		{"name":"lat","type":"histogram","labels":{},"value":6,"buckets":[1,5,10],"timestamp":%q},
		{"name":"lat","type":"histogram","labels":{},"value":99,"buckets":[1,5,10],"timestamp":%q}
	]}`, ts(base.Add(time.Second)), ts(base.Add(2*time.Second)), ts(base.Add(61*time.Second)))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	rec := getMetricRange(t, h, rangeTarget("lat", base, base.Add(2*time.Minute), 60))
	s := decodeBody(t, rec)["series"].([]any)[0].(map[string]any)
	points := s["points"].([]any)
	if len(points) != 2 {
		t.Fatalf("points len = %d, want 2", len(points))
	}
	p0 := points[0].(map[string]any)
	if p0["count"] != float64(2) || p0["sum"] != float64(7) {
		t.Fatalf("histogram window 0 = %v", p0)
	}
	buckets := p0["buckets"].([]any)
	want := []float64{1, 1, 2}
	for i, w := range want {
		if got := buckets[i].(map[string]any)["count"]; got != w {
			t.Fatalf("bucket %d = %v, want %v", i, got, w)
		}
	}
	p1 := points[1].(map[string]any)
	if p1["count"] != float64(1) || p1["sum"] != float64(99) {
		t.Fatalf("histogram window 1 = %v", p1)
	}
}

func TestMetricRangeDefaultTimestampAndUTCOutput(t *testing.T) {
	h := Handler()

	// No timestamp: the sample lands at the receive instant.
	if rec := postMetrics(t, h, `{"samples":[{"name":"m","type":"gauge","labels":{},"value":7}]}`, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}
	// Explicit non-UTC offset timestamp: output must be normalized to UTC.
	east := time.FixedZone("plus8", 8*3600)
	explicit := time.Now().Add(-30 * time.Minute).In(east).Format(time.RFC3339Nano)
	body := fmt.Sprintf(`{"samples":[{"name":"m","type":"gauge","labels":{},"value":8,"timestamp":%q}]}`, explicit)
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
	}

	start := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	rec := getMetricRange(t, h, rangeTarget("m", start, time.Now().Add(time.Hour), 600))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	points := decodeBody(t, rec)["series"].([]any)[0].(map[string]any)["points"].([]any)
	if len(points) == 0 {
		t.Fatal("expected points for default and explicit timestamps")
	}
	for _, p := range points {
		stamp := p.(map[string]any)["timestamp"].(string)
		parsed, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			t.Fatalf("point timestamp %q not RFC3339Nano", stamp)
		}
		if !strings.HasSuffix(stamp, "Z") || !parsed.Equal(parsed.UTC()) {
			t.Fatalf("point timestamp %q not normalized to UTC", stamp)
		}
	}
	// Latest observation wins: the default-timestamp sample (now) beats the
	// explicit one (30m ago) when they share a window.
	if got := points[len(points)-1].(map[string]any)["value"]; got != float64(7) {
		t.Fatalf("latest point = %v, want 7", got)
	}
}

func TestPostTimestampValidation(t *testing.T) {
	h := Handler()

	badFormat := []string{
		`{"samples":[{"name":"m","type":"gauge","labels":{},"value":1,"timestamp":"not-a-time"}]}`,
		`{"samples":[{"name":"m","type":"gauge","labels":{},"value":1,"timestamp":"2026-01-02T03:04:05"}]}`,  // no zone
		`{"samples":[{"name":"m","type":"gauge","labels":{},"value":1,"timestamp":"2026-01-02 03:04:05Z"}]}`, // space separator
		`{"samples":[{"name":"m","type":"gauge","labels":{},"value":1,"timestamp":123}]}`,
	}
	for _, body := range badFormat {
		expectErrorCode(t, postMetrics(t, h, body, ""), http.StatusBadRequest, "invalid_metrics")
	}

	tooOld := time.Now().Add(-24*time.Hour - time.Minute).UTC().Format(time.RFC3339Nano)
	tooNew := time.Now().Add(5*time.Minute + time.Minute).UTC().Format(time.RFC3339Nano)
	for _, stamp := range []string{tooOld, tooNew} {
		body := fmt.Sprintf(`{"samples":[{"name":"m","type":"gauge","labels":{},"value":1,"timestamp":%q}]}`, stamp)
		expectErrorCode(t, postMetrics(t, h, body, ""), http.StatusBadRequest, "metric_timestamp_out_of_range")
	}

	// A batch with one out-of-range sample commits nothing: no current value,
	// no history.
	good := time.Now().UTC().Format(time.RFC3339Nano)
	body := fmt.Sprintf(`{"samples":[
		{"name":"atomic","type":"counter","labels":{},"value":5,"timestamp":%q},
		{"name":"atomic","type":"counter","labels":{},"value":5,"timestamp":%q}
	]}`, good, tooOld)
	expectErrorCode(t, postMetrics(t, h, body, ""), http.StatusBadRequest, "metric_timestamp_out_of_range")
	rec := getMetrics(t, h, metricsPath+"?name=atomic")
	if got := decodeBody(t, rec)["series"].([]any); len(got) != 0 {
		t.Fatalf("failed batch left current state: %v", got)
	}
	start := time.Now().Add(-time.Hour)
	rec = getMetricRange(t, h, rangeTarget("atomic", start, time.Now().Add(time.Hour), 60))
	if got := decodeBody(t, rec)["series"].([]any); len(got) != 0 {
		t.Fatalf("failed batch left history: %v", got)
	}

	// Timestamps just inside the accepted window are fine.
	oldOK := time.Now().Add(-24*time.Hour + time.Minute).UTC().Format(time.RFC3339Nano)
	newOK := time.Now().Add(5*time.Minute - time.Second).UTC().Format(time.RFC3339Nano)
	body = fmt.Sprintf(`{"samples":[
		{"name":"edge","type":"counter","labels":{},"value":1,"timestamp":%q},
		{"name":"edge","type":"counter","labels":{},"value":2,"timestamp":%q}
	]}`, oldOK, newOK)
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("boundary timestamps rejected: %d %q", rec.Code, rec.Body.String())
	}
}

func TestMetricRangeQueryValidation(t *testing.T) {
	h := Handler()
	now := time.Now().UTC()
	start := now.Add(-time.Hour).Format(time.RFC3339Nano)
	end := now.Format(time.RFC3339Nano)
	base := metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=60"

	bad := []string{
		metricRangePath, // everything missing
		metricRangePath + "?start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=60", // no name
		base + "&name=m",              // duplicate name
		base + "&step=60",             // duplicate step
		base + "&label.a=1&label.a=1", // duplicate label key
		base + "&bogus=1",             // unknown parameter
		metricRangePath + "?name=1bad&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=60",
		base + "&label.=x",    // empty label key
		base + "&label.a-b=x", // bad label key
		metricRangePath + "?name=m&start=nope&end=" + url.QueryEscape(end) + "&step=60",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(start) + "&step=60", // start == end
		metricRangePath + "?name=m&start=" + url.QueryEscape(end) + "&end=" + url.QueryEscape(start) + "&step=60",   // start > end
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=0",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=3601",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=1.5",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=+60",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=", // empty step
		// 10001 windows of 1s.
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(now.Add(10001*time.Second-time.Hour).Format(time.RFC3339Nano)) + "&step=1",
	}
	for _, target := range bad {
		rec := getMetricRange(t, h, target)
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_metric_range")
	}

	// Exactly 10000 windows is accepted; no data yields an empty array.
	ok := metricRangePath + "?name=m&start=" + url.QueryEscape(start) +
		"&end=" + url.QueryEscape(now.Add(10000*time.Second-time.Hour).Format(time.RFC3339Nano)) + "&step=1"
	rec := getMetricRange(t, h, ok)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"series":[]}` {
		t.Fatalf("10000-window response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestMetricRangeMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, metricRangePath, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Fatalf("%s Allow = %q, want GET", method, got)
		}
		expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

func TestMetricRangeSelectorAndSeriesOrder(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[
		{"name":"m","type":"counter","labels":{"a":"1","b":"2"},"value":1,"timestamp":%q},
		{"name":"m","type":"counter","labels":{"a":"1","b":"3"},"value":2,"timestamp":%q},
		{"name":"m","type":"counter","labels":{"a":"9"},"value":3,"timestamp":%q},
		{"name":"n","type":"counter","labels":{},"value":4,"timestamp":%q}
	]}`, ts(base), ts(base), ts(base), ts(base))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	// Selector filters; series follow the canonical full-label order.
	rec := getMetricRange(t, h, rangeTarget("m", base, base.Add(time.Minute), 60, "label.a=1"))
	series := decodeBody(t, rec)["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("series len = %d, want 2", len(series))
	}
	if got := series[0].(map[string]any)["labels"].(map[string]any)["b"]; got != "2" {
		t.Fatalf("series order = %v", series)
	}

	// No selector: all three series of m, ordered by normalized label set.
	rec = getMetricRange(t, h, rangeTarget("m", base, base.Add(time.Minute), 60))
	series = decodeBody(t, rec)["series"].([]any)
	if len(series) != 3 {
		t.Fatalf("series len = %d, want 3", len(series))
	}
	labels := make([]string, 3)
	for i, s := range series {
		labels[i] = fmt.Sprintf("%v", s.(map[string]any)["labels"])
	}
	if labels[0] != "map[a:1 b:2]" || labels[1] != "map[a:1 b:3]" || labels[2] != "map[a:9]" {
		t.Fatalf("series order = %v", labels)
	}
}

func TestMetricRangeNonFiniteAggregation(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[
		{"name":"big","type":"counter","labels":{},"value":1e308,"timestamp":%q},
		{"name":"big","type":"counter","labels":{},"value":1e308,"timestamp":%q}
	]}`, ts(base), ts(base.Add(time.Second)))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	rec := getMetricRange(t, h, rangeTarget("big", base, base.Add(time.Minute), 60))
	expectErrorCode(t, rec, http.StatusUnprocessableEntity, "invalid_metric_range_data")
}

func TestMetricRangeRetentionTrim(t *testing.T) {
	h := Handler()
	// One sample near the 24h retention edge, one recent.
	old := time.Now().Add(-24*time.Hour + 5*time.Minute).UTC()
	recent := time.Now().Add(-time.Minute).UTC()
	body := fmt.Sprintf(`{"samples":[
		{"name":"m","type":"counter","labels":{},"value":1,"timestamp":%q},
		{"name":"m","type":"counter","labels":{},"value":2,"timestamp":%q}
	]}`, ts(old), ts(recent))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	// A start far before the retention boundary is not an error; only the
	// retained portion is returned.
	start := old.Add(-time.Hour).Truncate(time.Minute)
	rec := getMetricRange(t, h, rangeTarget("m", start, time.Now().Add(time.Minute), 3600))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	series := decodeBody(t, rec)["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("series len = %d, want to be 1", len(series))
	}
	points := series[0].(map[string]any)["points"].([]any)
	if len(points) != 2 {
		t.Fatalf("points len = %d, want 2 (both samples still retained)", len(points))
	}
}

func TestMetricRangeTenantIsolation(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[{"name":"m","type":"counter","labels":{},"value":5,"timestamp":%q}]}`, ts(base))

	req := httptest.NewRequest(http.MethodPost, metricsPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-SignalWatch-Tenant", "alpha")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	target := rangeTarget("m", base, base.Add(time.Minute), 60)

	// Other tenants and the default tenant see nothing.
	for _, tenant := range []string{"", "beta"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if tenant != "" {
			req.Header.Set("X-SignalWatch-Tenant", tenant)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := decodeBody(t, rec)["series"].([]any); len(got) != 0 {
			t.Fatalf("tenant %q saw %v", tenant, got)
		}
	}

	req = httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("X-SignalWatch-Tenant", "alpha")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	series := decodeBody(t, rec)["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("alpha series len = %d, want 1", len(series))
	}
}
