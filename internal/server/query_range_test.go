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

func runQueryRange(t *testing.T, h http.Handler, target, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if tenant != "" {
		req.Header.Set(tenantHeader, tenant)
	}
	h.ServeHTTP(rec, req)
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

func matrixPoints(series map[string]any) []map[string]any {
	raw := series["points"].([]any)
	out := make([]map[string]any, len(raw))
	for i, p := range raw {
		out[i] = p.(map[string]any)
	}
	return out
}

// TestQueryRangePlainSelector covers counter increments summed per window,
// empty windows emitting nothing, the half-open end boundary, gauge latest
// observation with commit-order tie break, histogram exclusion and canonical
// series ordering.
func TestQueryRangePlainSelector(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)

	body := fmt.Sprintf(`{"samples":[
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":3,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":4,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":5,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/b"},"value":2,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/b"},"value":9,"timestamp":%q},		{"name":"temp","type":"gauge","labels":{"route":"/a"},"value":1,"timestamp":%q},
		{"name":"temp","type":"gauge","labels":{"route":"/a"},"value":2,"timestamp":%q},
		{"name":"temp","type":"gauge","labels":{"route":"/a"},"value":3,"timestamp":%q},
		{"name":"hits","type":"histogram","labels":{"route":"/h"},"value":1,"buckets":[1,2],"timestamp":%q}
	]}`,
		ts(base.Add(5*time.Second)),   // hits /a w0
		ts(base.Add(10*time.Second)),  // hits /a w0 -> 7
		ts(base.Add(125*time.Second)), // hits /a w2 -> 5
		ts(base.Add(5*time.Second)),   // hits /b w0 -> 2
		ts(base.Add(180*time.Second)), // hits /b exactly at end: half-open excludes
		ts(base.Add(10*time.Second)),  // temp w0 (earlier ts, first commit)
		ts(base.Add(10*time.Second)),  // temp w0 same instant, later commit -> wins
		ts(base.Add(5*time.Second)),   // temp w0 earlier instant -> loses
		ts(base.Add(5*time.Second)),   // histogram: never participates
	)
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
	}

	// end = base+3m excludes the /b sample landing exactly on the boundary.
	target := queryRangeTarget(`hits{}`, base, base.Add(3*time.Minute), 60)
	got := expectMatrix(t, runQueryRange(t, h, target, ""))
	if len(got) != 2 {
		t.Fatalf("series len = %d, want 2 (histogram excluded): %v", len(got), got)
	}
	// Canonical full-label order: route=/a before route=/b.
	if got[0]["labels"].(map[string]any)["route"] != "/a" {
		t.Fatalf("series order = %v", got)
	}
	points := matrixPoints(got[0])
	if len(points) != 2 {
		t.Fatalf("/a points len = %d, want 2 (empty window w1 omitted): %v", len(points), points)
	}
	if points[0]["timestamp"] != ts(base) || points[0]["value"] != float64(7) {
		t.Fatalf("/a point 0 = %v, want {%s 7}", points[0], ts(base))
	}
	if points[1]["timestamp"] != ts(base.Add(2*time.Minute)) || points[1]["value"] != float64(5) {
		t.Fatalf("/a point 1 = %v, want {%s 5}", points[1], ts(base.Add(2*time.Minute)))
	}
	points = matrixPoints(got[1])
	if len(points) != 1 || points[0]["timestamp"] != ts(base) || points[0]["value"] != float64(2) {
		t.Fatalf("/b points = %v, want single {%s 2}", points, ts(base))
	}

	// Gauge: latest timestamp wins, later commit breaks equal-timestamp ties.
	target = queryRangeTarget(`temp{route="/a"}`, base, base.Add(time.Minute), 60)
	got = expectMatrix(t, runQueryRange(t, h, target, ""))
	if len(got) != 1 {
		t.Fatalf("gauge series len = %d, want 1", len(got))
	}
	points = matrixPoints(got[0])
	if len(points) != 1 || points[0]["value"] != float64(2) {
		t.Fatalf("gauge point = %v, want value 2", points)
	}
}

