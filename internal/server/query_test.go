package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func getQuery(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func queryURL(expr string) string {
	return queryPath + "?expr=" + url.QueryEscape(expr)
}

func seedQueryData(t *testing.T, h http.Handler) {
	t.Helper()
	body := `{"samples":[
		{"name":"http_requests","type":"counter","labels":{"route":"/a","code":"200"},"value":3},
		{"name":"http_requests","type":"counter","labels":{"route":"/a","code":"500"},"value":1},
		{"name":"http_requests","type":"counter","labels":{"route":"/b","code":"200"},"value":6},
		{"name":"temp","type":"gauge","labels":{"room":"k"},"value":10},
		{"name":"temp","type":"gauge","labels":{"room":"k"},"value":4},
		{"name":"lat","type":"histogram","labels":{"route":"/a"},"value":2,"buckets":[1,5]}
	]}`
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("seed status = %d body=%q", rec.Code, rec.Body.String())
	}
}

func resultVector(t *testing.T, rec *httptest.ResponseRecorder) []any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)
	if payload["result_type"] != "vector" {
		t.Fatalf("result_type = %v", payload["result_type"])
	}
	result, ok := payload["result"].([]any)
	if !ok {
		t.Fatalf("result is not an array: %v", payload["result"])
	}
	return result
}

func TestQueryPlainSelector(t *testing.T) {
	h := Handler()
	seedQueryData(t, h)

	// Full labels and current values, sorted by normalized label order.
	result := resultVector(t, getQuery(t, h, queryURL(`http_requests{route="/a"}`)))
	if len(result) != 2 {
		t.Fatalf("result len = %d, want 2", len(result))
	}
	first := result[0].(map[string]any)
	second := result[1].(map[string]any)
	if first["labels"].(map[string]any)["code"] != "200" || first["value"] != 3.0 {
		t.Fatalf("first entry = %v", first)
	}
	if second["labels"].(map[string]any)["code"] != "500" || second["value"] != 1.0 {
		t.Fatalf("second entry = %v", second)
	}

	// Bare metric name and empty braces both select every series; histograms
	// never participate.
	result = resultVector(t, getQuery(t, h, queryURL(`http_requests`)))
	if len(result) != 3 {
		t.Fatalf("bare selector len = %d, want 3", len(result))
	}
	result = resultVector(t, getQuery(t, h, queryURL(`lat{}`)))
	if len(result) != 0 {
		t.Fatalf("histogram selector len = %d, want 0", len(result))
	}

	// Gauge keeps only the latest value; JSON escapes decode in label values.
	result = resultVector(t, getQuery(t, h, queryURL(`temp{room="k"}`)))
	if len(result) != 1 || result[0].(map[string]any)["value"] != 4.0 {
		t.Fatalf("gauge result = %v", result)
	}

	// No match is 200 with an empty (non-null) array.
	rec := getQuery(t, h, queryURL(`missing_metric`))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"result_type":"vector","result":[]}` {
		t.Fatalf("no-match response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestQueryAggregations(t *testing.T) {
	h := Handler()
	seedQueryData(t, h)

	// Ungrouped aggregation yields one group with empty labels.
	result := resultVector(t, getQuery(t, h, queryURL(`sum(http_requests)`)))
	if len(result) != 1 {
		t.Fatalf("sum len = %d, want 1", len(result))
	}
	entry := result[0].(map[string]any)
	if entry["value"] != 10.0 || len(entry["labels"].(map[string]any)) != 0 {
		t.Fatalf("sum entry = %v", entry)
	}

	result = resultVector(t, getQuery(t, h, queryURL(`avg(http_requests)`)))
	if result[0].(map[string]any)["value"] != 10.0/3.0 {
		t.Fatalf("avg = %v", result[0])
	}
	result = resultVector(t, getQuery(t, h, queryURL(`min(http_requests)`)))
	if result[0].(map[string]any)["value"] != 1.0 {
		t.Fatalf("min = %v", result[0])
	}
	result = resultVector(t, getQuery(t, h, queryURL(`max(http_requests)`)))
	if result[0].(map[string]any)["value"] != 6.0 {
		t.Fatalf("max = %v", result[0])
	}

	// Grouped aggregation projects labels and sorts groups.
	result = resultVector(t, getQuery(t, h, queryURL(`sum by (route)(http_requests)`)))
	if len(result) != 2 {
		t.Fatalf("grouped len = %d, want 2", len(result))
	}
	a := result[0].(map[string]any)
	b := result[1].(map[string]any)
	if a["labels"].(map[string]any)["route"] != "/a" || a["value"] != 4.0 {
		t.Fatalf("group /a = %v", a)
	}
	if b["labels"].(map[string]any)["route"] != "/b" || b["value"] != 6.0 {
		t.Fatalf("group /b = %v", b)
	}

	// Multi-label grouping, whitespace around punctuation, and a selector.
	result = resultVector(t, getQuery(t, h, queryURL(`sum by ( route , code ) ( http_requests{code="200"} )`)))
	if len(result) != 2 {
		t.Fatalf("multi-label len = %d, want 2", len(result))
	}
	for _, item := range result {
		labels := item.(map[string]any)["labels"].(map[string]any)
		if labels["code"] != "200" || len(labels) != 2 {
			t.Fatalf("grouped labels = %v", labels)
		}
	}

	// Aggregation with no matching series is an empty result, not zero.
	result = resultVector(t, getQuery(t, h, queryURL(`sum(nope)`)))
	if len(result) != 0 {
		t.Fatalf("empty aggregation = %v", result)
	}
}

func TestQueryInvalidExpressions(t *testing.T) {
	h := Handler()
	seedQueryData(t, h)

	badExprs := []string{
		``,                            // empty
		`1m`,                          // bad metric ident
		`m{1k="v"}`,                   // bad label key
		`m{k=v}`,                      // unquoted value
		`m{k="v"`,                     // unclosed braces
		`m{k="v",}`,                   // trailing comma
		`m{k="1",k="2"}`,              // duplicate matcher key
		`m{} trailing`,                // trailing content
		`sum(m) extra`,                // trailing content after aggregation
		`sum(`,                        // unclosed call
		`sum()`,                       // missing selector
		`sum(sum(m))`,                 // nested aggregation
		`count(m)`,                    // unknown function
		`SUM(m)`,                      // keywords are case-sensitive
		`sum By (route)(m)`,           // by is case-sensitive
		`sum by ()(m)`,                // empty grouping list
		`sum by (route,route)(m)`,     // duplicate grouping label
		`sum by (route)(m`,            // unclosed selector arg
		`avg by (route http_requests`, // garbage in grouping list
		`m{k="v"x}`,                   // junk after matcher
		`m{k="v`,                      // unterminated string
		`m{k="v\q"}`,                  // invalid JSON escape
	}
	for _, expr := range badExprs {
		t.Run(fmt.Sprintf("expr %q", expr), func(t *testing.T) {
			expectErrorCode(t, getQuery(t, h, queryURL(expr)), http.StatusBadRequest, "invalid_expression")
		})
	}

	// Parameter-shape errors: missing, duplicated or unexpected parameters.
	for _, target := range []string{
		queryPath,
		queryPath + "?expr=m&expr=m",
		queryPath + "?expr=m&other=1",
		queryPath + "?other=1",
		queryPath + "?expr=%zz",
	} {
		t.Run(fmt.Sprintf("target %q", target), func(t *testing.T) {
			expectErrorCode(t, getQuery(t, h, target), http.StatusBadRequest, "invalid_expression")
		})
	}
}

func TestQueryMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, queryPath+"?expr=m", nil))
		expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Fatalf("%s Allow = %q, want GET", method, got)
		}
	}
}

