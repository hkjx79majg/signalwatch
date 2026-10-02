package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func putRule(t *testing.T, h http.Handler, id, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	if contentType == "" {
		contentType = "application/json"
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, alertRulesPath+"/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	h.ServeHTTP(rec, req)
	return rec
}

func doRequest(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func getAlerts(t *testing.T, h http.Handler) []any {
	t.Helper()
	rec := doRequest(t, h, http.MethodGet, alertsPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("alerts status = %d, body=%q", rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)["alerts"].([]any)
}

func TestRuleLifecycle(t *testing.T) {
	h := Handler()

	// Empty collection is a non-null array.
	rec := doRequest(t, h, http.MethodGet, alertRulesPath)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"rules":[]}` {
		t.Fatalf("empty rules = %d %q", rec.Code, rec.Body.String())
	}

	// Missing single rule: 404 on read and delete.
	expectErrorCode(t, doRequest(t, h, http.MethodGet, alertRulesPath+"/r1"), http.StatusNotFound, "rule_not_found")
	expectErrorCode(t, doRequest(t, h, http.MethodDelete, alertRulesPath+"/r1"), http.StatusNotFound, "rule_not_found")

	// Create.
	body := `{"kind":"threshold","metric":"hits","labels":{"route":"/a"},"operator":"gt","threshold":10}`
	rec = putRule(t, h, "r1", body, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%q", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)
	if created["id"] != "r1" || created["kind"] != "threshold" || created["metric"] != "hits" ||
		created["operator"] != "gt" || created["threshold"] != float64(10) {
		t.Fatalf("created rule = %v", created)
	}
	if _, ok := created["numerator"]; ok {
		t.Fatalf("threshold rule must not carry numerator: %v", created)
	}

	// Read back.
	rec = doRequest(t, h, http.MethodGet, alertRulesPath+"/r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	got := decodeBody(t, rec)
	if got["id"] != "r1" || got["labels"].(map[string]any)["route"] != "/a" {
		t.Fatalf("stored rule = %v", got)
	}

	// Replace is atomic and returns 200.
	replacement := `{"kind":"ratio","numerator":{"metric":"err","labels":{}},"denominator":{"metric":"tot","labels":{}},"operator":"gte","threshold":0.5}`
	rec = putRule(t, h, "r1", replacement, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body=%q", rec.Code, rec.Body.String())
	}
	got = decodeBody(t, rec)
	if got["kind"] != "ratio" {
		t.Fatalf("replaced rule = %v", got)
	}
	if _, ok := got["metric"]; ok {
		t.Fatalf("ratio rule must not carry metric: %v", got)
	}
	num := got["numerator"].(map[string]any)
	if num["metric"] != "err" {
		t.Fatalf("numerator = %v", num)
	}
	if labels, ok := num["labels"].(map[string]any); !ok || len(labels) != 0 {
		t.Fatalf("numerator labels = %v", num["labels"])
	}

	// Collection is sorted by id.
	putRule(t, h, "b_rule", body, "")
	putRule(t, h, "a_rule", body, "")
	rec = doRequest(t, h, http.MethodGet, alertRulesPath)
	rules := decodeBody(t, rec)["rules"].([]any)
	if len(rules) != 3 {
		t.Fatalf("rules len = %d, want 3", len(rules))
	}
	wantOrder := []string{"a_rule", "b_rule", "r1"}
	for i, want := range wantOrder {
		if rules[i].(map[string]any)["id"] != want {
			t.Fatalf("rules order = %v", rules)
		}
	}

	// Delete returns 204 with no body; a second delete 404s.
	rec = doRequest(t, h, http.MethodDelete, alertRulesPath+"/r1")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d body=%q", rec.Code, rec.Body.String())
	}
	expectErrorCode(t, doRequest(t, h, http.MethodDelete, alertRulesPath+"/r1"), http.StatusNotFound, "rule_not_found")
}

func TestRuleValidation(t *testing.T) {
	h := Handler()
	good := `{"kind":"threshold","metric":"m","labels":{},"operator":"lt","threshold":2}`
	if rec := putRule(t, h, "keep", good, ""); rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d", rec.Code)
	}

	cases := map[string]string{
		"not json":             `{`,
		"not an object":        `[1]`,
		"two values":           `{}{}`,
		"empty":                `{}`,
		"unknown field":        `{"kind":"threshold","metric":"m","labels":{},"operator":"lt","threshold":2,"x":1}`,
		"missing operator":     `{"kind":"threshold","metric":"m","labels":{},"threshold":2}`,
		"missing threshold":    `{"kind":"threshold","metric":"m","labels":{},"operator":"lt"}`,
		"missing kind":         `{"metric":"m","labels":{},"operator":"lt","threshold":2}`,
		"missing metric":       `{"kind":"threshold","labels":{},"operator":"lt","threshold":2}`,
		"missing labels":       `{"kind":"threshold","metric":"m","operator":"lt","threshold":2}`,
		"null labels":          `{"kind":"threshold","metric":"m","labels":null,"operator":"lt","threshold":2}`,
		"bad kind":             `{"kind":"summary","metric":"m","labels":{},"operator":"lt","threshold":2}`,
		"bad operator":         `{"kind":"threshold","metric":"m","labels":{},"operator":"eq","threshold":2}`,
		"string threshold":     `{"kind":"threshold","metric":"m","labels":{},"operator":"lt","threshold":"2"}`,
		"bad metric name":      `{"kind":"threshold","metric":"1m","labels":{},"operator":"lt","threshold":2}`,
		"bad label key":        `{"kind":"threshold","metric":"m","labels":{"a-b":"1"},"operator":"lt","threshold":2}`,
		"threshold with ratio": `{"kind":"threshold","metric":"m","labels":{},"numerator":{"metric":"a","labels":{}},"operator":"lt","threshold":2}`,
		"ratio with metric":    `{"kind":"ratio","metric":"m","numerator":{"metric":"a","labels":{}},"denominator":{"metric":"b","labels":{}},"operator":"lt","threshold":2}`,
		"ratio missing den":    `{"kind":"ratio","numerator":{"metric":"a","labels":{}},"operator":"lt","threshold":2}`,
		"ratio null num":       `{"kind":"ratio","numerator":null,"denominator":{"metric":"b","labels":{}},"operator":"lt","threshold":2}`,
		"num missing labels":   `{"kind":"ratio","numerator":{"metric":"a"},"denominator":{"metric":"b","labels":{}},"operator":"lt","threshold":2}`,
		"num bad metric":       `{"kind":"ratio","numerator":{"metric":"a b","labels":{}},"denominator":{"metric":"b","labels":{}},"operator":"lt","threshold":2}`,
		"num extra field":      `{"kind":"ratio","numerator":{"metric":"a","labels":{},"z":1},"denominator":{"metric":"b","labels":{}},"operator":"lt","threshold":2}`,
		"den not object":       `{"kind":"ratio","numerator":{"metric":"a","labels":{}},"denominator":"b","operator":"lt","threshold":2}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			expectErrorCode(t, putRule(t, h, "keep", body, ""), http.StatusBadRequest, "invalid_rule")
		})
	}

	// Bad id in path is an invalid rule, not a 404.
	expectErrorCode(t, putRule(t, h, "1bad", good, ""), http.StatusBadRequest, "invalid_rule")

	// Non-JSON media type.
	expectErrorCode(t, putRule(t, h, "other", good, "text/plain"), http.StatusUnsupportedMediaType, "unsupported_media_type")

	// The failed writes must not have touched the stored rule.
	rec := doRequest(t, h, http.MethodGet, alertRulesPath+"/keep")
	got := decodeBody(t, rec)
	if got["kind"] != "threshold" || got["metric"] != "m" || got["operator"] != "lt" || got["threshold"] != float64(2) {
		t.Fatalf("rule changed by invalid writes: %v", got)
	}
	// Empty selector labels must round-trip as an explicit empty object.
	if labels, ok := got["labels"].(map[string]any); !ok || len(labels) != 0 {
		t.Fatalf("labels = %v, want present empty object", got["labels"])
	}
	if len(getAlerts(t, h)) != 1 {
		t.Fatal("invalid writes created extra rules")
	}
}

