package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// doRequest issues a request, optionally carrying tenant header lines.
func doRequest(t *testing.T, h http.Handler, method, target, body, contentType string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, reader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i, tv := range tenants {
		if i == 0 {
			req.Header.Set(tenantHeader, tv)
		} else {
			req.Header.Add(tenantHeader, tv)
		}
	}
	h.ServeHTTP(rec, req)
	return rec
}

func tenantJSON(t *testing.T, h http.Handler, method, target, body string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, method, target, body, "application/json", tenants...)
}

func decodeJSON(rec *httptest.ResponseRecorder, target any) error {
	return json.Unmarshal(rec.Body.Bytes(), target)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func seriesCount(rec *httptest.ResponseRecorder) int {
	var payload struct {
		Series []any `json:"series"`
	}
	_ = decodeJSON(rec, &payload)
	return len(payload.Series)
}

func metricValue(t *testing.T, rec *httptest.ResponseRecorder) float64 {
	t.Helper()
	payload := decodeBody(t, rec)
	series, _ := payload["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("want exactly one series, got %v", payload["series"])
	}
	item, _ := series[0].(map[string]any)
	v, _ := item["value"].(float64)
	return v
}

func alertStateFor(rec *httptest.ResponseRecorder, id string) string {
	var payload struct {
		Alerts []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"alerts"`
	}
	_ = decodeJSON(rec, &payload)
	for _, a := range payload.Alerts {
		if a.ID == id {
			return a.State
		}
	}
	return "<missing>"
}

func sloStatusFor(rec *httptest.ResponseRecorder, id string) string {
	var payload struct {
		SLOs []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"slos"`
	}
	_ = decodeJSON(rec, &payload)
	for _, s := range payload.SLOs {
		if s.ID == id {
			return s.State
		}
	}
	return "<missing>"
}

func TestTenantHeaderMissingSelectsDefaultAndExplicitDefaultMatches(t *testing.T) {
	h := Handler()

	rec := postMetrics(t, h, `{"samples":[{"name":"hits","type":"counter","labels":{},"value":3}]}`, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("default write status = %d, body=%q", rec.Code, rec.Body.String())
	}
	// No header and an explicit default header see the same series.
	for _, tenants := range [][]string{nil, {"default"}} {
		rec := doRequest(t, h, http.MethodGet, metricsPath+"?name=hits", "", "", tenants...)
		if rec.Code != http.StatusOK {
			t.Fatalf("read status = %d body=%q", rec.Code, rec.Body.String())
		}
		payload := decodeBody(t, rec)
		series, _ := payload["series"].([]any)
		if len(series) != 1 {
			t.Fatalf("default tenant should see one series, got %v", payload["series"])
		}
	}
}

func TestTenantMetricSeriesAreIsolated(t *testing.T) {
	h := Handler()

	put := func(tenant, typ string, value float64) *httptest.ResponseRecorder {
		return tenantJSON(t, h, http.MethodPost, metricsPath,
			`{"samples":[{"name":"hits","type":"`+typ+`","labels":{"route":"/a"},"value":`+
				formatFloat(value)+`}]}`, tenant)
	}

	if rec := put("team-a", "counter", 3); rec.Code != http.StatusAccepted {
		t.Fatalf("team-a counter: %d %s", rec.Code, rec.Body.String())
	}
	// The same name+label set may carry a different type in another tenant.
	if rec := put("team-b", "gauge", 9); rec.Code != http.StatusAccepted {
		t.Fatalf("team-b gauge: %d %s", rec.Code, rec.Body.String())
	}
	// ... while the type conflict is still rejected within one tenant.
	if rec := put("team-a", "gauge", 1); rec.Code != http.StatusConflict {
		t.Fatalf("same-tenant conflict: got %d want 409", rec.Code)
	}

	recA := doRequest(t, h, http.MethodGet, metricsPath+"?name=hits&label.route=/a", "", "", "team-a")
	recB := doRequest(t, h, http.MethodGet, metricsPath+"?name=hits&label.route=/a", "", "", "team-b")
	recC := doRequest(t, h, http.MethodGet, metricsPath+"?name=hits&label.route=/a", "", "", "team-c")

	if v := metricValue(t, recA); v != 3 {
		t.Fatalf("team-a value = %v, want 3", v)
	}
	if v := metricValue(t, recB); v != 9 {
		t.Fatalf("team-b value = %v, want 9", v)
	}
	if n := seriesCount(recC); n != 0 {
		t.Fatalf("fresh tenant team-c must be empty, got %d series", n)
	}
}

func TestTenantRulesSilencesRoutesAndAlertsAreIsolated(t *testing.T) {
	h := Handler()

	// Identical rule id in both tenants; only default gets metrics, so only
	// default evaluates to firing.
	ruleBody := `{"kind":"threshold","operator":"gt","threshold":1,"metric":"hits","labels":{}}`
	if rec := tenantJSON(t, h, http.MethodPut, alertRulesPrefix+"shared_r", ruleBody, "default"); rec.Code != http.StatusCreated {
		t.Fatalf("default rule put: %d %s", rec.Code, rec.Body.String())
	}
	if rec := tenantJSON(t, h, http.MethodPut, alertRulesPrefix+"shared_r", ruleBody, "team-a"); rec.Code != http.StatusCreated {
		t.Fatalf("team-a rule put: %d %s", rec.Code, rec.Body.String())
	}
	postMetrics(t, h, `{"samples":[{"name":"hits","type":"counter","labels":{},"value":5}]}`, "")

	// Silence and route in team-a reference shared_r; they must not affect
	// default's alert state or notification plan.
	silBody := `{"rule_ids":["shared_r"],"starts_at":"2000-01-01T00:00:00Z","ends_at":"2100-01-01T00:00:00Z","comment":"x"}`
	if rec := tenantJSON(t, h, http.MethodPut, silencesPrefix+"sil1", silBody, "team-a"); rec.Code != http.StatusCreated {
		t.Fatalf("team-a silence put: %d %s", rec.Code, rec.Body.String())
	}
	routeBody := `{"rule_ids":["shared_r"],"receiver":"pager","priority":10,"comment":null}`
	if rec := tenantJSON(t, h, http.MethodPut, notificationRoutesPrefix+"rt1", routeBody, "team-a"); rec.Code != http.StatusCreated {
		t.Fatalf("team-a route put: %d %s", rec.Code, rec.Body.String())
	}
	// Inhibit rule in default references a rule id that only exists in team-a;
	// it must never resolve across tenants.
	inhBody := `{"source_rule_ids":["ghost"],"target_rule_ids":["shared_r"],"comment":""}`
	if rec := tenantJSON(t, h, http.MethodPut, inhibitRulesPrefix+"inh1", inhBody, "default"); rec.Code != http.StatusCreated {
		t.Fatalf("default inhibit put: %d %s", rec.Code, rec.Body.String())
	}

	defaultAlerts := tenantJSON(t, h, http.MethodGet, alertsPath, "")
	if state := alertStateFor(defaultAlerts, "shared_r"); state != "firing" {
		t.Fatalf("default shared_r state = %q, want firing (silence/inhibition leaked across tenants)", state)
	}
	teamAAlerts := tenantJSON(t, h, http.MethodGet, alertsPath, "", "team-a")
	if state := alertStateFor(teamAAlerts, "shared_r"); state != "no_data" {
		t.Fatalf("team-a shared_r state = %q, want no_data (metrics leaked across tenants)", state)
	}

	pd := decodeBody(t, tenantJSON(t, h, http.MethodGet, notificationPlanPath, ""))
	if deliv, _ := pd["deliveries"].([]any); len(deliv) != 0 {
		t.Fatalf("default must have no route for shared_r, got %v", pd["deliveries"])
	}
	if unrouted, _ := pd["unrouted_alert_ids"].([]any); len(unrouted) != 1 || unrouted[0] != "shared_r" {
		t.Fatalf("default shared_r must be unrouted, got %v", pd["unrouted_alert_ids"])
	}
	pa := decodeBody(t, tenantJSON(t, h, http.MethodGet, notificationPlanPath, "", "team-a"))
	if deliv, _ := pa["deliveries"].([]any); len(deliv) != 0 {
		t.Fatalf("team-a must have no deliveries (no firing alert), got %v", pa["deliveries"])
	}

	// Collection endpoints never surface another tenant's resources.
	for _, tc := range []struct {
		target string
		key    string
	}{
		{silencesPath, "silences"},
		{inhibitRulesPath, "rules"},
		{notificationRoutesPath, "routes"},
	} {
		rec := tenantJSON(t, h, http.MethodGet, tc.target, "", "team-a")
		body := decodeBody(t, rec)
		if tc.target == inhibitRulesPath {
			// team-a has no inhibit rules; default's inh1 must not appear.
			if list, _ := body[tc.key].([]any); len(list) != 0 {
				t.Fatalf("team-a %s leaked: %v", tc.target, body[tc.key])
			}
		}
	}
	inhDefault := decodeBody(t, tenantJSON(t, h, http.MethodGet, inhibitRulesPath, "", "default"))
	if list, _ := inhDefault["rules"].([]any); len(list) != 1 {
		t.Fatalf("default should keep its own inhibit rule, got %v", inhDefault["rules"])
	}

	// Deleting in team-a must not touch default's rule.
	if rec := tenantJSON(t, h, http.MethodDelete, alertRulesPrefix+"shared_r", "", "team-a"); rec.Code != http.StatusNoContent {
		t.Fatalf("team-a delete: %d", rec.Code)
	}
	if rec := tenantJSON(t, h, http.MethodGet, alertRulesPrefix+"shared_r", "", "team-a"); rec.Code != http.StatusNotFound {
		t.Fatalf("team-a get after delete: got %d want 404", rec.Code)
	}
	if rec := tenantJSON(t, h, http.MethodGet, alertRulesPrefix+"shared_r", "", "default"); rec.Code != http.StatusOK {
		t.Fatalf("default rule must survive team-a delete, got %d", rec.Code)
	}
}

func TestTenantSLOsAreIsolated(t *testing.T) {
	h := Handler()

	// default: 9/10 good. team-a: no counters at all.
	postMetrics(t, h, `{"samples":[{"name":"good","type":"counter","labels":{},"value":9},{"name":"total","type":"counter","labels":{},"value":10}]}`, "")

	sloBody := `{"objective":0.95,"good":{"metric":"good","labels":{}},"total":{"metric":"total","labels":{}},"comment":""}`
	if rec := tenantJSON(t, h, http.MethodPut, slosPrefix+"s1", sloBody, "default"); rec.Code != http.StatusCreated {
		t.Fatalf("default slo put: %d %s", rec.Code, rec.Body.String())
	}
	if rec := tenantJSON(t, h, http.MethodPut, slosPrefix+"s1", sloBody, "team-a"); rec.Code != http.StatusCreated {
		t.Fatalf("team-a slo put: %d %s", rec.Code, rec.Body.String())
	}

	if st := sloStatusFor(tenantJSON(t, h, http.MethodGet, sloStatusPath, ""), "s1"); st != "breached" {
		t.Fatalf("default s1 = %q, want breached", st)
	}
	if st := sloStatusFor(tenantJSON(t, h, http.MethodGet, sloStatusPath, "", "team-a"), "s1"); st != "no_data" {
		t.Fatalf("team-a s1 = %q, want no_data (counters leaked)", st)
	}
}

func TestFreshTenantUsesBaselineEmptyStateAndNotFound(t *testing.T) {
	h := Handler()

	empty := []struct {
		target string
		key    string
	}{
		{metricsPath + "?name=anything", "series"},
		{alertRulesPath, "rules"},
		{silencesPath, "silences"},
		{inhibitRulesPath, "rules"},
		{notificationRoutesPath, "routes"},
		{slosPath, "slos"},
		{alertsPath, "alerts"},
		{sloStatusPath, "slos"},
	}
	for _, tc := range empty {
		rec := doRequest(t, h, http.MethodGet, tc.target, "", "", "fresh-tenant")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d body=%q", tc.target, rec.Code, rec.Body.String())
		}
		payload := decodeBody(t, rec)
		got, _ := payload[tc.key].([]any)
		if len(got) != 0 {
			t.Fatalf("%s: want empty %s, got %v", tc.target, tc.key, payload[tc.key])
		}
	}

	plan := decodeBody(t, doRequest(t, h, http.MethodGet, notificationPlanPath, "", "", "fresh-tenant"))
	if d, _ := plan["deliveries"].([]any); len(d) != 0 {
		t.Fatalf("fresh deliveries = %v", plan["deliveries"])
	}
	if u, _ := plan["unrouted_alert_ids"].([]any); len(u) != 0 {
		t.Fatalf("fresh unrouted = %v", plan["unrouted_alert_ids"])
	}

	// Single-item reads and deletes against an empty tenant keep the resource's
	// existing not_found code even though the id exists in default.
	ruleBody := `{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{}}`
	tenantJSON(t, h, http.MethodPut, alertRulesPrefix+"r1", ruleBody, "default")
	expectErrorCode(t, tenantJSON(t, h, http.MethodGet, alertRulesPrefix+"r1", "", "fresh-tenant"), http.StatusNotFound, "rule_not_found")
	expectErrorCode(t, tenantJSON(t, h, http.MethodDelete, alertRulesPrefix+"r1", "", "fresh-tenant"), http.StatusNotFound, "rule_not_found")
}