func TestQueryNonFiniteData(t *testing.T) {
	h := Handler()
	// Counter overflow: MaxFloat64 + MaxFloat64 accumulates to +Inf.
	big := `{"samples":[{"name":"big","type":"counter","labels":{},"value":1.7976931348623157e308}]}`
	for i := 0; i < 2; i++ {
		if rec := postMetrics(t, h, big, ""); rec.Code != http.StatusAccepted {
			t.Fatalf("seed status = %d", rec.Code)
		}
	}
	expectErrorCode(t, getQuery(t, h, queryURL(`big`)), http.StatusUnprocessableEntity, "invalid_query_data")
	expectErrorCode(t, getQuery(t, h, queryURL(`sum(big)`)), http.StatusUnprocessableEntity, "invalid_query_data")
}

func TestQueryTenantIsolation(t *testing.T) {
	h := Handler()
	body := `{"samples":[{"name":"m","type":"gauge","labels":{},"value":5}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, metricsPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-SignalWatch-Tenant", "alpha")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("seed status = %d", rec.Code)
	}

	// The default tenant does not see alpha's series.
	result := resultVector(t, getQuery(t, h, queryURL(`m`)))
	if len(result) != 0 {
		t.Fatalf("default tenant result = %v", result)
	}

	// The owning tenant does.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, queryURL(`sum(m)`), nil)
	req.Header.Set("X-SignalWatch-Tenant", "alpha")
	h.ServeHTTP(rec, req)
	result = resultVector(t, rec)
	if len(result) != 1 || result[0].(map[string]any)["value"] != 5.0 {
		t.Fatalf("alpha result = %v", result)
	}

	// invalid_tenant still takes precedence over any expression error.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, queryPath, nil)
	req.Header.Set("X-SignalWatch-Tenant", "bad tenant!")
	h.ServeHTTP(rec, req)
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
}
