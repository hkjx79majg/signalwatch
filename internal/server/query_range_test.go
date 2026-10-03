package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func queryRangeTarget(expr string, start, end time.Time, step int) string {
	v := url.Values{}
	v.Set("expr", expr)
	v.Set("start", start.UTC().Format(time.RFC3339Nano))
	v.Set("end", end.UTC().Format(time.RFC3339Nano))
	v.Set("step", fmt.Sprintf("%d", step))
	return queryRangePath + "?" + v.Encode()
}

func getQueryRange(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func expectMatrix(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%q", rec.Code, rec.Body.String())
	}
	var payload struct {
		ResultType string           `json:"result_type"`
		Result     []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON body %q: %v", rec.Body.String(), err)
	}
	if payload.ResultType != "matrix" {
		t.Fatalf("result_type = %q, want matrix", payload.ResultType)
	}
	return payload.Result
}

func pointsOf(s map[string]any) []map[string]any {
	raw := s["points"].([]any)
	out := make([]map[string]any, len(raw))
	for i, p := range raw {
		out[i] = p.(map[string]any)
	}
	return out
}

func TestQueryRangePlainSelectorCounterAndGauge(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)

	// Counter: two increments in window 0, one in window 2, window 1 empty.
	// Gauge: latest timestamp in window 0 wins, commit order breaks ties.
	body := fmt.Sprintf(`{"samples":[
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":3,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":4,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":2.5,"timestamp":%q},
		{"name":"temp","type":"gauge","labels":{"zone":"z1"},"value":1,"timestamp":%q},
		{"name":"temp","type":"gauge","labels":{"zone":"z1"},"value":2,"timestamp":%q},
		{"name":"hits","type":"histogram","labels":{"route":"/h"},"value":1,"buckets":[1,2],"timestamp":%q}
	]}`,
		ts(base.Add(5*time.Second)), ts(base.Add(10*time.Second)), ts(base.Add(125*time.Second)),
		ts(base.Add(10*time.Second)), ts(base.Add(10*time.Second)),
		ts(base.Add(5*time.Second)))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
	}

	rec := getQueryRange(t, h, queryRangeTarget(`hits{route="/a"}`, base, base.Add(3*time.Minute), 60))
	got := expectMatrix(t, rec)
	if len(got) != 1 {
		t.Fatalf("series len = %d, want 1 (histogram excluded): %v", len(got), got)
	}
	labels := got[0]["labels"].(map[string]any)
	if labels["route"] != "/a" || len(labels) != 1 {
		t.Fatalf("labels = %v", labels)
	}
	points := pointsOf(got[0])
	if len(points) != 2 {
		t.Fatalf("points len = %d, want 2 (empty window omitted): %v", len(points), points)
	}
	if points[0]["timestamp"] != ts(base) || points[0]["value"] != float64(7) {
		t.Fatalf("point 0 = %v, want {%s 7}", points[0], ts(base))
	}
	if points[1]["timestamp"] != ts(base.Add(2*time.Minute)) || points[1]["value"] != 2.5 {
		t.Fatalf("point 1 = %v, want {%s 2.5}", points[1], ts(base.Add(2*time.Minute)))
	}

	// Gauge: later commit at the same timestamp wins.
	rec = getQueryRange(t, h, queryRangeTarget(`temp{zone="z1"}`, base, base.Add(time.Minute), 60))
	got = expectMatrix(t, rec)
	if len(got) != 1 {
		t.Fatalf("gauge series len = %d", len(got))
	}
	points = pointsOf(got[0])
	if len(points) != 1 || points[0]["value"] != float64(2) {
		t.Fatalf("gauge point = %v, want value 2", points)
	}
}

