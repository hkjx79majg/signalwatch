package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// rangeQuery builds the metric-range target with properly escaped parameters.
func rangeQuery(name, start, end string, step int, labels ...string) string {
	v := url.Values{}
	v.Set("name", name)
	v.Set("start", start)
	v.Set("end", end)
	v.Set("step", fmt.Sprintf("%d", step))
	for i := 0; i+1 < len(labels); i += 2 {
		v.Set("label."+labels[i], labels[i+1])
	}
	return metricRangePath + "?" + v.Encode()
}

func rfc(ts time.Time) string {
	return ts.Format(time.RFC3339Nano)
}

// rangePoints decodes the points of the i-th returned series.
func rangePoints(t *testing.T, rec map[string]any, i int) []any {
	t.Helper()
	series, ok := rec["series"].([]any)
	if !ok || len(series) <= i {
		t.Fatalf("series[%d] missing: %v", i, rec["series"])
	}
	points, ok := series[i].(map[string]any)["points"].([]any)
	if !ok {
		t.Fatalf("series[%d].points missing: %v", i, series[i])
	}
	return points
}

func TestMetricRangeCounterWindows(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	east := time.FixedZone("UTC+8", 8*3600)

	// Samples in three windows; the first timestamp uses a +08:00 offset to
	// verify instants are compared absolutely and output normalized to UTC.
	body := fmt.Sprintf(`{"samples":[
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":1,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":2,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":4,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/b"},"value":7,"timestamp":%q}
	]}`,
		rfc(base.Add(10*time.Second).In(east)),
		rfc(base.Add(20*time.Second)),
		rfc(base.Add(70*time.Second)),
		rfc(base.Add(15*time.Second)),
	)
	rec := doRequest(t, h, http.MethodPost, metricsPath, body, "application/json")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
	}

	target := rangeQuery("hits", rfc(base), rfc(base.Add(180*time.Second)), 60, "route", "/a")
	rec = doRequest(t, h, http.MethodGet, target, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)
	points := rangePoints(t, payload, 0)
	if len(points) != 2 {
		t.Fatalf("points = %v, want 2 points (empty window omitted)", points)
	}
	first := points[0].(map[string]any)
	if first["timestamp"] != rfc(base.UTC()) {
		t.Fatalf("point timestamp = %v, want UTC %q", first["timestamp"], rfc(base.UTC()))
	}
	if !strings.HasSuffix(first["timestamp"].(string), "Z") {
		t.Fatalf("point timestamp %v not normalized to UTC", first["timestamp"])
	}
	if first["value"] != 3.0 {
		t.Fatalf("window 0 value = %v, want 3", first["value"])
	}
	second := points[1].(map[string]any)
	if second["timestamp"] != rfc(base.Add(60*time.Second).UTC()) || second["value"] != 4.0 {
		t.Fatalf("window 1 = %v, want ts=%q value=4", second, rfc(base.Add(60*time.Second).UTC()))
	}

	// The /b series is excluded by the label selector but visible without it.
	target = rangeQuery("hits", rfc(base), rfc(base.Add(180*time.Second)), 60)
	payload = decodeBody(t, doRequest(t, h, http.MethodGet, target, "", ""))
	series := payload["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("series count = %d, want 2", len(series))
	}
	// Canonical full-label order: {route:/a} sorts before {route:/b}.
	if series[0].(map[string]any)["labels"].(map[string]any)["route"] != "/a" {
		t.Fatalf("series order wrong: %v", series)
	}
}

func TestMetricRangeGaugeLatestAndCommitOrder(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	t1, t2 := base.Add(10*time.Second), base.Add(20*time.Second)

	post := func(value float64, ts time.Time) {
		body := fmt.Sprintf(`{"samples":[{"name":"temp","type":"gauge","labels":{},"value":%v,"timestamp":%q}]}`, value, rfc(ts))
		rec := doRequest(t, h, http.MethodPost, metricsPath, body, "application/json")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
		}
	}
	post(100, t1)
	post(7, t2)  // later timestamp wins over the larger earlier value
	post(9, t2)  // same instant: later commit wins
	post(42, t1) // earlier timestamp must not displace the window's latest

	target := rangeQuery("temp", rfc(base), rfc(base.Add(60*time.Second)), 60)
	payload := decodeBody(t, doRequest(t, h, http.MethodGet, target, "", ""))
	points := rangePoints(t, payload, 0)
	if len(points) != 1 || points[0].(map[string]any)["value"] != 9.0 {
		t.Fatalf("points = %v, want single point value 9", points)
	}
}

