package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func runQuery(t *testing.T, h http.Handler, expr, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	target := queryPath + "?expr=" + url.QueryEscape(expr)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if tenant != "" {
		req.Header.Set(tenantHeader, tenant)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func runRawQuery(t *testing.T, h http.Handler, rawQuery, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, queryPath+"?"+rawQuery, nil)
	if tenant != "" {
		req.Header.Set(tenantHeader, tenant)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func expectVector(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
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
	if payload.ResultType != "vector" {
		t.Fatalf("result_type = %q, want vector", payload.ResultType)
	}
	return payload.Result
}

func seedQuerySeries(t *testing.T, h http.Handler) {
	t.Helper()
	body := `{"samples":[
		{"name":"hits","type":"counter","labels":{"route":"/a","zone":"z1"},"value":3},
		{"name":"hits","type":"counter","labels":{"route":"/b","zone":"z1"},"value":5},
		{"name":"hits","type":"counter","labels":{"route":"/c","zone":"z2"},"value":7},
		{"name":"temp","type":"gauge","labels":{"zone":"z1"},"value":10},
		{"name":"hits","type":"histogram","labels":{"route":"/d","zone":"z1"},"value":1,"buckets":[1,2]}
	]}`
	rec := postMetrics(t, h, body, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("seed failed: %d %s", rec.Code, rec.Body.String())
	}
}

func sampleLabels(sample map[string]any) map[string]any {
	labels, _ := sample["labels"].(map[string]any)
	return labels
}

func sampleValue(sample map[string]any) float64 {
	v, _ := sample["value"].(float64)
	return v
}

func TestQueryPlainSelector(t *testing.T) {
	h := Handler()
	seedQuerySeries(t, h)

	// Full label sets, sorted by the normalized full-label ordering.
	got := expectVector(t, runQuery(t, h, `hits{zone="z1"}`, ""))
	if len(got) != 2 {
		t.Fatalf("got %d samples: %v", len(got), got)
	}
	if sampleLabels(got[0])["route"] != "/a" || sampleValue(got[0]) != 3 {
		t.Fatalf("first sample = %v", got[0])
	}
	if sampleLabels(got[1])["route"] != "/b" || sampleValue(got[1]) != 5 {
		t.Fatalf("second sample = %v", got[1])
	}

	// The histogram never participates.
	got = expectVector(t, runQuery(t, h, `hits{route="/d"}`, ""))
	if len(got) != 0 {
		t.Fatalf("histogram must not match, got %v", got)
	}

	// Empty selector is legal and matches every series of the metric.
	got = expectVector(t, runQuery(t, h, `hits{}`, ""))
	if len(got) != 3 {
		t.Fatalf("empty selector matches %d series, want 3: %v", len(got), got)
	}

	// Gauges participate as well.
	got = expectVector(t, runQuery(t, h, `temp{}`, ""))
	if len(got) != 1 || sampleValue(got[0]) != 10 {
		t.Fatalf("gauge query = %v", got)
	}
}

func TestQueryUnknownMetricIsEmpty(t *testing.T) {
	h := Handler()
	if got := expectVector(t, runQuery(t, h, `missing{}`, "")); len(got) != 0 {
		t.Fatalf("want empty result, got %v", got)
	}
}

func TestQueryJSONStringEscaping(t *testing.T) {
	h := Handler()
	body := `{"samples":[
		{"name":"hits","type":"counter","labels":{"k":"a\"b\\c","zone":"中"},"value":4}
	]}`
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("seed failed: %d %s", rec.Code, rec.Body.String())
	}
	got := expectVector(t, runQuery(t, h, `hits{k="a\"b\\c",zone="中"}`, ""))
	if len(got) != 1 || sampleValue(got[0]) != 4 {
		t.Fatalf("escaped selector result = %v", got)
	}
}

func TestQueryAggregationsWithoutBy(t *testing.T) {
	h := Handler()
	seedQuerySeries(t, h)

	cases := []struct {
		expr string
		want float64
	}{
		{`sum(hits{zone="z1"})`, 8},
		{`avg(hits{zone="z1"})`, 4},
		{`min(hits{zone="z1"})`, 3},
		{`max(hits{zone="z1"})`, 5},
	}
	for _, tc := range cases {
		got := expectVector(t, runQuery(t, h, tc.expr, ""))
		if len(got) != 1 {
			t.Fatalf("%s: want single group, got %v", tc.expr, got)
		}
		if len(sampleLabels(got[0])) != 0 {
			t.Fatalf("%s: group labels = %v, want {}", tc.expr, sampleLabels(got[0]))
		}
		if sampleValue(got[0]) != tc.want {
			t.Fatalf("%s: value = %v, want %v", tc.expr, sampleValue(got[0]), tc.want)
		}
	}

	// No numeric series: empty result, never a zero.
	if got := expectVector(t, runQuery(t, h, `sum(hits{route="/d"})`, "")); len(got) != 0 {
		t.Fatalf("histogram-only aggregate = %v, want empty", got)
	}
}

func TestQueryAggregationWithBy(t *testing.T) {
	h := Handler()
	seedQuerySeries(t, h)

	got := expectVector(t, runQuery(t, h, `sum by (zone)(hits{})`, ""))
	if len(got) != 2 {
		t.Fatalf("want 2 zone groups, got %v", got)
	}
	if sampleLabels(got[0])["zone"] != "z1" || sampleValue(got[0]) != 8 {
		t.Fatalf("z1 group = %v", got[0])
	}
	if sampleLabels(got[1])["zone"] != "z2" || sampleValue(got[1]) != 7 {
		t.Fatalf("z2 group = %v", got[1])
	}

	// avg divides by the number of participating series.
	got = expectVector(t, runQuery(t, h, `avg by (zone)(hits{zone="z1"})`, ""))
	if len(got) != 1 || sampleValue(got[0]) != 4 {
		t.Fatalf("avg by = %v", got)
	}

	// min/max fold only within each group.
	got = expectVector(t, runQuery(t, h, `min by (zone)(hits{})`, ""))
	if len(got) != 2 || sampleValue(got[0]) != 3 || sampleValue(got[1]) != 7 {
		t.Fatalf("min by zone = %v", got)
	}
}

func TestQueryGroupLabelAbsentProjectsAway(t *testing.T) {
	h := Handler()
	body := `{"samples":[
		{"name":"m","type":"gauge","labels":{"route":"a","zone":"z1"},"value":2},
		{"name":"m","type":"gauge","labels":{"route":"b"},"value":9}
	]}`
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("seed failed: %d %s", rec.Code, rec.Body.String())
	}
	got := expectVector(t, runQuery(t, h, `sum by (zone)(m{})`, ""))
	if len(got) != 2 {
		t.Fatalf("want 2 groups, got %v", got)
	}
	// Empty-label group sorts first under the normalized label ordering.
	if len(sampleLabels(got[0])) != 0 || sampleValue(got[0]) != 9 {
		t.Fatalf("missing-label group = %v", got[0])
	}
	if sampleLabels(got[1])["zone"] != "z1" || sampleValue(got[1]) != 2 {
		t.Fatalf("z1 group = %v", got[1])
	}
}

func TestQueryMetricNamedLikeAggregate(t *testing.T) {
	h := Handler()
	body := `{"samples":[
		{"name":"sum","type":"gauge","labels":{"a":"1"},"value":6},
		{"name":"sum","type":"gauge","labels":{"a":"2"},"value":10}
	]}`
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("seed failed: %d %s", rec.Code, rec.Body.String())
	}
	// A keyword followed by "{" is a bare selector on a metric named "sum".
	got := expectVector(t, runQuery(t, h, `sum{a="1"}`, ""))
	if len(got) != 1 || sampleValue(got[0]) != 6 {
		t.Fatalf("keyword-named metric selector = %v", got)
	}
	got = expectVector(t, runQuery(t, h, `sum(sum{})`, ""))
	if len(got) != 1 || sampleValue(got[0]) != 16 {
		t.Fatalf("aggregate over keyword-named metric = %v", got)
	}
}