func TestQueryRangeAggregationPerWindow(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)

	// zone z1: series a in w0,w1; series b only in w1. zone z2: w0 only.
	body := fmt.Sprintf(`{"samples":[
		{"name":"m","type":"counter","labels":{"zone":"z1","id":"a"},"value":3,"timestamp":%q},
		{"name":"m","type":"counter","labels":{"zone":"z1","id":"a"},"value":4,"timestamp":%q},
		{"name":"m","type":"counter","labels":{"zone":"z1","id":"b"},"value":8,"timestamp":%q},
		{"name":"m","type":"counter","labels":{"zone":"z2","id":"c"},"value":5,"timestamp":%q}
	]}`,
		ts(base.Add(5*time.Second)),  // a w0 -> 3
		ts(base.Add(65*time.Second)), // a w1 -> 4
		ts(base.Add(70*time.Second)), // b w1 -> 8
		ts(base.Add(5*time.Second)),  // c w0 -> 5
	)
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
	}

	// No by: one label-less series. w0 sum=8, w1 sum=12; no zero fill in w2.
	target := queryRangeTarget(`sum(m{})`, base, base.Add(3*time.Minute), 60)
	got := expectMatrix(t, runQueryRange(t, h, target, ""))
	if len(got) != 1 || len(got[0]["labels"].(map[string]any)) != 0 {
		t.Fatalf("sum without by = %v, want single label-less series", got)
	}
	points := matrixPoints(got[0])
	if len(points) != 2 {
		t.Fatalf("sum points = %v, want 2 windows", points)
	}
	if points[0]["timestamp"] != ts(base) || points[0]["value"] != float64(8) {
		t.Fatalf("sum w0 = %v, want 8", points[0])
	}
	if points[1]["timestamp"] != ts(base.Add(time.Minute)) || points[1]["value"] != float64(12) {
		t.Fatalf("sum w1 = %v, want 12", points[1])
	}

	// avg divides by the series actually participating in each window:
	// w0 has only series a within z1 -> 3; w1 has a and b -> (4+8)/2 = 6.
	target = queryRangeTarget(`avg by (zone)(m{zone="z1"})`, base, base.Add(3*time.Minute), 60)
	got = expectMatrix(t, runQueryRange(t, h, target, ""))
	if len(got) != 1 {
		t.Fatalf("avg by zone = %v, want one z1 group", got)
	}
	if got[0]["labels"].(map[string]any)["zone"] != "z1" {
		t.Fatalf("avg group labels = %v", got[0]["labels"])
	}
	points = matrixPoints(got[0])
	if len(points) != 2 || points[0]["value"] != float64(3) || points[1]["value"] != float64(6) {
		t.Fatalf("avg points = %v, want w0=3 w1=6", points)
	}

	// min/max fold within each window.
	for _, tc := range []struct {
		expr string
		w0   float64
		w1   float64
	}{
		{`min(m{})`, 3, 4},
		{`max(m{})`, 5, 8},
	} {
		target = queryRangeTarget(tc.expr, base, base.Add(3*time.Minute), 60)
		got = expectMatrix(t, runQueryRange(t, h, target, ""))
		points = matrixPoints(got[0])
		if len(points) != 2 || points[0]["value"] != tc.w0 || points[1]["value"] != tc.w1 {
			t.Fatalf("%s points = %v, want w0=%v w1=%v", tc.expr, points, tc.w0, tc.w1)
		}
	}

	// by groups projected independently; groups are ordered canonically and
	// a group with no value in a window emits no point.
	target = queryRangeTarget(`sum by (zone)(m{})`, base, base.Add(3*time.Minute), 60)
	got = expectMatrix(t, runQueryRange(t, h, target, ""))
	if len(got) != 2 {
		t.Fatalf("by zone groups = %v, want 2", got)
	}
	if got[0]["labels"].(map[string]any)["zone"] != "z1" {
		t.Fatalf("group order = %v, want z1 first", got)
	}
	z1 := matrixPoints(got[0])
	if len(z1) != 2 || z1[0]["value"] != float64(3) || z1[1]["value"] != float64(12) {
		t.Fatalf("z1 points = %v, want 3 then 12", z1)
	}
	z2 := matrixPoints(got[1])
	if len(z2) != 1 || z2[0]["timestamp"] != ts(base) || z2[0]["value"] != float64(5) {
		t.Fatalf("z2 points = %v, want single w0=5", z2)
	}
}