func TestRuleMethodNotAllowed(t *testing.T) {
	h := Handler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := doRequest(t, h, method, alertRulesPath)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
			t.Fatalf("collection %s = %d Allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
		expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	}

	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodOptions} {
		rec := doRequest(t, h, method, alertRulesPath+"/x")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, PUT, DELETE" {
			t.Fatalf("item %s = %d Allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
		expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := doRequest(t, h, method, alertsPath)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
			t.Fatalf("alerts %s = %d Allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
		expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

func TestThresholdAlertStates(t *testing.T) {
	h := Handler()

	// No matching series yet -> no_data with null value.
	putRule(t, h, "high_hits", `{"kind":"threshold","metric":"hits","labels":{"route":"/a"},"operator":"gt","threshold":10}`, "")
	alerts := getAlerts(t, h)
	a := alerts[0].(map[string]any)
	if a["id"] != "high_hits" || a["state"] != "no_data" {
		t.Fatalf("alert = %v", a)
	}
	if v, ok := a["value"]; !ok || v != nil {
		t.Fatalf("no_data value = %v (present=%v), want explicit null", v, ok)
	}

	// Counter + gauge sums push the rule into firing, then a gauge overwrite
	// brings it back to inactive. Value is the summed value.
	postMetrics(t, h, `{"samples":[
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":6},
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":6},
		{"name":"hits","type":"gauge","labels":{"route":"/a","extra":"1"},"value":1},
		{"name":"hits","type":"counter","labels":{"route":"/b"},"value":100}
	]}`, "")
	a = getAlerts(t, h)[0].(map[string]any)
	if a["state"] != "firing" || a["value"] != float64(13) {
		t.Fatalf("firing alert = %v", a)
	}

	postMetrics(t, h, `{"samples":[{"name":"hits","type":"gauge","labels":{"route":"/a","extra":"1"},"value":-10}]}`, "")
	a = getAlerts(t, h)[0].(map[string]any)
	if a["state"] != "inactive" || a["value"] != float64(2) {
		t.Fatalf("inactive alert = %v", a)
	}
}

func TestHistogramOnlySelectorIsNoData(t *testing.T) {
	h := Handler()
	// Histograms never participate, even when the selector matches them.
	putRule(t, h, "r", `{"kind":"threshold","metric":"hits","labels":{"route":"/a"},"operator":"gt","threshold":1}`, "")
	postMetrics(t, h, `{"samples":[{"name":"hits","type":"histogram","labels":{"route":"/a"},"value":99,"buckets":[1,10]}]}`, "")
	a := getAlerts(t, h)[0].(map[string]any)
	if a["state"] != "no_data" || a["value"] != nil {
		t.Fatalf("histogram-only alert = %v, want no_data", a)
	}
}

func TestRatioAlertStates(t *testing.T) {
	h := Handler()
	putRule(t, h, "err_ratio", `{"kind":"ratio","numerator":{"metric":"err","labels":{}},"denominator":{"metric":"tot","labels":{}},"operator":"gte","threshold":0.5}`, "")

	// Both sides missing -> no_data.
	if got := getAlerts(t, h)[0].(map[string]any)["state"]; got != "no_data" {
		t.Fatalf("empty state = %v", got)
	}

	// Denominator sum of zero -> no_data even though both sides exist.
	postMetrics(t, h, `{"samples":[
		{"name":"err","type":"counter","labels":{},"value":1},
		{"name":"tot","type":"gauge","labels":{},"value":0}
	]}`, "")
	if got := getAlerts(t, h)[0].(map[string]any)["state"]; got != "no_data" {
		t.Fatalf("zero denominator state = %v", got)
	}

	// Quotient below the threshold -> inactive with the quotient as value.
	postMetrics(t, h, `{"samples":[{"name":"tot","type":"gauge","labels":{},"value":4}]}`, "")
	a := getAlerts(t, h)[0].(map[string]any)
	if a["state"] != "inactive" || a["value"] != 0.25 {
		t.Fatalf("ratio alert = %v", a)
	}

	// Quotient reaching the threshold -> firing (gte is inclusive).
	postMetrics(t, h, `{"samples":[{"name":"err","type":"counter","labels":{},"value":1}]}`, "")
	a = getAlerts(t, h)[0].(map[string]any)
	if a["state"] != "firing" || a["value"] != 0.5 {
		t.Fatalf("ratio alert = %v", a)
	}
}

func TestAlertsSortedAndReflectReplacements(t *testing.T) {
	h := Handler()
	putRule(t, h, "z_rule", `{"kind":"threshold","metric":"m","labels":{},"operator":"gt","threshold":1}`, "")
	putRule(t, h, "a_rule", `{"kind":"threshold","metric":"m","labels":{},"operator":"lt","threshold":1}`, "")
	postMetrics(t, h, `{"samples":[{"name":"m","type":"gauge","labels":{},"value":5}]}`, "")

	alerts := getAlerts(t, h)
	if len(alerts) != 2 {
		t.Fatalf("alerts len = %d", len(alerts))
	}
	first, second := alerts[0].(map[string]any), alerts[1].(map[string]any)
	if first["id"] != "a_rule" || second["id"] != "z_rule" {
		t.Fatalf("alerts order = %v", alerts)
	}
	if first["state"] != "inactive" || second["state"] != "firing" {
		t.Fatalf("states = %v %v", first["state"], second["state"])
	}

	// Replacing a rule changes evaluation; deleting removes it.
	putRule(t, h, "a_rule", `{"kind":"threshold","metric":"m","labels":{},"operator":"lte","threshold":5}`, "")
	if got := getAlerts(t, h)[0].(map[string]any)["state"]; got != "firing" {
		t.Fatalf("replaced rule state = %v", got)
	}
	doRequest(t, h, http.MethodDelete, alertRulesPath+"/a_rule")
	alerts = getAlerts(t, h)
	if len(alerts) != 1 || alerts[0].(map[string]any)["id"] != "z_rule" {
		t.Fatalf("alerts after delete = %v", alerts)
	}
}

func TestAlertsValueIsNullInJSON(t *testing.T) {
	h := Handler()
	putRule(t, h, "r", `{"kind":"threshold","metric":"missing","labels":{},"operator":"gt","threshold":1}`, "")
	rec := doRequest(t, h, http.MethodGet, alertsPath)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw["alerts"], &items); err != nil {
		t.Fatal(err)
	}
	if string(items[0]["value"]) != "null" {
		t.Fatalf("no_data value = %s, want null", items[0]["value"])
	}
}