func TestInvalidTenantHeader(t *testing.T) {
	h := Handler()

	invalid := []string{
		"",                      // explicit empty value
		" team-a",               // no leading whitespace trimming
		"team-a ",               // no trailing whitespace trimming
		"1abc",                  // must start with letter or underscore
		"-abc",                  // hyphen is not a legal first character
		"ab/c",                  // illegal character
		"a,b",                   // comma is illegal (single header value, not two)
		"ten_ant\x00",           // NUL illegal
		strings.Repeat("a", 65), // too long
	}
	for _, bad := range invalid {
		rec := doRequest(t, h, http.MethodGet, alertRulesPath, "", "", bad)
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
	}

	// Multiple header lines, each value legal on its own, are rejected.
	rec := doRequest(t, h, http.MethodGet, alertRulesPath, "", "", "team-a", "team-b")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")

	// Valid boundary identifiers are accepted (empty-state 200).
	for _, good := range []string{"_", "a", "A_1-2", strings.Repeat("a", 64)} {
		rec := doRequest(t, h, http.MethodGet, alertRulesPath, "", "", good)
		if rec.Code != http.StatusOK {
			t.Fatalf("tenant %q should be accepted, got %d %s", good, rec.Code, rec.Body.String())
		}
	}
}

func TestInvalidTenantHeaderIsRejectedBeforeParsingAndMutatesNothing(t *testing.T) {
	h := Handler()

	// Seed default with a series and a rule.
	postMetrics(t, h, `{"samples":[{"name":"hits","type":"counter","labels":{},"value":5}]}`, "")
	ruleBody := `{"kind":"threshold","operator":"gt","threshold":1,"metric":"hits","labels":{}}`
	tenantJSON(t, h, http.MethodPut, alertRulesPrefix+"r1", ruleBody, "default")

	// Invalid tenant beats media-type parsing (baseline would be 415)...
	rec := doRequest(t, h, http.MethodPost, metricsPath, `{}`, "text/plain", "bad tenant")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")

	// ...body parsing (baseline invalid_metrics/invalid_rule 400)...
	rec = doRequest(t, h, http.MethodPost, metricsPath, `{not json`, "application/json", "bad tenant")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
	rec = doRequest(t, h, http.MethodPut, alertRulesPrefix+"r1", `{not json`, "application/json", "bad tenant")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")

	// ...resource-id parsing (baseline invalid_rule 400 / rule_not_found 404)...
	rec = doRequest(t, h, http.MethodGet, alertRulesPrefix+"!!!", "", "", "bad tenant")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
	rec = doRequest(t, h, http.MethodDelete, alertRulesPrefix+"r1", "", "", "bad tenant")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")

	// ...query parsing (baseline invalid_query 400), and method handling.
	rec = doRequest(t, h, http.MethodGet, metricsPath, "", "", "bad tenant")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
	rec = doRequest(t, h, http.MethodPost, alertRulesPath, "", "", "bad tenant")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")

	// No state changed anywhere: default rule and metric are intact.
	if rec := tenantJSON(t, h, http.MethodGet, alertRulesPrefix+"r1", "", "default"); rec.Code != http.StatusOK {
		t.Fatalf("default rule changed after rejected requests: %d", rec.Code)
	}
	if rec := doRequest(t, h, http.MethodGet, metricsPath+"?name=hits", "", "", "default"); seriesCount(rec) != 1 {
		t.Fatalf("default metrics changed after rejected requests: %s", rec.Body.String())
	}
}