func TestQueryRangeSeriesOrderAndTimestamps(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[
		{"name":"m","type":"counter","labels":{"a":"1","b":"2"},"value":1,"timestamp":%q},
		{"name":"m","type":"counter","labels":{"a":"1","b":"3"},"value":2,"timestamp":%q},
		{"name":"m","type":"counter","labels":{"a":"9"},"value":3,"timestamp":%q}
	]}`, ts(base), ts(base), ts(base))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	rec := getQueryRange(t, h, queryRangeTarget(`m{}`, base, base.Add(time.Minute), 60))
	got := expectMatrix(t, rec)
	if len(got) != 3 {
		t.Fatalf("series len = %d, want 3", len(got))
	}
	want := []string{"map[a:1 b:2]", "map[a:1 b:3]", "map[a:9]"}
	for i, s := range got {
		if fmt.Sprintf("%v", s["labels"]) != want[i] {
			t.Fatalf("series order = %v, want %v", got, want)
		}
	}

	// Every timestamp is the UTC RFC3339Nano window start.
	for _, p := range pointsOf(got[0]) {
		stamp := p["timestamp"].(string)
		parsed, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || !strings.HasSuffix(stamp, "Z") || !parsed.Equal(parsed.UTC()) {
			t.Fatalf("bad UTC window timestamp %q", stamp)
		}
	}
}

func TestQueryRangeAggregationPerWindowParticipation(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)

	// z1: series /a has values in windows 0 and 2; series /b only in window 0.
	// z2: series /c only in window 0. Window 1 is empty for everyone.
	body := fmt.Sprintf(`{"samples":[
		{"name":"hits","type":"counter","labels":{"route":"/a","zone":"z1"},"value":3,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/a","zone":"z1"},"value":5,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/b","zone":"z1"},"value":1,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/c","zone":"z2"},"value":7,"timestamp":%q}
	]}`, ts(base.Add(5*time.Second)), ts(base.Add(125*time.Second)),
		ts(base.Add(10*time.Second)), ts(base.Add(7*time.Second)))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	// sum without by: window 0 folds both z1 series (4), window 2 only /a (5);
	// no point is emitted for the empty window 1.
	rec := getQueryRange(t, h, queryRangeTarget(`sum(hits{zone="z1"})`, base, base.Add(3*time.Minute), 60))
	got := expectMatrix(t, rec)
	if len(got) != 1 || len(got[0]["labels"].(map[string]any)) != 0 {
		t.Fatalf("sum result = %v", got)
	}
	points := pointsOf(got[0])
	if len(points) != 2 {
		t.Fatalf("sum points len = %d, want 2: %v", len(points), points)
	}
	if points[0]["timestamp"] != ts(base) || points[0]["value"] != float64(4) {
		t.Fatalf("sum w0 = %v, want 4", points[0])
	}
	if points[1]["timestamp"] != ts(base.Add(2*time.Minute)) || points[1]["value"] != float64(5) {
		t.Fatalf("sum w2 = %v, want 5", points[1])
	}

	// avg divides by the actual number of series with a value in each window:
	// 2 participants in window 0, 1 in window 2.
	rec = getQueryRange(t, h, queryRangeTarget(`avg(hits{zone="z1"})`, base, base.Add(3*time.Minute), 60))
	points = pointsOf(expectMatrix(t, rec)[0])
	if points[0]["value"] != float64(2) || points[1]["value"] != float64(5) {
		t.Fatalf("avg per-window divisor wrong: %v", points)
	}

	// min/max fold window-locally as well.
	rec = getQueryRange(t, h, queryRangeTarget(`min(hits{zone="z1"})`, base, base.Add(3*time.Minute), 60))
	points = pointsOf(expectMatrix(t, rec)[0])
	if points[0]["value"] != float64(1) || points[1]["value"] != float64(5) {
		t.Fatalf("min windows = %v", points)
	}
	rec = getQueryRange(t, h, queryRangeTarget(`max(hits{zone="z1"})`, base, base.Add(3*time.Minute), 60))
	points = pointsOf(expectMatrix(t, rec)[0])
	if points[0]["value"] != float64(3) || points[1]["value"] != float64(5) {
		t.Fatalf("max windows = %v", points)
	}

	// by grouping: z1 gets points in windows 0 and 2; z2 only in window 0.
	// Groups are full-label ordered (z1 first); no zero is filled for z2's
	// missing window.
	rec = getQueryRange(t, h, queryRangeTarget(`sum by (zone)(hits{})`, base, base.Add(3*time.Minute), 60))
	got = expectMatrix(t, rec)
	if len(got) != 2 {
		t.Fatalf("by groups = %d, want 2: %v", len(got), got)
	}
	if got[0]["labels"].(map[string]any)["zone"] != "z1" {
		t.Fatalf("group order = %v", got)
	}
	z1 := pointsOf(got[0])
	z2 := pointsOf(got[1])
	if len(z1) != 2 || z1[0]["value"] != float64(4) || z1[1]["value"] != float64(5) {
		t.Fatalf("z1 points = %v", z1)
	}
	if len(z2) != 1 || z2[0]["timestamp"] != ts(base) || z2[0]["value"] != float64(7) {
		t.Fatalf("z2 points = %v, want single {w0 7}", z2)
	}
}

func TestQueryRangeByAbsentLabelProjectsAway(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[
		{"name":"m","type":"gauge","labels":{"route":"a","zone":"z1"},"value":2,"timestamp":%q},
		{"name":"m","type":"gauge","labels":{"route":"b"},"value":9,"timestamp":%q}
	]}`, ts(base), ts(base.Add(60*time.Second)))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	rec := getQueryRange(t, h, queryRangeTarget(`sum by (zone)(m{})`, base, base.Add(2*time.Minute), 60))
	got := expectMatrix(t, rec)
	if len(got) != 2 {
		t.Fatalf("want 2 groups, got %v", got)
	}
	// The empty-label group sorts first; it only has a point in window 1.
	if len(got[0]["labels"].(map[string]any)) != 0 {
		t.Fatalf("first group labels = %v, want {}", got[0]["labels"])
	}
	p0 := pointsOf(got[0])
	if len(p0) != 1 || p0[0]["timestamp"] != ts(base.Add(time.Minute)) || p0[0]["value"] != float64(9) {
		t.Fatalf("empty-label group points = %v", p0)
	}
	p1 := pointsOf(got[1])
	if len(p1) != 1 || p1[0]["timestamp"] != ts(base) || p1[0]["value"] != float64(2) {
		t.Fatalf("z1 group points = %v", p1)
	}
}

