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

func ruleRequest(t *testing.T, h http.Handler, method, id, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	target := alertRulesPath + "/" + id
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func putRule(t *testing.T, h http.Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return ruleRequest(t, h, http.MethodPut, id, body, "application/json")
}

func getAlerts(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, alertsPath, nil))
	return rec
}

func TestAlertRuleLifecycle(t *testing.T) {
	h := Handler()

	body := `{"kind":"threshold","operator":"gt","threshold":3,"metric":"hits","labels":{"route":"/a"}}`
	rec := putRule(t, h, "high_hits", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%q", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)
	if created["id"] != "high_hits" || created["kind"] != "threshold" || created["operator"] != "gt" ||
		created["threshold"] != float64(3) || created["metric"] != "hits" ||
		created["labels"].(map[string]any)["route"] != "/a" {
		t.Fatalf("unexpected create payload: %v", created)
	}

	// Replace atomically: same id, different content -> 200.
	body2 := `{"kind":"threshold","operator":"lte","threshold":9,"metric":"other","labels":{}}`
	rec = putRule(t, h, "high_hits", body2)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["metric"]; got != "other" {
		t.Fatalf("replaced rule = %v", rec.Body.String())
	}

	// GET returns the replacement.
	rec = ruleRequest(t, h, http.MethodGet, "high_hits", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	if got := decodeBody(t, rec)["threshold"]; got != float64(9) {
		t.Fatalf("get rule = %v", rec.Body.String())
	}

	// Collection sorted by id.
	rec2 := putRule(t, h, "alpha", `{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{}}`)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("second create status = %d", rec2.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, alertRulesPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	rules := decodeBody(t, rec)["rules"].([]any)
	if len(rules) != 2 || rules[0].(map[string]any)["id"] != "alpha" ||
		rules[1].(map[string]any)["id"] != "high_hits" {
		t.Fatalf("rules order = %v", rules)
	}

	// DELETE then 404s.
	rec = ruleRequest(t, h, http.MethodDelete, "high_hits", "", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d body=%q", rec.Code, rec.Body.String())
	}
	expectErrorCode(t, ruleRequest(t, h, http.MethodGet, "high_hits", "", ""), http.StatusNotFound, "rule_not_found")
	expectErrorCode(t, ruleRequest(t, h, http.MethodDelete, "high_hits", "", ""), http.StatusNotFound, "rule_not_found")
}

func TestAlertRuleEmptyCollection(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, alertRulesPath, nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"rules":[]}` {
		t.Fatalf("empty rules = %d %q", rec.Code, rec.Body.String())
	}
	rec = getAlerts(t, h)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"alerts":[]}` {
		t.Fatalf("empty alerts = %d %q", rec.Code, rec.Body.String())
	}
}

func TestThresholdAlertEvaluation(t *testing.T) {
	h := Handler()
	seed := `{"samples":[
		{"name":"hits","type":"counter","labels":{"route":"/a","zone":"z1"},"value":3},
		{"name":"hits","type":"counter","labels":{"route":"/b"},"value":7},
		{"name":"hits","type":"gauge","labels":{"route":"/a"},"value":2},
		{"name":"lat","type":"histogram","labels":{"route":"/a"},"value":1,"buckets":[1,2]}
	]}`
	if rec := postMetrics(t, h, seed, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("seed = %d", rec.Code)
	}

	// Sums counters and gauges matching all labels: 3 + 2 = 5.
	rec := putRule(t, h, "r_sum", `{"kind":"threshold","operator":"gte","threshold":5,"metric":"hits","labels":{"route":"/a"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %q", rec.Code, rec.Body.String())
	}
	// Selector matches a histogram only -> no_data.
	putRule(t, h, "r_hist", `{"kind":"threshold","operator":"gt","threshold":0,"metric":"lat","labels":{"route":"/a"}}`)
	// No series at all -> no_data.
	putRule(t, h, "r_missing", `{"kind":"threshold","operator":"gt","threshold":0,"metric":"nope","labels":{}}`)
	// All hits series: 3+7+2 = 12, gt 100 -> inactive with value 12.
	putRule(t, h, "r_all", `{"kind":"threshold","operator":"gt","threshold":100,"metric":"hits","labels":{}}`)

	alerts := alertsByID(t, getAlerts(t, h))
	assertAlert := func(id, state string, wantVal any) {
		t.Helper()
		a, ok := alerts[id].(map[string]any)
		if !ok {
			t.Fatalf("alert %q missing: %v", id, alerts)
		}
		if a["state"] != state {
			t.Fatalf("alert %s state = %v, want %s", id, a["state"], state)
		}
		if wantVal == nil {
			if a["value"] != nil {
				t.Fatalf("alert %s value = %v, want null", id, a["value"])
			}
		} else if a["value"] != wantVal {
			t.Fatalf("alert %s value = %v, want %v", id, a["value"], wantVal)
		}
	}
	assertAlert("r_sum", "firing", float64(5))
	assertAlert("r_hist", "no_data", nil)
	assertAlert("r_missing", "no_data", nil)
	assertAlert("r_all", "inactive", float64(12))

	// Ordering: r_all, r_hist, r_missing, r_sum.
	gotOrder := alertIDs(t, getAlerts(t, h))
	want := []string{"r_all", "r_hist", "r_missing", "r_sum"}
	if fmt.Sprint(gotOrder) != fmt.Sprint(want) {
		t.Fatalf("alert order = %v, want %v", gotOrder, want)
	}
}

func TestRatioAlertEvaluation(t *testing.T) {
	h := Handler()
	postMetrics(t, h, `{"samples":[
		{"name":"ok","type":"counter","labels":{},"value":8},
		{"name":"bad","type":"counter","labels":{},"value":2},
		{"name":"zero","type":"gauge","labels":{},"value":0}
	]}`, "")

	body := `{"kind":"ratio","operator":"lt","threshold":0.5,
		"numerator":{"metric":"bad","labels":{}},
		"denominator":{"metric":"ok","labels":{}}}`
	rec := putRule(t, h, "ratio_fire", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %q", rec.Code, rec.Body.String())
	}
	got := decodeBody(t, rec)
	if got["kind"] != "ratio" || got["numerator"].(map[string]any)["metric"] != "bad" ||
		got["denominator"].(map[string]any)["metric"] != "ok" {
		t.Fatalf("ratio rule payload = %v", got)
	}

	// 2/8 = 0.25 < 0.5 -> firing.
	a := alertsByID(t, getAlerts(t, h))["ratio_fire"].(map[string]any)
	if a["state"] != "firing" || a["value"] != float64(0.25) {
		t.Fatalf("ratio alert = %v", a)
	}

	// Zero denominator -> no_data.
	putRule(t, h, "ratio_zero", `{"kind":"ratio","operator":"gt","threshold":1,
		"numerator":{"metric":"ok","labels":{}},
		"denominator":{"metric":"zero","labels":{}}}`)
	a = alertsByID(t, getAlerts(t, h))["ratio_zero"].(map[string]any)
	if a["state"] != "no_data" || a["value"] != nil {
		t.Fatalf("zero-denom alert = %v", a)
	}

	// Missing numerator side -> no_data.
	putRule(t, h, "ratio_missing", `{"kind":"ratio","operator":"gt","threshold":1,
		"numerator":{"metric":"ghost","labels":{}},
		"denominator":{"metric":"ok","labels":{}}}`)
	a = alertsByID(t, getAlerts(t, h))["ratio_missing"].(map[string]any)
	if a["state"] != "no_data" || a["value"] != nil {
		t.Fatalf("missing-side alert = %v", a)
	}
}

func TestInvalidRuleBodies(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"not json":            `{`,
		"not object":          `[1]`,
		"null":                `null`,
		"two values":          `{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{}} {}`,
		"unknown field":       `{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{},"extra":1}`,
		"missing kind":        `{"operator":"gt","threshold":1,"metric":"m","labels":{}}`,
		"missing operator":    `{"kind":"threshold","threshold":1,"metric":"m","labels":{}}`,
		"missing threshold":   `{"kind":"threshold","operator":"gt","metric":"m","labels":{}}`,
		"missing metric":      `{"kind":"threshold","operator":"gt","threshold":1,"labels":{}}`,
		"missing labels":      `{"kind":"threshold","operator":"gt","threshold":1,"metric":"m"}`,
		"null labels":         `{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":null}`,
		"bad kind":            `{"kind":"summary","operator":"gt","threshold":1,"metric":"m","labels":{}}`,
		"bad operator":        `{"kind":"threshold","operator":"eq","threshold":1,"metric":"m","labels":{}}`,
		"bad metric":          `{"kind":"threshold","operator":"gt","threshold":1,"metric":"1m","labels":{}}`,
		"bad label key":       `{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{"a-b":"1"}}`,
		"threshold string":    `{"kind":"threshold","operator":"gt","threshold":"1","metric":"m","labels":{}}`,
		"threshold null":      `{"kind":"threshold","operator":"gt","threshold":null,"metric":"m","labels":{}}`,
		"threshold has num":   `{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{},"numerator":{"metric":"m","labels":{}}}`,
		"threshold has den":   `{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{},"denominator":{"metric":"m","labels":{}}}`,
		"ratio flat fields":   `{"kind":"ratio","operator":"gt","threshold":1,"metric":"m","labels":{}}`,
		"ratio missing den":   `{"kind":"ratio","operator":"gt","threshold":1,"numerator":{"metric":"m","labels":{}}}`,
		"ratio missing num":   `{"kind":"ratio","operator":"gt","threshold":1,"denominator":{"metric":"m","labels":{}}}`,
		"side missing metric": `{"kind":"ratio","operator":"gt","threshold":1,"numerator":{"labels":{}},"denominator":{"metric":"m","labels":{}}}`,
		"side missing labels": `{"kind":"ratio","operator":"gt","threshold":1,"numerator":{"metric":"m"},"denominator":{"metric":"m","labels":{}}}`,
		"side bad metric":     `{"kind":"ratio","operator":"gt","threshold":1,"numerator":{"metric":"m-","labels":{}},"denominator":{"metric":"m","labels":{}}}`,
		"side unknown field":  `{"kind":"ratio","operator":"gt","threshold":1,"numerator":{"metric":"m","labels":{},"x":1},"denominator":{"metric":"m","labels":{}}}`,
		"ratio extra metric":  `{"kind":"ratio","operator":"gt","threshold":1,"metric":"m","labels":{},"numerator":{"metric":"m","labels":{}},"denominator":{"metric":"m","labels":{}}}`,
		"id in body":          `{"id":"x","kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			expectErrorCode(t, putRule(t, h, "r1", body), http.StatusBadRequest, "invalid_rule")
		})
	}

	// A failed create leaves nothing behind; a failed replace keeps the old rule.
	if rec := ruleRequest(t, h, http.MethodGet, "r1", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("r1 should not exist, got %d", rec.Code)
	}
	good := `{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{}}`
	if rec := putRule(t, h, "keep", good); rec.Code != http.StatusCreated {
		t.Fatalf("setup = %d", rec.Code)
	}
	expectErrorCode(t, putRule(t, h, "keep", `{"kind":"bogus"}`), http.StatusBadRequest, "invalid_rule")
	rec := ruleRequest(t, h, http.MethodGet, "keep", "", "")
	if got := decodeBody(t, rec)["metric"]; got != "m" {
		t.Fatalf("original rule changed after invalid replace: %v", rec.Body.String())
	}
}

func TestRulePutMediaTypeAndMethods(t *testing.T) {
	h := Handler()
	body := `{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{}}`

	rec := ruleRequest(t, h, http.MethodPut, "r1", body, "text/plain")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")
	rec = ruleRequest(t, h, http.MethodPut, "r1", body, "")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")

	// Unsupported methods on item -> 405 with Allow.
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		rec := ruleRequest(t, h, method, "r1", "", "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, PUT, DELETE" {
			t.Fatalf("%s item = %d allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	// Unsupported methods on collection -> 405 Allow GET.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, alertRulesPath, nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
			t.Fatalf("%s collection = %d allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	// Alerts is GET-only.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, alertsPath, nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST alerts = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestRuleIDValidation(t *testing.T) {
	h := Handler()
	body := `{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{}}`
	for _, id := range []string{"1bad", "a-b"} {
		rec := ruleRequest(t, h, http.MethodGet, id, "", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("get id %q = %d, want 404", id, rec.Code)
		}
		expectErrorCode(t, ruleRequest(t, h, http.MethodPut, id, body, "application/json"),
			http.StatusBadRequest, "invalid_rule")
		expectErrorCode(t, ruleRequest(t, h, http.MethodDelete, id, "", ""),
			http.StatusNotFound, "rule_not_found")
	}
	// Trailing slash / nested path must not be treated as an id.
	for _, target := range []string{alertRulesPrefix, alertRulesPrefix + "r1/extra"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("nested path %s = %d, want 404", target, rec.Code)
		}
	}
	// Valid ids create successfully.
	for _, id := range []string{"a", "_x", "A9_b"} {
		if rec := putRule(t, h, id, body); rec.Code != http.StatusCreated {
			t.Fatalf("valid id %q -> %d %q", id, rec.Code, rec.Body.String())
		}
	}
}

func TestAlertsUseConsistentSnapshot(t *testing.T) {
	h := Handler()
	putRule(t, h, "r1", `{"kind":"threshold","operator":"gte","threshold":1,"metric":"c","labels":{}}`)

	const writers = 16
	const iterations = 200
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, metricsPath, strings.NewReader(
					`{"samples":[{"name":"c","type":"counter","labels":{},"value":1}]}`))
				req.Header.Set("Content-Type", "application/json")
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusAccepted {
					t.Errorf("post = %d", rec.Code)
					return
				}
			}
		}()
	}
	// Concurrent readers: every response must be valid JSON with a value that
	// reflects some committed snapshot (0..writers*iterations), and no_data
	// must never appear once... it may before the first commit, so only assert
	// structural validity here.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				rec := getAlerts(t, h)
				if rec.Code != http.StatusOK {
					t.Errorf("alerts = %d", rec.Code)
					return
				}
				var payload struct {
					Alerts []struct {
						ID    string   `json:"id"`
						State string   `json:"state"`
						Value *float64 `json:"value"`
					} `json:"alerts"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || len(payload.Alerts) != 1 {
					t.Errorf("bad alerts payload: %s", rec.Body.String())
					return
				}
				a := payload.Alerts[0]
				switch a.State {
				case "firing", "inactive":
					if a.Value == nil {
						t.Errorf("state %s with null value", a.State)
					}
				case "no_data":
					if a.Value != nil {
						t.Errorf("no_data with non-null value")
					}
				default:
					t.Errorf("unknown state %q", a.State)
				}
			}
		}()
	}
	wg.Wait()

	// Final value must be exactly writers*iterations — no lost updates.
	alerts := alertsByID(t, getAlerts(t, h))
	a := alerts["r1"].(map[string]any)
	if a["state"] != "firing" || a["value"] != float64(writers*iterations) {
		t.Fatalf("final alert = %v", a)
	}
}

func alertsByID(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	list := decodeBody(t, rec)["alerts"].([]any)
	out := make(map[string]any, len(list))
	for _, item := range list {
		m := item.(map[string]any)
		out[m["id"].(string)] = m
	}
	return out
}

func alertIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	list := decodeBody(t, rec)["alerts"].([]any)
	ids := make([]string, len(list))
	for i, item := range list {
		ids[i] = item.(map[string]any)["id"].(string)
	}
	return ids
}