func TestMetricRangeHistogramWindows(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[
		{"name":"lat","type":"histogram","labels":{},"value":0.5,"buckets":[1,10],"timestamp":%q},
		{"name":"lat","type":"histogram","labels":{},"value":5,"buckets":[1,10],"timestamp":%q},
		{"name":"lat","type":"histogram","labels":{},"value":50,"buckets":[1,10],"timestamp":%q},
		{"name":"lat","type":"histogram","labels":{},"value":2,"buckets":[1,10],"timestamp":%q}
	]}`,
		rfc(base.Add(10*time.Second)), rfc(base.Add(20*time.Second)),
		rfc(base.Add(30*time.Second)), rfc(base.Add(70*time.Second)),
	)
	rec := doRequest(t, h, http.MethodPost, metricsPath, body, "application/json")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
	}

	target := rangeQuery("lat", rfc(base), rfc(base.Add(120*time.Second)), 60)
	payload := decodeBody(t, doRequest(t, h, http.MethodGet, target, "", ""))
	points := rangePoints(t, payload, 0)
	if len(points) != 2 {
		t.Fatalf("points = %v, want 2", points)
	}
	w0 := points[0].(map[string]any)
	if w0["count"] != 3.0 || w0["sum"] != 55.5 {
		t.Fatalf("window 0 count/sum = %v/%v, want 3/55.5", w0["count"], w0["sum"])
	}
	buckets := w0["buckets"].([]any)
	if len(buckets) != 2 ||
		buckets[0].(map[string]any)["le"] != 1.0 || buckets[0].(map[string]any)["count"] != 1.0 ||
		buckets[1].(map[string]any)["le"] != 10.0 || buckets[1].(map[string]any)["count"] != 2.0 {
		t.Fatalf("window 0 buckets = %v, want le=1:1 le=10:2", buckets)
	}
	w1 := points[1].(map[string]any)
	if w1["count"] != 1.0 || w1["sum"] != 2.0 {
		t.Fatalf("window 1 count/sum = %v/%v, want 1/2", w1["count"], w1["sum"])
	}
}

func TestMetricRangeDefaultTimestamp(t *testing.T) {
	h := Handler()
	rec := doRequest(t, h, http.MethodPost, metricsPath,
		`{"samples":[{"name":"hits","type":"counter","labels":{},"value":3}]}`, "application/json")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
	}

	now := time.Now()
	target := rangeQuery("hits", rfc(now.Add(-time.Hour)), rfc(now.Add(time.Hour)), 3600)
	payload := decodeBody(t, doRequest(t, h, http.MethodGet, target, "", ""))
	points := rangePoints(t, payload, 0)
	if len(points) != 1 || points[0].(map[string]any)["value"] != 3.0 {
		t.Fatalf("points = %v, want one point with value 3", points)
	}
}

func TestMetricsTimestampValidation(t *testing.T) {
	h := Handler()
	now := time.Now()

	// Malformed timestamps are format errors.
	for _, ts := range []string{"not-a-time", "2026-01-01", "2026-01-01T00:00:00"} {
		body := fmt.Sprintf(`{"samples":[{"name":"m","type":"gauge","labels":{},"value":1,"timestamp":%q}]}`, ts)
		rec := doRequest(t, h, http.MethodPost, metricsPath, body, "application/json")
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_metrics")
	}

	// Outside [now-24h, now+5min] the whole batch is rejected and leaves no
	// state behind, including the valid sample sharing the batch.
	outOfRange := []time.Time{now.Add(-24*time.Hour - time.Second), now.Add(5*time.Minute + time.Second)}
	for _, ts := range outOfRange {
		body := fmt.Sprintf(`{"samples":[
			{"name":"m","type":"counter","labels":{},"value":1},
			{"name":"m","type":"counter","labels":{},"value":1,"timestamp":%q}
		]}`, rfc(ts))
		rec := doRequest(t, h, http.MethodPost, metricsPath, body, "application/json")
		expectErrorCode(t, rec, http.StatusBadRequest, "metric_timestamp_out_of_range")
	}

	payload := decodeBody(t, getMetrics(t, h, metricsPath+"?name=m"))
	if got := payload["series"].([]any); len(got) != 0 {
		t.Fatalf("rejected batches left state behind: %v", got)
	}

	// Boundary-adjacent timestamps inside the window are accepted.
	body := fmt.Sprintf(`{"samples":[
		{"name":"m","type":"counter","labels":{},"value":1,"timestamp":%q},
		{"name":"m","type":"counter","labels":{},"value":1,"timestamp":%q}
	]}`, rfc(now.Add(-23*time.Hour-59*time.Minute)), rfc(now.Add(4*time.Minute)))
	rec := doRequest(t, h, http.MethodPost, metricsPath, body, "application/json")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("in-window timestamps rejected: %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestMetricRangeQueryValidation(t *testing.T) {
	h := Handler()
	now := time.Now()
	start, end := rfc(now.Add(-time.Hour)), rfc(now)
	far := rfc(now.Add(10001 * time.Second))

	bad := []string{
		metricRangePath, // all missing
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end), // no step
		metricRangePath + "?start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=60",
		metricRangePath + "?name=m&name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=60",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=60&foo=1",
		metricRangePath + "?name=1bad&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=60",
		metricRangePath + "?name=m&start=garbage&end=" + url.QueryEscape(end) + "&step=60",
		metricRangePath + "?name=m&start=" + url.QueryEscape(end) + "&end=" + url.QueryEscape(start) + "&step=60",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(start) + "&step=60",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=0",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=3601",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=1.5",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=abc",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=-1",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(far) + "&step=1", // >10000 windows
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=60&label.1x=v",
		metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=60&label.a=1&label.a=2",
	}
	for _, target := range bad {
		rec := doRequest(t, h, http.MethodGet, target, "", "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s: status = %d, want 400", target, rec.Code)
		}
		if code := decodeBody(t, rec)["error"].(map[string]any)["code"]; code != "invalid_metric_range" {
			t.Fatalf("GET %s: code = %v, want invalid_metric_range", target, code)
		}
	}

	// Exactly 10000 windows is allowed.
	ok := metricRangePath + "?name=m&start=" + url.QueryEscape(start) + "&end=" +
		url.QueryEscape(rfc(now.Add(-time.Hour).Add(10000*time.Second))) + "&step=1"
	rec := doRequest(t, h, http.MethodGet, ok, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("10000-window query: status = %d, body=%q", rec.Code, rec.Body.String())
	}
}

func TestMetricRangeEmptyAndMethod(t *testing.T) {
	h := Handler()
	now := time.Now()
	target := rangeQuery("missing", rfc(now.Add(-time.Hour)), rfc(now), 60)

	rec := doRequest(t, h, http.MethodGet, target, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)
	if series, ok := payload["series"].([]any); !ok || len(series) != 0 {
		t.Fatalf("series = %v, want empty array", payload["series"])
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := doRequest(t, h, method, target, "", "")
		expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("%s: Allow = %q, want GET", method, allow)
		}
	}
}

func TestMetricRangeNonFinite(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[
		{"name":"big","type":"counter","labels":{},"value":1.7976931348623157e308,"timestamp":%q},
		{"name":"big","type":"counter","labels":{},"value":1.7976931348623157e308,"timestamp":%q}
	]}`, rfc(base.Add(10*time.Second)), rfc(base.Add(20*time.Second)))
	rec := doRequest(t, h, http.MethodPost, metricsPath, body, "application/json")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
	}

	target := rangeQuery("big", rfc(base), rfc(base.Add(60*time.Second)), 60)
	rec = doRequest(t, h, http.MethodGet, target, "", "")
	expectErrorCode(t, rec, http.StatusUnprocessableEntity, "invalid_metric_range_data")
}