func TestQueryRangeByMissingLabelProjectsAway(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[
		{"name":"m","type":"gauge","labels":{"zone":"z1"},"value":2,"timestamp":%q},
		{"name":"m","type":"gauge","labels":{},"value":9,"timestamp":%q}
	]}`, ts(base.Add(time.Second)), ts(base.Add(time.Second)))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}
	got := expectMatrix(t, runQueryRange(t, h,
		queryRangeTarget(`sum by (zone)(m{})`, base, base.Add(time.Minute), 60), ""))
	if len(got) != 2 {
		t.Fatalf("want 2 groups, got %v", got)
	}
	// Empty-label group sorts first.
	if len(got[0]["labels"].(map[string]any)) != 0 || matrixPoints(got[0])[0]["value"] != float64(9) {
		t.Fatalf("missing-label group = %v", got[0])
	}
	if got[1]["labels"].(map[string]any)["zone"] != "z1" || matrixPoints(got[1])[0]["value"] != float64(2) {
		t.Fatalf("z1 group = %v", got[1])
	}
}

func TestQueryRangeEmptyResult(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	// Unknown metric: empty array, never null.
	rec := runQueryRange(t, h, queryRangeTarget(`missing{}`, base, base.Add(time.Minute), 60), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"result_type":"matrix","result":[]}` {
		t.Fatalf("empty body = %q", got)
	}

	// Histogram-only selector yields nothing.
	body := fmt.Sprintf(`{"samples":[{"name":"h","type":"histogram","labels":{},"value":1,"buckets":[1],"timestamp":%q}]}`, ts(base.Add(time.Second)))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}
	rec = runQueryRange(t, h, queryRangeTarget(`sum(h{})`, base, base.Add(time.Minute), 60), "")
	if got := strings.TrimSpace(rec.Body.String()); got != `{"result_type":"matrix","result":[]}` {
		t.Fatalf("histogram aggregate body = %q", got)
	}
}

func TestQueryRangeTimestampsNormalizedToUTC(t *testing.T) {
	h := Handler()
	east := time.FixedZone("plus8", 8*3600)
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	stamp := base.Add(time.Second).In(east).Format(time.RFC3339Nano)
	body := fmt.Sprintf(`{"samples":[{"name":"m","type":"gauge","labels":{},"value":1,"timestamp":%q}]}`, stamp)
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body=%q", rec.Code, rec.Body.String())
	}
	got := expectMatrix(t, runQueryRange(t, h,
		queryRangeTarget(`m{}`, base, base.Add(time.Minute), 60), ""))
	points := matrixPoints(got[0])
	if len(points) != 1 {
		t.Fatalf("points = %v", points)
	}
	stampOut := points[0]["timestamp"].(string)
	if !strings.HasSuffix(stampOut, "Z") {
		t.Fatalf("timestamp %q not normalized to UTC", stampOut)
	}
	if _, err := time.Parse(time.RFC3339Nano, stampOut); err != nil {
		t.Fatalf("timestamp %q not RFC3339Nano: %v", stampOut, err)
	}
}