func TestTenantHeaderIsCaseSensitiveAndNotTrimmed(t *testing.T) {
	h := Handler()

	tenantJSON(t, h, http.MethodPut, alertRulesPrefix+"r1",
		`{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{}}`, "TeamA")

	// "TeamA" and "teama" are different tenants.
	if rec := tenantJSON(t, h, http.MethodGet, alertRulesPrefix+"r1", "", "teama"); rec.Code != http.StatusNotFound {
		t.Fatalf("teama must not see TeamA's rule, got %d", rec.Code)
	}
	if rec := tenantJSON(t, h, http.MethodGet, alertRulesPrefix+"r1", "", "TeamA"); rec.Code != http.StatusOK {
		t.Fatalf("TeamA must see its own rule, got %d", rec.Code)
	}
}

func TestHealthzIgnoresTenantHeaderAndUnknownPathsKeep404(t *testing.T) {
	h := Handler()

	// Valid, invalid and duplicated tenant headers are all ignored by healthz.
	for _, tenants := range [][]string{{"team-a"}, {"bad tenant"}, {"a", "b"}} {
		rec := doRequest(t, h, http.MethodGet, healthzPath, "", "", tenants...)
		if rec.Code != http.StatusOK {
			t.Fatalf("healthz tenants=%v: status %d", tenants, rec.Code)
		}
	}

	// Unknown paths keep the baseline plain 404 even with a bad tenant header.
	rec := doRequest(t, h, http.MethodGet, "/api/v1/nope", "", "", "bad tenant")
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "invalid_tenant") {
		t.Fatalf("unknown path: code=%d body=%q, want baseline 404", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, h, http.MethodGet, "/nope", "", "", "team-a")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unrelated path: code=%d, want 404", rec.Code)
	}
}