func TestQueryRangeEmptyResultShape(t *testing.T) {
	h := Handler()
	now := time.Now().UTC()
	base := now.Add(-time.Hour).Truncate(time.Second)

	// Unknown metric: an empty array, never null.
	rec := getQueryRange(t, h, queryRangeTarget(`missing{}`, base, now.Add(time.Hour), 60))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"result_type":"matrix","result":[]}` {
		t.Fatalf("empty body = %q", got)
	}

	// A matching selector whose only series is a histogram is also empty.
	body := fmt.Sprintf(`{"samples":[{"name":"lat","type":"histogram","labels":{},"value":1,"buckets":[1],"timestamp":%q}]}`, ts(base))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}
	rec = getQueryRange(t, h, queryRangeTarget(`lat{}`, base, now.Add(time.Hour), 60))
	if got := strings.TrimSpace(rec.Body.String()); got != `{"result_type":"matrix","result":[]}` {
		t.Fatalf("histogram body = %q", got)
	}

	// An aggregation over groups that never participate in any window likewise
	// returns an empty array, not a zero group.
	rec = getQueryRange(t, h, queryRangeTarget(`sum by (zone)(lat{})`, base, now.Add(time.Hour), 60))
	if got := strings.TrimSpace(rec.Body.String()); got != `{"result_type":"matrix","result":[]}` {
		t.Fatalf("aggregate body = %q", got)
	}
}

func TestQueryRangeQueryValidation(t *testing.T) {
	h := Handler()
	now := time.Now().UTC()
	start := now.Add(-time.Hour).Format(time.RFC3339Nano)
	end := now.Format(time.RFC3339Nano)
	expr := url.QueryEscape(`m{}`)
	good := queryRangePath + "?expr=" + expr + "&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=60"

	bad := []string{
		queryRangePath,                   // everything missing
		queryRangePath + "?expr=" + expr, // only expr
		queryRangePath + "?start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=60",      // no expr
		queryRangePath + "?expr=" + expr + "&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end), // no step
		good + "&step=60",                         // duplicate step
		good + "&expr=" + expr,                    // duplicate expr
		good + "&start=" + url.QueryEscape(start), // duplicate start
		good + "&bogus=1",                         // unknown parameter
		queryRangePath + "?expr=" + expr + "&start=nope&end=" + url.QueryEscape(end) + "&step=60",
		queryRangePath + "?expr=" + expr + "&start=" + url.QueryEscape(start) + "&end=not-a-time&step=60",
		queryRangePath + "?expr=" + expr + "&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(start) + "&step=60", // start == end
		queryRangePath + "?expr=" + expr + "&start=" + url.QueryEscape(end) + "&end=" + url.QueryEscape(start) + "&step=60",   // start > end
		queryRangePath + "?expr=" + expr + "&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=0",
		queryRangePath + "?expr=" + expr + "&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=3601",
		queryRangePath + "?expr=" + expr + "&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=1.5",
		queryRangePath + "?expr=" + expr + "&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end) + "&step=",
		// 10001 windows of 1s.
		queryRangePath + "?expr=" + expr + "&start=" + url.QueryEscape(start) +
			"&end=" + url.QueryEscape(now.Add(10001*time.Second-time.Hour).Format(time.RFC3339Nano)) + "&step=1",
	}
	for _, target := range bad {
		expectErrorCode(t, getQueryRange(t, h, target), http.StatusBadRequest, "invalid_query_range")
	}

	// Exactly 10000 windows is accepted; no data yields an empty matrix.
	ok := queryRangePath + "?expr=" + expr + "&start=" + url.QueryEscape(start) +
		"&end=" + url.QueryEscape(now.Add(10000*time.Second-time.Hour).Format(time.RFC3339Nano)) + "&step=1"
	rec := getQueryRange(t, h, ok)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"result_type":"matrix","result":[]}` {
		t.Fatalf("10000-window response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestQueryRangeInvalidExpressions(t *testing.T) {
	h := Handler()
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	end := now

	bad := []string{
		``,
		`hits`,
		`1hits{}`,
		`hits{route="z1"`,
		`hits{route=z1}`,
		`hits{route="a",route="b"}`, // duplicate matcher key
		`hits{bad-key="a"}`,
		`sum(hits{}`,
		`SUM(hits{})`,
		`foo(hits{})`,
		`sum(sum(hits{}))`,
		`sum by ()(hits{})`,
		`sum by (a,a)(hits{})`, // duplicate grouping key
		`sum by (a)(hits{}) x`,
	}
	for _, expr := range bad {
		rec := getQueryRange(t, h, queryRangeTarget(expr, start, end, 60))
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_expression")
	}
}

func TestQueryRangeNonFiniteDataIs422(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	// Two finite increments in the same window overflow the window sum.
	body := fmt.Sprintf(`{"samples":[
		{"name":"big","type":"counter","labels":{},"value":1e308,"timestamp":%q},
		{"name":"big","type":"counter","labels":{},"value":1e308,"timestamp":%q}
	]}`, ts(base), ts(base.Add(time.Second)))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	expectErrorCode(t, getQueryRange(t, h, queryRangeTarget(`big{}`, base, base.Add(time.Minute), 60)),
		http.StatusUnprocessableEntity, "invalid_query_range_data")
	expectErrorCode(t, getQueryRange(t, h, queryRangeTarget(`sum(big{})`, base, base.Add(time.Minute), 60)),
		http.StatusUnprocessableEntity, "invalid_query_range_data")
}

func TestQueryRangeMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, queryRangePath, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Fatalf("%s Allow = %q, want GET", method, got)
		}
		expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