func TestQueryRangeValidation(t *testing.T) {
	h := Handler()
	now := time.Now().UTC()
	start := now.Add(-time.Hour).Format(time.RFC3339Nano)
	end := now.Format(time.RFC3339Nano)
	esc := url.QueryEscape
	goodExpr := esc(`m{}`)
	base := queryRangePath + "?expr=" + goodExpr + "&start=" + esc(start) + "&end=" + esc(end) + "&step=60"

	badRange := []string{
		queryRangePath, // everything missing
		queryRangePath + "?start=" + esc(start) + "&end=" + esc(end) + "&step=60",          // no expr
		queryRangePath + "?expr=" + goodExpr + "&end=" + esc(end) + "&step=60",             // no start
		queryRangePath + "?expr=" + goodExpr + "&start=" + esc(start) + "&step=60",         // no end
		queryRangePath + "?expr=" + goodExpr + "&start=" + esc(start) + "&end=" + esc(end), // no step
		base + "&step=60",          // duplicate step
		base + "&expr=" + goodExpr, // duplicate expr
		base + "&bogus=1",          // unknown parameter
		queryRangePath + "?expr=" + goodExpr + "&start=nope&end=" + esc(end) + "&step=60",                 // bad start
		queryRangePath + "?expr=" + goodExpr + "&start=" + esc(start) + "&end=nope&step=60",               // bad end
		queryRangePath + "?expr=" + goodExpr + "&start=" + esc(end) + "&end=" + esc(start) + "&step=60",   // start > end
		queryRangePath + "?expr=" + goodExpr + "&start=" + esc(start) + "&end=" + esc(start) + "&step=60", // start == end
		queryRangePath + "?expr=" + goodExpr + "&start=" + esc(start) + "&end=" + esc(end) + "&step=0",
		queryRangePath + "?expr=" + goodExpr + "&start=" + esc(start) + "&end=" + esc(end) + "&step=3601",
		queryRangePath + "?expr=" + goodExpr + "&start=" + esc(start) + "&end=" + esc(end) + "&step=1.5",
		// 10001 windows of 1s.
		queryRangePath + "?expr=" + goodExpr + "&start=" + esc(start) +
			"&end=" + esc(now.Add(10001*time.Second-time.Hour).Format(time.RFC3339Nano)) + "&step=1",
	}
	for _, target := range badRange {
		expectErrorCode(t, runQueryRange(t, h, target, ""), http.StatusBadRequest, "invalid_query_range")
	}

	// Expression problems keep the query endpoint's error code.
	badExpr := []string{
		``,
		`m`,
		`1m{}`,
		`m{a="1",a="2"}`,
		`foo(m{})`,
		`sum by (z,z)(m{})`,
		`sum(m{}) trailing`,
	}
	for _, expr := range badExpr {
		target := queryRangePath + "?expr=" + esc(expr) + "&start=" + esc(start) + "&end=" + esc(end) + "&step=60"
		expectErrorCode(t, runQueryRange(t, h, target, ""), http.StatusBadRequest, "invalid_expression")
	}

	// Exactly 10000 windows is accepted and yields an empty matrix.
	ok := queryRangePath + "?expr=" + goodExpr + "&start=" + esc(start) +
		"&end=" + esc(now.Add(10000*time.Second-time.Hour).Format(time.RFC3339Nano)) + "&step=1"
	rec := runQueryRange(t, h, ok, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"result_type":"matrix","result":[]}` {
		t.Fatalf("10000-window response = %d %q", rec.Code, rec.Body.String())
	}
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

func TestQueryRangeNonFiniteIs422(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	// Two finite increments overflow their window sum to +Inf.
	body := fmt.Sprintf(`{"samples":[
		{"name":"big","type":"counter","labels":{},"value":1e308,"timestamp":%q},
		{"name":"big","type":"counter","labels":{},"value":1e308,"timestamp":%q}
	]}`, ts(base.Add(time.Second)), ts(base.Add(2*time.Second)))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}
	target := queryRangeTarget(`big{}`, base, base.Add(time.Minute), 60)
	expectErrorCode(t, runQueryRange(t, h, target, ""), http.StatusUnprocessableEntity, "invalid_query_range_data")
	target = queryRangeTarget(`sum(big{})`, base, base.Add(time.Minute), 60)
	expectErrorCode(t, runQueryRange(t, h, target, ""), http.StatusUnprocessableEntity, "invalid_query_range_data")
}

func TestQueryRangeRetentionAndEarlyStart(t *testing.T) {
	h := Handler()
	old := time.Now().Add(-24*time.Hour + 5*time.Minute).UTC().Truncate(time.Second)
	recent := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[
		{"name":"m","type":"counter","labels":{},"value":1,"timestamp":%q},
		{"name":"m","type":"counter","labels":{},"value":2,"timestamp":%q}
	]}`, ts(old), ts(recent))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	// A start well before the retention boundary is not an error; retained
	// samples still land in windows cut from that start.
	start := old.Add(-2 * time.Hour).Truncate(time.Hour)
	got := expectMatrix(t, runQueryRange(t, h,
		queryRangeTarget(`m{}`, start, time.Now().Add(time.Hour), 3600), ""))
	if len(got) != 1 {
		t.Fatalf("series len = %d, want 1: %v", len(got), got)
	}
	points := matrixPoints(got[0])
	if len(points) != 2 {
		t.Fatalf("points len = %d, want 2 retained samples: %v", len(points), points)
	}
	kOld := int(old.Sub(start) / time.Hour)
	kRecent := int(recent.Sub(start) / time.Hour)
	if points[0]["timestamp"] != start.Add(time.Duration(kOld)*time.Hour).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("old point window start = %v", points[0])
	}
	if points[1]["timestamp"] != start.Add(time.Duration(kRecent)*time.Hour).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("recent point window start = %v", points[1])
	}
}

func TestQueryRangeTenantIsolationAndPrecedence(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[{"name":"m","type":"counter","labels":{},"value":5,"timestamp":%q}]}`, ts(base.Add(time.Second)))
	req := httptest.NewRequest(http.MethodPost, metricsPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(tenantHeader, "alpha")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d", rec.Code)
	}

	target := queryRangeTarget(`m{}`, base, base.Add(time.Minute), 60)
	if got := expectMatrix(t, runQueryRange(t, h, target, "")); len(got) != 0 {
		t.Fatalf("default tenant leaked: %v", got)
	}
	if got := expectMatrix(t, runQueryRange(t, h, target, "beta")); len(got) != 0 {
		t.Fatalf("beta tenant leaked: %v", got)
	}
	if got := expectMatrix(t, runQueryRange(t, h, target, "alpha")); len(got) != 1 {
		t.Fatalf("alpha should see its series: %v", got)
	}

	// invalid_tenant is decided before any query-parameter validation.
	req = httptest.NewRequest(http.MethodGet, queryRangePath, nil)
	req.Header.Set(tenantHeader, "Bad Tenant")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
}
