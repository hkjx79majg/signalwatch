package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// do issues a request. tenants selects X-SignalWatch-Tenant handling: no
// arguments omits the header; otherwise one header line is added per value.
func do(t *testing.T, h http.Handler, method, path string, body io.Reader, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, tenant := range tenants {
		req.Header.Add(tenantHeader, tenant)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func mustCode(t *testing.T, rec *httptest.ResponseRecorder, want int) map[string]any {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d, body=%q", rec.Code, want, rec.Body.String())
	}
	return decodeBody(t, rec)
}

func TestTenantHeaderMissingSelectsDefault(t *testing.T) {
	h := Handler()

	body := `{"samples":[{"name":"hits","type":"counter","labels":{},"value":3}]}`
	mustCode(t, do(t, h, http.MethodPost, metricsPath, strings.NewReader(body)), http.StatusAccepted)

	// An explicit "default" header sees the same data as a missing header.
	for _, tenants := range [][]string{nil, {"default"}} {
		rec := do(t, h, http.MethodGet, metricsPath+"?name=hits", nil, tenants...)
		list := mustCode(t, rec, http.StatusOK)["series"].([]any)
		if len(list) != 1 {
			t.Fatalf("tenants=%v: series len = %d, want 1", tenants, len(list))
		}
	}
}

func TestMetricsTenantIsolation(t *testing.T) {
	h := Handler()

	// The same series key may carry different types in different tenants.
	post := func(tenant, sample string) {
		t.Helper()
		body := `{"samples":[{"name":"m","type":"` + sample + `","labels":{"k":"v"},"value":1}]}`
		mustCode(t, do(t, h, http.MethodPost, metricsPath, strings.NewReader(body), tenant), http.StatusAccepted)
	}
	post("t_alpha", "counter")
	post("t_beta", "gauge")

	rec := do(t, h, http.MethodGet, metricsPath+"?name=m", nil, "t_alpha")
	list := mustCode(t, rec, http.StatusOK)["series"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["type"] != "counter" {
		t.Fatalf("t_alpha series = %v", list)
	}
	rec = do(t, h, http.MethodGet, metricsPath+"?name=m", nil, "t_beta")
	list = mustCode(t, rec, http.StatusOK)["series"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["type"] != "gauge" {
		t.Fatalf("t_beta series = %v", list)
	}

	// The type conflict is scoped to the tenant: t_alpha rejects a gauge while
	// t_beta accepts another gauge, and the failed batch changes nothing.
	fail := `{"samples":[{"name":"m","type":"gauge","labels":{"k":"v"},"value":2}]}`
	rec = do(t, h, http.MethodPost, metricsPath, strings.NewReader(fail), "t_alpha")
	expectErrorCode(t, rec, http.StatusConflict, "metric_conflict")
	rec = do(t, h, http.MethodPost, metricsPath, strings.NewReader(fail), "t_beta")
	mustCode(t, rec, http.StatusAccepted)

	// t_alpha's counter was not advanced by the rejected batch or t_beta writes.
	rec = do(t, h, http.MethodGet, metricsPath+"?name=m", nil, "t_alpha")
	v := mustCode(t, rec, http.StatusOK)["series"].([]any)[0].(map[string]any)["value"]
	if v != float64(1) {
		t.Fatalf("t_alpha counter value = %v, want 1", v)
	}
}

func TestResourceCollectionsAreTenantScoped(t *testing.T) {
	h := Handler()

	rule := `{"kind":"threshold","operator":"gt","threshold":10,"metric":"hits","labels":{}}`
	mustCode(t, do(t, h, http.MethodPut, alertRulesPrefix+"shared", strings.NewReader(rule), "t_alpha"), http.StatusCreated)

	sil := `{"rule_ids":["shared"],"starts_at":"2000-01-01T00:00:00Z","ends_at":"2100-01-01T00:00:00Z","comment":""}`
	mustCode(t, do(t, h, http.MethodPut, silencesPrefix+"s1", strings.NewReader(sil), "t_alpha"), http.StatusCreated)

	inhib := `{"source_rule_ids":["a"],"target_rule_ids":["shared"],"comment":""}`
	mustCode(t, do(t, h, http.MethodPut, inhibitRulesPrefix+"i1", strings.NewReader(inhib), "t_alpha"), http.StatusCreated)

	route := `{"rule_ids":["shared"],"receiver":"team","priority":10,"comment":null}`
	mustCode(t, do(t, h, http.MethodPut, notificationRoutesPrefix+"n1", strings.NewReader(route), "t_alpha"), http.StatusCreated)

	slo := `{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":""}`
	mustCode(t, do(t, h, http.MethodPut, slosPrefix+"o1", strings.NewReader(slo), "t_alpha"), http.StatusCreated)

	// t_beta sees the baseline empty state for every collection.
	emptyCollections := map[string]string{
		alertRulesPath:             "rules",
		silencesPath:               "silences",
		inhibitRulesPath:           "rules",
		notificationRoutesPath:     "routes",
		slosPath:                   "slos",
		metricsPath + "?name=hits": "series",
	}
	for path, key := range emptyCollections {
		got := mustCode(t, do(t, h, http.MethodGet, path, nil, "t_beta"), http.StatusOK)[key].([]any)
		if len(got) != 0 {
			t.Fatalf("%s in t_beta = %v, want empty", path, got)
		}
	}

	// Item lookups and deletes in the other tenant keep the not_found codes.
	notFound := []struct {
		path string
		code string
	}{
		{alertRulesPrefix + "shared", "rule_not_found"},
		{silencesPrefix + "s1", "silence_not_found"},
		{inhibitRulesPrefix + "i1", "inhibit_rule_not_found"},
		{notificationRoutesPrefix + "n1", "notification_route_not_found"},
		{slosPrefix + "o1", "slo_not_found"},
	}
	for _, tc := range notFound {
		expectErrorCode(t, do(t, h, http.MethodGet, tc.path, nil, "t_beta"), http.StatusNotFound, tc.code)
		expectErrorCode(t, do(t, h, http.MethodDelete, tc.path, nil, "t_beta"), http.StatusNotFound, tc.code)
		// The failed cross-tenant delete left the resource in t_alpha.
		mustCode(t, do(t, h, http.MethodGet, tc.path, nil, "t_alpha"), http.StatusOK)
	}
}

func TestSameResourceIDDifferentPerTenant(t *testing.T) {
	h := Handler()

	r10 := `{"kind":"threshold","operator":"gt","threshold":10,"metric":"hits","labels":{}}`
	r20 := `{"kind":"threshold","operator":"gt","threshold":20,"metric":"hits","labels":{}}`
	mustCode(t, do(t, h, http.MethodPut, alertRulesPrefix+"shared", strings.NewReader(r10), "t_alpha"), http.StatusCreated)
	mustCode(t, do(t, h, http.MethodPut, alertRulesPrefix+"shared", strings.NewReader(r20), "t_beta"), http.StatusCreated)

	threshold := func(tenant string) float64 {
		body := mustCode(t, do(t, h, http.MethodGet, alertRulesPrefix+"shared", nil, tenant), http.StatusOK)
		return body["threshold"].(float64)
	}
	if threshold("t_alpha") != 10 || threshold("t_beta") != 20 {
		t.Fatalf("replacement leaked across tenants: %v %v", threshold("t_alpha"), threshold("t_beta"))
	}
}

func TestComputedEndpointsAreTenantScoped(t *testing.T) {
	h := Handler()

	fire := func(tenant string) {
		t.Helper()
		rule := `{"kind":"threshold","operator":"gt","threshold":0,"metric":"hits","labels":{}}`
		mustCode(t, do(t, h, http.MethodPut, alertRulesPrefix+"r1", strings.NewReader(rule), tenant), http.StatusCreated)
		sample := `{"samples":[{"name":"hits","type":"counter","labels":{},"value":5}]}`
		mustCode(t, do(t, h, http.MethodPost, metricsPath, strings.NewReader(sample), tenant), http.StatusAccepted)
	}
	fire("t_alpha")

	// t_beta's rule collection is empty, so its alert view is empty.
	alerts := func(tenants ...string) []any {
		return mustCode(t, do(t, h, http.MethodGet, alertsPath, nil, tenants...), http.StatusOK)["alerts"].([]any)
	}
	if got := alerts("t_alpha"); len(got) != 1 || got[0].(map[string]any)["state"] != "firing" {
		t.Fatalf("t_alpha alerts = %v", got)
	}
	if got := alerts("t_beta"); len(got) != 0 {
		t.Fatalf("t_beta alerts = %v, want empty", got)
	}
	// The default tenant never received data either.
	if got := alerts(); len(got) != 0 {
		t.Fatalf("default alerts = %v, want empty", got)
	}

	// SLO status likewise evaluates only the current tenant's definitions and
	// counters: an SLO in t_beta referencing t_alpha's counters sees no data.
	slo := `{"objective":0.9,"good":{"metric":"hits","labels":{}},"total":{"metric":"hits","labels":{}},"comment":""}`
	mustCode(t, do(t, h, http.MethodPut, slosPrefix+"o1", strings.NewReader(slo), "t_beta"), http.StatusCreated)
	sloStates := mustCode(t, do(t, h, http.MethodGet, sloStatusPath, nil, "t_beta"), http.StatusOK)["slos"].([]any)
	if len(sloStates) != 1 || sloStates[0].(map[string]any)["state"] != "no_data" {
		t.Fatalf("t_beta slo status = %v", sloStates)
	}
	if got := mustCode(t, do(t, h, http.MethodGet, sloStatusPath, nil, "t_alpha"), http.StatusOK)["slos"].([]any); len(got) != 0 {
		t.Fatalf("t_alpha slo status = %v, want empty", got)
	}
}

func TestReferencesDoNotResolveAcrossTenants(t *testing.T) {
	h := Handler()

	// t_alpha: r1 is firing.
	rule := func(metric string) string {
		return `{"kind":"threshold","operator":"gt","threshold":0,"metric":"` + metric + `","labels":{}}`
	}
	mustCode(t, do(t, h, http.MethodPut, alertRulesPrefix+"r1", strings.NewReader(rule("hits_a")), "t_alpha"), http.StatusCreated)
	sample := `{"samples":[{"name":"hits_a","type":"counter","labels":{},"value":5}]}`
	mustCode(t, do(t, h, http.MethodPost, metricsPath, strings.NewReader(sample), "t_alpha"), http.StatusAccepted)

	// t_beta: r2 is firing. r1 exists and fires only in t_alpha.
	mustCode(t, do(t, h, http.MethodPut, alertRulesPrefix+"r2", strings.NewReader(rule("hits_b")), "t_beta"), http.StatusCreated)
	sampleB := `{"samples":[{"name":"hits_b","type":"counter","labels":{},"value":5}]}`
	mustCode(t, do(t, h, http.MethodPost, metricsPath, strings.NewReader(sampleB), "t_beta"), http.StatusAccepted)

	// An active silence in t_beta referencing r1 must not silence t_alpha's r1.
	sil := `{"rule_ids":["r1"],"starts_at":"2000-01-01T00:00:00Z","ends_at":"2100-01-01T00:00:00Z","comment":""}`
	mustCode(t, do(t, h, http.MethodPut, silencesPrefix+"s1", strings.NewReader(sil), "t_beta"), http.StatusCreated)

	// An inhibit rule in t_beta with source r1 (fires only in t_alpha) must not
	// inhibit t_beta's r2.
	inhib := `{"source_rule_ids":["r1"],"target_rule_ids":["r2"],"comment":""}`
	mustCode(t, do(t, h, http.MethodPut, inhibitRulesPrefix+"i1", strings.NewReader(inhib), "t_beta"), http.StatusCreated)

	got := mustCode(t, do(t, h, http.MethodGet, alertsPath, nil, "t_beta"), http.StatusOK)["alerts"].([]any)
	for _, raw := range got {
		al := raw.(map[string]any)
		if al["id"] == "r2" {
			if al["silenced"] != false || al["inhibited"] != false {
				t.Fatalf("t_beta r2 resolved cross-tenant references: %v", al)
			}
		}
	}
	got = mustCode(t, do(t, h, http.MethodGet, alertsPath, nil, "t_alpha"), http.StatusOK)["alerts"].([]any)
	for _, raw := range got {
		al := raw.(map[string]any)
		if al["id"] == "r1" && (al["silenced"] != false || al["inhibited"] != false) {
			t.Fatalf("t_alpha r1 affected by t_beta references: %v", al)
		}
	}

	// A route in t_beta covering r1 cannot route t_alpha's firing r1; t_beta's
	// own r2 is unrouted.
	route := `{"rule_ids":["r1"],"receiver":"team_b","priority":10,"comment":null}`
	mustCode(t, do(t, h, http.MethodPut, notificationRoutesPrefix+"n1", strings.NewReader(route), "t_beta"), http.StatusCreated)
	plan := mustCode(t, do(t, h, http.MethodGet, notificationPlanPath, nil, "t_beta"), http.StatusOK)
	if len(plan["deliveries"].([]any)) != 0 {
		t.Fatalf("t_beta deliveries = %v, want none", plan["deliveries"])
	}
	if unrouted := plan["unrouted_alert_ids"].([]any); len(unrouted) != 1 || unrouted[0] != "r2" {
		t.Fatalf("t_beta unrouted = %v, want [r2]", unrouted)
	}
	plan = mustCode(t, do(t, h, http.MethodGet, notificationPlanPath, nil, "t_alpha"), http.StatusOK)
	if len(plan["deliveries"].([]any)) != 0 {
		t.Fatalf("t_alpha deliveries = %v, want none (route exists only in t_beta)", plan["deliveries"])
	}
	if unrouted := plan["unrouted_alert_ids"].([]any); len(unrouted) != 1 || unrouted[0] != "r1" {
		t.Fatalf("t_alpha unrouted = %v, want [r1]", unrouted)
	}
}

func TestInvalidTenantHeader(t *testing.T) {
	h := Handler()

	invalid := []string{
		"",
		"1abc",    // leading digit
		" team-a", // leading whitespace: no trimming
		"team-a ", // trailing whitespace
		"team.a",  // illegal character
		"team a",
		"ten/ant",
		"a@b",
		strings.Repeat("a", 65), // too long
		"-team",                 // leading dash
	}
	for _, tenant := range invalid {
		rec := do(t, h, http.MethodGet, metricsPath+"?name=hits", nil, tenant)
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
	}

	// "Default" is a legal name on its own and, without case folding, names a
	// different tenant than the header-less "default".
	seed := `{"samples":[{"name":"hits","type":"counter","labels":{},"value":1}]}`
	mustCode(t, do(t, h, http.MethodPost, metricsPath, strings.NewReader(seed)), http.StatusAccepted)
	upper := mustCode(t, do(t, h, http.MethodGet, metricsPath+"?name=hits", nil, "Default"), http.StatusOK)["series"].([]any)
	if len(upper) != 0 {
		t.Fatalf(`tenant "Default" saw %v, want its own empty state`, upper)
	}

	// Multiple header values are rejected even when both are otherwise legal.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, metricsPath+"?name=hits", nil)
	req.Header.Add(tenantHeader, "t_alpha")
	req.Header.Add(tenantHeader, "t_beta")
	h.ServeHTTP(rec, req)
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")

	// Boundary lengths of the pattern are accepted.
	longest := "a" + strings.Repeat("x", 63)
	mustCode(t, do(t, h, http.MethodGet, metricsPath+"?name=hits", nil, longest), http.StatusOK)
	for _, tenant := range []string{"a", "A", "_", "a0_-b", "tenant-1_2"} {
		mustCode(t, do(t, h, http.MethodGet, metricsPath+"?name=hits", nil, tenant), http.StatusOK)
	}
}

func TestInvalidTenantRejectedBeforeRequestParsing(t *testing.T) {
	h := Handler()

	// Seed data the rejected requests must not touch.
	rule := `{"kind":"threshold","operator":"gt","threshold":10,"metric":"hits","labels":{}}`
	mustCode(t, do(t, h, http.MethodPut, alertRulesPrefix+"r1", strings.NewReader(rule), "t_alpha"), http.StatusCreated)

	// Bad tenant beats a bad media type (would otherwise be 415).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, alertRulesPrefix+"r1", strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set(tenantHeader, "bad tenant")
	h.ServeHTTP(rec, req)
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")

	// Bad tenant beats a malformed query (would otherwise be invalid_query).
	rec = do(t, h, http.MethodGet, metricsPath+"?name=9bad", nil, "!")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")

	// Bad tenant beats an invalid resource id (would otherwise be 404/400).
	rec = do(t, h, http.MethodPut, alertRulesPrefix+"9bad", strings.NewReader(rule), "!")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")

	// Bad tenant beats a delete, and no state changes.
	rec = do(t, h, http.MethodDelete, alertRulesPrefix+"r1", nil, "!")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
	mustCode(t, do(t, h, http.MethodGet, alertRulesPrefix+"r1", nil, "t_alpha"), http.StatusOK)

	// The rejected writes never landed in any tenant, including default.
	if got := mustCode(t, do(t, h, http.MethodGet, alertRulesPath, nil), http.StatusOK)["rules"].([]any); len(got) != 0 {
		t.Fatalf("default rules = %v, want empty", got)
	}
}

func TestHealthzIgnoresTenantHeader(t *testing.T) {
	h := Handler()
	for _, tenants := range [][]string{
		nil,
		{"t_alpha"},
		{"bad tenant"},
		{""},
		{"a", "b"},
	} {
		rec := do(t, h, http.MethodGet, "/healthz", nil, tenants...)
		if rec.Code != http.StatusOK {
			t.Fatalf("tenants=%v: healthz = %d, want 200", tenants, rec.Code)
		}
	}
	// Method handling is also unaffected: the header does not turn POST into a
	// tenant error.
	rec := do(t, h, http.MethodPost, "/healthz", nil, "bad tenant")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST healthz = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestUnknownPathsSkipTenantValidation(t *testing.T) {
	h := Handler()
	// Invalid header on an unregistered path keeps the plain 404.
	if rec := do(t, h, http.MethodGet, "/api/v1/unknown", nil, "!"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown api path = %d, want 404", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/", nil, "!"); rec.Code != http.StatusNotFound {
		t.Fatalf("/api/v1/ = %d, want 404", rec.Code)
	}
	// A valid tenant does not change 404 behavior elsewhere either.
	if rec := do(t, h, http.MethodGet, "/totally-unknown", nil, "t_alpha"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path = %d, want 404", rec.Code)
	}
}

func TestConcurrentDifferentTenantsDoNotInterfere(t *testing.T) {
	h := Handler()
	const goroutines = 16
	const iterations = 100

	var wg sync.WaitGroup
	var failMu sync.Mutex
	failures := 0
	post := func(tenant, typ string) {
		defer wg.Done()
		body := `{"samples":[{"name":"m","type":"` + typ + `","labels":{"k":"v"},"value":1}]}`
		for range iterations {
			rec := do(t, h, http.MethodPost, metricsPath, strings.NewReader(body), tenant)
			if rec.Code != http.StatusAccepted {
				failMu.Lock()
				failures++
				failMu.Unlock()
			}
		}
	}
	for i := range goroutines / 2 {
		_ = i
		wg.Add(2)
		go post("con_a", "counter")
		go post("con_b", "gauge")
	}
	wg.Wait()
	if failures != 0 {
		t.Fatalf("cross-tenant concurrent writes failed %d times", failures)
	}

	rec := do(t, h, http.MethodGet, metricsPath+"?name=m", nil, "con_a")
	a := mustCode(t, rec, http.StatusOK)["series"].([]any)[0].(map[string]any)
	if a["type"] != "counter" || a["value"] != float64(goroutines/2*iterations) {
		t.Fatalf("con_a = %v", a)
	}
	rec = do(t, h, http.MethodGet, metricsPath+"?name=m", nil, "con_b")
	b := mustCode(t, rec, http.StatusOK)["series"].([]any)[0].(map[string]any)
	if b["type"] != "gauge" || b["value"] != float64(1) {
		t.Fatalf("con_b = %v", b)
	}
}