func TestQueryRangeRetentionTrim(t *testing.T) {
	h := Handler()
	old := time.Now().Add(-24*time.Hour + 5*time.Minute).UTC()
	recent := time.Now().Add(-time.Minute).UTC()
	body := fmt.Sprintf(`{"samples":[
		{"name":"m","type":"counter","labels":{},"value":1,"timestamp":%q},
		{"name":"m","type":"counter","labels":{},"value":2,"timestamp":%q}
	]}`, ts(old), ts(recent))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	// A start before the retention boundary is not an error; expired samples
	// are simply ignored.
	start := old.Add(-time.Hour).Truncate(time.Minute)
	rec := getQueryRange(t, h, queryRangeTarget(`m{}`, start, time.Now().Add(time.Minute), 3600))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	got := expectMatrix(t, rec)
	if len(got) != 1 {
		t.Fatalf("series len = %d, want 1", len(got))
	}
	if points := pointsOf(got[0]); len(points) != 2 {
		t.Fatalf("points len = %d, want 2 retained samples", len(points))
	}
}

func TestQueryRangeTimezoneNormalization(t *testing.T) {
	h := Handler()
	east := time.FixedZone("plus8", 8*3600)
	when := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[{"name":"m","type":"gauge","labels":{},"value":8,"timestamp":%q}]}`,
		when.In(east).Format(time.RFC3339Nano))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	start := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	// Parameters may carry any zone; window timestamps come back as UTC.
	target := queryRangePath + "?expr=" + url.QueryEscape(`m{}`) +
		"&start=" + url.QueryEscape(start.In(east).Format(time.RFC3339Nano)) +
		"&end=" + url.QueryEscape(time.Now().Add(time.Hour).In(east).Format(time.RFC3339Nano)) + "&step=600"
	rec := getQueryRange(t, h, target)
	got := expectMatrix(t, rec)
	if len(got) != 1 || len(pointsOf(got[0])) == 0 {
		t.Fatalf("expected points, got %v", got)
	}
	for _, p := range pointsOf(got[0]) {
		stamp := p["timestamp"].(string)
		if !strings.HasSuffix(stamp, "Z") {
			t.Fatalf("window timestamp %q not normalized to UTC", stamp)
		}
	}
}

func TestQueryRangeTenantIsolation(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[{"name":"m","type":"counter","labels":{},"value":5,"timestamp":%q}]}`, ts(base))

	req := httptest.NewRequest(http.MethodPost, metricsPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(tenantHeader, "alpha")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	target := queryRangeTarget(`m{}`, base, base.Add(time.Minute), 60)
	for _, tenant := range []string{"", "beta"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if tenant != "" {
			req.Header.Set(tenantHeader, tenant)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := expectMatrix(t, rec); len(got) != 0 {
			t.Fatalf("tenant %q saw %v", tenant, got)
		}
	}

	req = httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(tenantHeader, "alpha")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := expectMatrix(t, rec); len(got) != 1 {
		t.Fatalf("alpha result = %v", got)
	}

	// Tenant validation precedes expression and parameter validation.
	req = httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(tenantHeader, "Bad Tenant")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
}