func TestMetricRangeTenantIsolation(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[{"name":"hits","type":"counter","labels":{},"value":5,"timestamp":%q}]}`, rfc(base.Add(10*time.Second)))
	rec := doRequest(t, h, http.MethodPost, metricsPath, body, "application/json", "alpha")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
	}

	target := rangeQuery("hits", rfc(base), rfc(base.Add(60*time.Second)), 60)
	payload := decodeBody(t, doRequest(t, h, http.MethodGet, target, "", "", "beta"))
	if series := payload["series"].([]any); len(series) != 0 {
		t.Fatalf("tenant beta sees alpha history: %v", series)
	}
	payload = decodeBody(t, doRequest(t, h, http.MethodGet, target, "", "", "alpha"))
	points := rangePoints(t, payload, 0)
	if len(points) != 1 || points[0].(map[string]any)["value"] != 5.0 {
		t.Fatalf("tenant alpha points = %v, want one point value 5", points)
	}
}

func TestMetricRangeDoesNotAffectCurrentValues(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[
		{"name":"hits","type":"counter","labels":{},"value":2,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{},"value":3,"timestamp":%q}
	]}`, rfc(base.Add(10*time.Second)), rfc(base.Add(20*time.Second)))
	rec := doRequest(t, h, http.MethodPost, metricsPath, body, "application/json")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
	}

	// The current-value endpoint is untouched by history handling.
	payload := decodeBody(t, getMetrics(t, h, metricsPath+"?name=hits"))
	series := payload["series"].([]any)
	if len(series) != 1 || series[0].(map[string]any)["value"] != 5.0 {
		t.Fatalf("current value = %v, want single series value 5", series)
	}

	// Range queries do not mutate current values either.
	target := rangeQuery("hits", rfc(base), rfc(base.Add(60*time.Second)), 60)
	doRequest(t, h, http.MethodGet, target, "", "")
	payload = decodeBody(t, getMetrics(t, h, metricsPath+"?name=hits"))
	if series := payload["series"].([]any); len(series) != 1 || series[0].(map[string]any)["value"] != 5.0 {
		t.Fatalf("current value after range query = %v, want 5", series)
	}
}