func TestQueryWhitespaceAroundPunctuation(t *testing.T) {
	h := Handler()
	seedQuerySeries(t, h)
	got := expectVector(t, runQuery(t, h, "  sum\tby\n( zone ,\troute )(  hits {\nzone = \"z1\" } )  ", ""))
	if len(got) != 2 {
		t.Fatalf("whitespace-tolerant parse = %v", got)
	}
}

func TestQueryNonFiniteDataIs422(t *testing.T) {
	h := Handler()
	// Two finite counter writes overflow the cumulative current value to +Inf.
	for i := 0; i < 2; i++ {
		rec := postMetrics(t, h, `{"samples":[{"name":"big","type":"counter","labels":{},"value":1e308}]}`, "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("seed %d failed: %d %s", i, rec.Code, rec.Body.String())
		}
	}

	expectErrorCode(t, runQuery(t, h, `big{}`, ""), http.StatusUnprocessableEntity, "invalid_query_data")
	expectErrorCode(t, runQuery(t, h, `sum(big{})`, ""), http.StatusUnprocessableEntity, "invalid_query_data")
}

func TestQueryInvalidExpressions(t *testing.T) {
	h := Handler()
	seedQuerySeries(t, h)

	bad := []string{
		``,
		`hits`,                      // missing selector
		`1hits{}`,                   // bad metric identifier
		`hits{route="z1"`,           // unterminated selector
		`hits{route=z1}`,            // unquoted value
		`hits{route="a"!}`,          // stray character in matcher list
		`hits{route="a",route="b"}`, // duplicate matcher key
		`hits{bad-key="a"}`,         // bad label key
		`hits{route="\x"}`,          // invalid JSON escape
		`sum(hits{}`,                // unclosed call
		`SUM(hits{})`,               // keywords are case-sensitive
		`foo(hits{})`,               // unknown function
		`sum(sum(hits{}))`,          // nested aggregation
		`sum by ()(hits{})`,         // empty grouping list
		`sum by (a,a)(hits{})`,      // duplicate grouping labels
		`sum by a(hits{})`,          // missing parentheses around by list
		`sum by (a)(hits{}) x`,      // trailing content
		"hits{}`",                   // trailing punctuation
		"   ",                       // whitespace only
	}
	for _, expr := range bad {
		expectErrorCode(t, runQuery(t, h, expr, ""), http.StatusBadRequest, "invalid_expression")
	}

	// Missing parameter.
	expectErrorCode(t, runRawQuery(t, h, "", ""), http.StatusBadRequest, "invalid_expression")
	// Duplicate expr parameter.
	expectErrorCode(t, runRawQuery(t, h,
		"expr="+url.QueryEscape(`hits{}`)+"&expr="+url.QueryEscape(`hits{}`), ""),
		http.StatusBadRequest, "invalid_expression")
	// Any other query parameter is rejected.
	expectErrorCode(t, runRawQuery(t, h, "expr="+url.QueryEscape(`hits{}`)+"&other=1", ""),
		http.StatusBadRequest, "invalid_expression")
	// Empty expr value.
	expectErrorCode(t, runRawQuery(t, h, "expr=", ""), http.StatusBadRequest, "invalid_expression")
}

func TestQueryMethodNotAllowed(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, queryPath, nil))
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", got)
	}
}

func TestQueryTenantIsolationAndPrecedence(t *testing.T) {
	h := Handler()
	seedQuerySeries(t, h) // default tenant

	if got := expectVector(t, runQuery(t, h, `hits{}`, "other_tenant")); len(got) != 0 {
		t.Fatalf("tenant isolation leaked series: %v", got)
	}
	// invalid_tenant is decided before the expression is examined.
	expectErrorCode(t, runRawQuery(t, h, "expr=notanexpr", "Bad Tenant"),
		http.StatusBadRequest, "invalid_tenant")
}