func TestDifferentTenantsDoNotBlockEachOther(t *testing.T) {
	registry := newTenantRegistry()
	h := newHandler(registry)

	// Hold tenant block-a's own store lock for the whole test; a request to
	// block-b must still complete because each tenant has an independent lock.
	storeA := registry.storeFor("block-a")
	storeA.mu.Lock()
	defer storeA.mu.Unlock()

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, alertRulesPath, nil)
		req.Header.Set(tenantHeader, "block-b")
		h.ServeHTTP(rec, req)
		done <- rec.Code
	}()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("tenant block-b status = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tenant block-b request blocked behind tenant block-a lock")
	}
}

func TestConcurrentDifferentTenantTrafficNeverMixes(t *testing.T) {
	h := Handler()

	const writers = 8
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := "t" + strconv.Itoa(i)
			body := `{"samples":[{"name":"hits","type":"counter","labels":{"w":"` + strconv.Itoa(i) + `"},"value":` + strconv.Itoa(i+1) + `}]}`
			for k := 0; k < 25; k++ {
				rec := doRequest(t, h, http.MethodPost, metricsPath, body, "application/json", tenant)
				if rec.Code != http.StatusAccepted {
					t.Errorf("tenant %s write %d: %d %s", tenant, k, rec.Code, rec.Body.String())
					return
				}
			}
		}(w)
	}
	wg.Wait()

	for w := 0; w < writers; w++ {
		tenant := "t" + strconv.Itoa(w)
		rec := doRequest(t, h, http.MethodGet, metricsPath+"?name=hits", "", "", tenant)
		if n := seriesCount(rec); n != 1 {
			t.Fatalf("tenant %s: want exactly its own one series, got %d: %s", tenant, n, rec.Body.String())
		}
		rec = doRequest(t, h, http.MethodGet, metricsPath+"?name=hits&label.w="+strconv.Itoa(w), "", "", tenant)
		if v := metricValue(t, rec); v != float64(25*(w+1)) {
			t.Fatalf("tenant %s: value = %v, want %d (cross-tenant write observed)", tenant, v, 25*(w+1))
		}
	}
}
