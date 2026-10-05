package server

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func exportRequest(t *testing.T, h http.Handler, method, body, contentType, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, diagnosticExportPath, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if tenant != "" {
		req.Header.Set(tenantHeader, tenant)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func postExport(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return exportRequest(t, h, http.MethodPost, body, "application/json", "")
}

func exportBody(start, end, sections string) string {
	return fmt.Sprintf(`{"start":%q,"end":%q,"sections":[%s]}`, start, end, sections)
}

// exportWindow returns a valid [start, end) pair around now.
func exportWindow() (string, string) {
	start := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	end := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	return start, end
}

func TestDiagnosticExportMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := exportRequest(t, h, method, "", "", "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, body=%q", method, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s Allow = %q", method, rec.Header().Get("Allow"))
		}
		if got := decodeBody(t, rec)["error"].(map[string]any)["code"]; got != "method_not_allowed" {
			t.Fatalf("%s code = %v", method, got)
		}
	}
}

func TestDiagnosticExportUnsupportedMediaType(t *testing.T) {
	h := Handler()
	start, end := exportWindow()
	rec := exportRequest(t, h, http.MethodPost, exportBody(start, end, `"metrics"`), "text/plain", "")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["error"].(map[string]any)["code"]; got != "unsupported_media_type" {
		t.Fatalf("code = %v", got)
	}
}

func TestDiagnosticExportInvalidRequests(t *testing.T) {
	h := Handler()
	start, end := exportWindow()
	far := time.Now().Add(26 * time.Hour).UTC().Format(time.RFC3339Nano)

	cases := map[string]string{
		"not json":            `{`,
		"two values":          `{} {}`,
		"not object":          `[1]`,
		"unknown field":       `{"start":"` + start + `","end":"` + end + `","sections":["metrics"],"extra":1}`,
		"missing start":       `{"end":"` + end + `","sections":["metrics"]}`,
		"missing end":         `{"start":"` + start + `","sections":["metrics"]}`,
		"missing sections":    `{"start":"` + start + `","end":"` + end + `"}`,
		"null sections":       `{"start":"` + start + `","end":"` + end + `","sections":null}`,
		"start not time":      `{"start":"soon","end":"` + end + `","sections":["metrics"]}`,
		"start no zone":       `{"start":"2026-10-04 10:00:00","end":"` + end + `","sections":["metrics"]}`,
		"end not time":        `{"start":"` + start + `","end":"later","sections":["metrics"]}`,
		"start equals end":    exportBody(end, end, `"metrics"`),
		"start after end":     exportBody(end, start, `"metrics"`),
		"span too long":       exportBody(start, far, `"metrics"`),
		"empty sections":      exportBody(start, end, ``),
		"duplicate section":   exportBody(start, end, `"metrics","metrics"`),
		"unknown section":     exportBody(start, end, `"metrics","secrets"`),
		"section wrong case":  exportBody(start, end, `"Metrics"`),
		"sections not array":  `{"start":"` + start + `","end":"` + end + `","sections":"metrics"}`,
		"section not string":  exportBody(start, end, `1`),
		"null section member": exportBody(start, end, `null`),
	}
	for name, body := range cases {
		rec := postExport(t, h, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body=%q", name, rec.Code, rec.Body.String())
		}
		if got := decodeBody(t, rec)["error"].(map[string]any)["code"]; got != "invalid_diagnostic_export" {
			t.Fatalf("%s: code = %v", name, got)
		}
	}
}

func TestDiagnosticExportSpanBoundary(t *testing.T) {
	h := Handler()
	start := time.Now().Add(-time.Hour).UTC()
	// Exactly 24 hours is accepted.
	end := start.Add(24 * time.Hour)
	rec := postExport(t, h, exportBody(start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), `"metrics"`))
	if rec.Code != http.StatusOK {
		t.Fatalf("24h span status = %d, body=%q", rec.Code, rec.Body.String())
	}
	// One nanosecond more is rejected.
	end = start.Add(24*time.Hour + time.Nanosecond)
	rec = postExport(t, h, exportBody(start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), `"metrics"`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("24h+1ns span status = %d, body=%q", rec.Code, rec.Body.String())
	}
}

func TestDiagnosticExportEnvelope(t *testing.T) {
	h := Handler()
	start := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	end := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)

	rec := exportRequest(t, h, http.MethodPost,
		exportBody(start, end, `"logs"`), "application/json", "team_a")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["schema_version"] != 1.0 {
		t.Fatalf("schema_version = %v", body["schema_version"])
	}
	if body["tenant"] != "team_a" {
		t.Fatalf("tenant = %v", body["tenant"])
	}
	generated, ok := body["generated_at"].(string)
	if !ok || !strings.HasSuffix(generated, "Z") {
		t.Fatalf("generated_at = %v", body["generated_at"])
	}
	if _, err := time.Parse(time.RFC3339Nano, generated); err != nil {
		t.Fatalf("generated_at not RFC3339Nano: %v", err)
	}
	rng := body["range"].(map[string]any)
	if rng["start"] != start || rng["end"] != end {
		t.Fatalf("range = %v, want %v..%v", rng, start, end)
	}
	sections := body["sections"].(map[string]any)
	if len(sections) != 1 {
		t.Fatalf("sections = %v", sections)
	}
	if _, ok := sections["logs"]; !ok {
		t.Fatalf("sections missing logs: %v", sections)
	}
	logs := sections["logs"].(map[string]any)
	if entries, ok := logs["entries"].([]any); !ok || len(entries) != 0 {
		t.Fatalf("entries = %v", logs["entries"])
	}
}

func TestDiagnosticExportRangeNormalizedToUTC(t *testing.T) {
	h := Handler()
	rec := postExport(t, h, exportBody(
		"2026-10-04T10:00:00+02:00", "2026-10-04T12:00:00.5+02:00", `"metrics"`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	rng := decodeBody(t, rec)["range"].(map[string]any)
	if rng["start"] != "2026-10-04T08:00:00Z" || rng["end"] != "2026-10-04T10:00:00.5Z" {
		t.Fatalf("range = %v", rng)
	}
}

func TestDiagnosticExportMetrics(t *testing.T) {
	h := Handler()
	now := time.Now()
	ts := func(d time.Duration) string {
		return now.Add(d).UTC().Format(time.RFC3339Nano)
	}

	// Two counter increments (kept as increments), a gauge and a histogram.
	post := `{"samples":[` +
		`{"name":"hits","type":"counter","labels":{"route":"/a"},"value":3,"timestamp":"` + ts(-30*time.Minute) + `"},` +
		`{"name":"hits","type":"counter","labels":{"route":"/a"},"value":4,"timestamp":"` + ts(-20*time.Minute) + `"},` +
		`{"name":"load","type":"gauge","labels":{},"value":1.5,"timestamp":"` + ts(-10*time.Minute) + `"},` +
		`{"name":"lat","type":"histogram","labels":{},"value":0.7,"buckets":[0.5,1],"timestamp":"` + ts(-5*time.Minute) + `"},` +
		`{"name":"old","type":"gauge","labels":{},"value":9,"timestamp":"` + ts(-2*time.Hour) + `"}` +
		`]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, metricsPath, strings.NewReader(post))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("metrics post = %d, body=%q", rec.Code, rec.Body.String())
	}

	start, end := ts(-time.Hour), ts(time.Hour)
	rec = postExport(t, h, exportBody(start, end, `"metrics"`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	metricsSec := body["sections"].(map[string]any)["metrics"].(map[string]any)

	// current: every series, query wire shape, sorted by name.
	current := metricsSec["current"].([]any)
	if len(current) != 4 {
		t.Fatalf("current len = %d: %v", len(current), current)
	}
	names := []string{"hits", "lat", "load", "old"}
	for i, want := range names {
		if got := current[i].(map[string]any)["name"]; got != want {
			t.Fatalf("current[%d].name = %v, want %v", i, got, want)
		}
	}
	hits := current[0].(map[string]any)
	if hits["type"] != "counter" || hits["value"] != 7.0 {
		t.Fatalf("hits current = %v", hits)
	}
	lat := current[1].(map[string]any)
	if lat["count"] != 1.0 || lat["sum"] != 0.7 {
		t.Fatalf("lat current = %v", lat)
	}
	buckets := lat["buckets"].([]any)
	if len(buckets) != 2 || buckets[0].(map[string]any)["le"] != 0.5 || buckets[0].(map[string]any)["count"] != 0.0 ||
		buckets[1].(map[string]any)["le"] != 1.0 || buckets[1].(map[string]any)["count"] != 1.0 {
		t.Fatalf("lat buckets = %v", buckets)
	}

	// samples: raw observations in [start, end); the -2h "old" sample is
	// outside the window and excluded, the rest arrive in timestamp order.
	samples := metricsSec["samples"].([]any)
	if len(samples) != 4 {
		t.Fatalf("samples len = %d: %v", len(samples), samples)
	}
	if samples[0].(map[string]any)["value"] != 3.0 || samples[1].(map[string]any)["value"] != 4.0 {
		t.Fatalf("counter increments not preserved: %v", samples)
	}
	histSample := samples[3].(map[string]any)
	if histSample["name"] != "lat" {
		t.Fatalf("samples[3] = %v", histSample)
	}
	hb := histSample["buckets"].([]any)
	if len(hb) != 2 || hb[0] != 0.5 || hb[1] != 1.0 {
		t.Fatalf("histogram sample buckets = %v", hb)
	}
	if _, present := samples[2].(map[string]any)["buckets"]; present {
		t.Fatalf("gauge sample must not carry buckets: %v", samples[2])
	}
	for i, s := range samples {
		tsStr := s.(map[string]any)["timestamp"].(string)
		if !strings.HasSuffix(tsStr, "Z") {
			t.Fatalf("sample %d timestamp not UTC: %v", i, tsStr)
		}
	}

	// A narrow window excludes out-of-range samples but keeps current.
	rec = postExport(t, h, exportBody(ts(-25*time.Minute), ts(-15*time.Minute), `"metrics"`))
	body = decodeBody(t, rec)
	metricsSec = body["sections"].(map[string]any)["metrics"].(map[string]any)
	samples = metricsSec["samples"].([]any)
	if len(samples) != 1 || samples[0].(map[string]any)["value"] != 4.0 {
		t.Fatalf("narrow samples = %v", samples)
	}
	if len(metricsSec["current"].([]any)) != 4 {
		t.Fatalf("current must not be range-filtered: %v", metricsSec["current"])
	}
}

func TestDiagnosticExportSampleOrdering(t *testing.T) {
	h := Handler()
	ts := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)

	// Same timestamp across series and within one series: the export orders
	// by canonical series key, then commit order.
	post := `{"samples":[` +
		`{"name":"b","type":"gauge","labels":{},"value":1,"timestamp":"` + ts + `"},` +
		`{"name":"a","type":"gauge","labels":{},"value":2,"timestamp":"` + ts + `"},` +
		`{"name":"b","type":"gauge","labels":{},"value":3,"timestamp":"` + ts + `"}` +
		`]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, metricsPath, strings.NewReader(post))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post = %d, body=%q", rec.Code, rec.Body.String())
	}

	start, end := exportWindow()
	rec = postExport(t, h, exportBody(start, end, `"metrics"`))
	samples := decodeBody(t, rec)["sections"].(map[string]any)["metrics"].(map[string]any)["samples"].([]any)
	if len(samples) != 3 {
		t.Fatalf("samples = %v", samples)
	}
	want := []struct {
		name  string
		value float64
	}{{"a", 2}, {"b", 1}, {"b", 3}}
	for i, w := range want {
		got := samples[i].(map[string]any)
		if got["name"] != w.name || got["value"] != w.value {
			t.Fatalf("samples[%d] = %v, want %v", i, got, w)
		}
	}
}

func TestDiagnosticExportAlerts(t *testing.T) {
	h := Handler()

	post := `{"samples":[{"name":"hits","type":"counter","labels":{},"value":150}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, metricsPath, strings.NewReader(post))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	putJSON := func(path, body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d body=%q", path, rec.Code, rec.Body.String())
		}
	}
	putJSON(alertRulesPath+"/high_hits", `{"kind":"threshold","operator":"gt","threshold":100,"metric":"hits","labels":{}}`)
	putJSON(notificationRoutesPath+"/nr1", `{"rule_ids":["high_hits"],"receiver":"oncall","priority":10,"comment":null}`)
	putJSON(slosPath+"/avail", `{"objective":0.9,"good":{"metric":"hits","labels":{}},"total":{"metric":"hits","labels":{}},"comment":""}`)

	start, end := exportWindow()
	rec = postExport(t, h, exportBody(start, end, `"alerts"`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	alerts := decodeBody(t, rec)["sections"].(map[string]any)["alerts"].(map[string]any)

	list := alerts["alerts"].([]any)
	if len(list) != 1 {
		t.Fatalf("alerts = %v", list)
	}
	al := list[0].(map[string]any)
	if al["id"] != "high_hits" || al["state"] != "firing" || al["value"] != 150.0 ||
		al["silenced"] != false || al["inhibited"] != false {
		t.Fatalf("alert = %v", al)
	}

	plan := alerts["notification_plan"].(map[string]any)
	deliveries := plan["deliveries"].([]any)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %v", deliveries)
	}
	d := deliveries[0].(map[string]any)
	if d["alert_id"] != "high_hits" || d["receiver"] != "oncall" || d["route_id"] != "nr1" {
		t.Fatalf("delivery = %v", d)
	}
	if unrouted := plan["unrouted_alert_ids"].([]any); len(unrouted) != 0 {
		t.Fatalf("unrouted = %v", unrouted)
	}

	slos := alerts["slo_status"].(map[string]any)["slos"].([]any)
	if len(slos) != 1 {
		t.Fatalf("slos = %v", slos)
	}
	slo := slos[0].(map[string]any)
	if slo["id"] != "avail" || slo["state"] != "met" || slo["compliance"] != 1.0 {
		t.Fatalf("slo status = %v", slo)
	}
}

func TestDiagnosticExportLogsAndTraces(t *testing.T) {
	h := Handler()
	now := time.Now()
	ts := func(d time.Duration) string {
		return now.Add(d).UTC().Format(time.RFC3339Nano)
	}

	logs := `{"entries":[` +
		`{"id":"in-a","timestamp":"` + ts(-30*time.Minute) + `","level":"info","message":"a","labels":{}},` +
		`{"id":"in-b","timestamp":"` + ts(-20*time.Minute) + `","level":"error","message":"b","labels":{"app":"web"}},` +
		`{"id":"out","timestamp":"` + ts(-2*time.Hour) + `","level":"info","message":"old","labels":{}}` +
		`]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, logsPath, strings.NewReader(logs))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("logs post = %d, body=%q", rec.Code, rec.Body.String())
	}

	spans := `{"spans":[` +
		`{"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"0000000000000001","parent_span_id":null,` +
		`"service":"web","name":"GET /","start_time":"` + ts(-25*time.Minute) + `","end_time":"` + ts(-24*time.Minute) + `","status":"ok","attributes":{}},` +
		`{"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"0000000000000002","parent_span_id":"0000000000000001",` +
		`"service":"db","name":"query","start_time":"` + ts(-3*time.Hour) + `","end_time":"` + ts(-3*time.Hour+time.Minute) + `","status":"error","attributes":{"db":"main"}}` +
		`]}`
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, spansPath, strings.NewReader(spans))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("spans post = %d, body=%q", rec.Code, rec.Body.String())
	}

	start, end := ts(-time.Hour), ts(time.Hour)
	rec = postExport(t, h, exportBody(start, end, `"logs","traces"`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	sections := decodeBody(t, rec)["sections"].(map[string]any)

	entries := sections["logs"].(map[string]any)["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
	}
	// Log read order: timestamp descending.
	if entries[0].(map[string]any)["id"] != "in-b" || entries[1].(map[string]any)["id"] != "in-a" {
		t.Fatalf("entries order = %v", entries)
	}
	if entries[0].(map[string]any)["labels"].(map[string]any)["app"] != "web" {
		t.Fatalf("entry labels = %v", entries[0])
	}

	exportedSpans := sections["traces"].(map[string]any)["spans"].([]any)
	if len(exportedSpans) != 1 {
		t.Fatalf("spans = %v", exportedSpans)
	}
	sp := exportedSpans[0].(map[string]any)
	if sp["span_id"] != "0000000000000001" || sp["service"] != "web" || sp["status"] != "ok" {
		t.Fatalf("span = %v", sp)
	}
	if sp["parent_span_id"] != nil {
		t.Fatalf("parent_span_id = %v", sp["parent_span_id"])
	}
	if !strings.HasSuffix(sp["start_time"].(string), "Z") {
		t.Fatalf("start_time not UTC: %v", sp["start_time"])
	}
}

func TestDiagnosticExportConfiguration(t *testing.T) {
	h := Handler()

	putJSON := func(path, body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d body=%q", path, rec.Code, rec.Body.String())
		}
	}
	putJSON(alertRulesPath+"/r1", `{"kind":"threshold","operator":"gte","threshold":5,"metric":"m","labels":{"a":"b"}}`)
	putJSON(silencesPath+"/s1", `{"rule_ids":["r1"],"starts_at":"2026-01-01T00:00:00Z","ends_at":"2027-01-01T00:00:00Z","comment":"maint"}`)
	putJSON(inhibitRulesPath+"/i1", `{"source_rule_ids":["r1"],"target_rule_ids":["r2"],"comment":""}`)
	putJSON(notificationRoutesPath+"/n1", `{"rule_ids":["r1"],"receiver":"team","priority":5,"comment":"hi"}`)
	putJSON(slosPath+"/avail", validSLOBody)
	putJSON(samplingPolicyPath, `{"log_rate":0.5,"trace_rate":1}`)

	reload := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, discoveryTargetsReloadPath,
		strings.NewReader(`{"targets":[{"id":"web_1","url":"https://example.com/metrics","labels":{"job":"web"},"enabled":true}]}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(reload, req)
	if reload.Code != http.StatusOK {
		t.Fatalf("reload = %d body=%q", reload.Code, reload.Body.String())
	}

	start, end := exportWindow()
	rec := postExport(t, h, exportBody(start, end, `"configuration"`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	cfg := decodeBody(t, rec)["sections"].(map[string]any)["configuration"].(map[string]any)

	rules := cfg["alert_rules"].([]any)
	if len(rules) != 1 || rules[0].(map[string]any)["id"] != "r1" || rules[0].(map[string]any)["threshold"] != 5.0 {
		t.Fatalf("alert_rules = %v", rules)
	}
	silences := cfg["silences"].([]any)
	if len(silences) != 1 || silences[0].(map[string]any)["id"] != "s1" ||
		silences[0].(map[string]any)["state"] != "active" {
		t.Fatalf("silences = %v", silences)
	}
	inhibits := cfg["inhibit_rules"].([]any)
	if len(inhibits) != 1 || inhibits[0].(map[string]any)["id"] != "i1" {
		t.Fatalf("inhibit_rules = %v", inhibits)
	}
	routes := cfg["notification_routes"].([]any)
	if len(routes) != 1 || routes[0].(map[string]any)["receiver"] != "team" {
		t.Fatalf("notification_routes = %v", routes)
	}
	slos := cfg["slos"].([]any)
	if len(slos) != 1 || slos[0].(map[string]any)["objective"] != 0.9 {
		t.Fatalf("slos = %v", slos)
	}
	sampling := cfg["sampling_policy"].(map[string]any)
	if sampling["log_rate"] != 0.5 || sampling["trace_rate"] != 1.0 {
		t.Fatalf("sampling_policy = %v", sampling)
	}
	discovery := cfg["discovery_targets"].(map[string]any)
	if discovery["generation"] != 1.0 {
		t.Fatalf("discovery = %v", discovery)
	}
	targets := discovery["targets"].([]any)
	if len(targets) != 1 || targets[0].(map[string]any)["id"] != "web_1" {
		t.Fatalf("targets = %v", targets)
	}
}

func TestDiagnosticExportTenantIsolation(t *testing.T) {
	h := Handler()
	start, end := exportWindow()

	// Data written under tenant alpha is invisible to beta and default.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, metricsPath,
		strings.NewReader(`{"samples":[{"name":"hits","type":"counter","labels":{},"value":3}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(tenantHeader, "alpha")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post = %d", rec.Code)
	}

	rec = exportRequest(t, h, http.MethodPost, exportBody(start, end, `"metrics"`), "application/json", "beta")
	body := decodeBody(t, rec)
	metricsSec := body["sections"].(map[string]any)["metrics"].(map[string]any)
	if len(metricsSec["current"].([]any)) != 0 || len(metricsSec["samples"].([]any)) != 0 {
		t.Fatalf("beta sees alpha data: %v", metricsSec)
	}
	if body["tenant"] != "beta" {
		t.Fatalf("tenant = %v", body["tenant"])
	}

	rec = exportRequest(t, h, http.MethodPost, exportBody(start, end, `"metrics"`), "application/json", "alpha")
	metricsSec = decodeBody(t, rec)["sections"].(map[string]any)["metrics"].(map[string]any)
	if len(metricsSec["current"].([]any)) != 1 || len(metricsSec["samples"].([]any)) != 1 {
		t.Fatalf("alpha export = %v", metricsSec)
	}

	// Invalid tenant headers are rejected before any export validation.
	rec = exportRequest(t, h, http.MethodPost, "not json", "application/json", "bad tenant")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := decodeBody(t, rec)["error"].(map[string]any)["code"]; got != "invalid_tenant" {
		t.Fatalf("code = %v", got)
	}
}

func TestDiagnosticExportNonFiniteData(t *testing.T) {
	h := Handler()
	big := fmt.Sprintf("%g", math.MaxFloat64)
	post := `{"samples":[` +
		`{"name":"big","type":"counter","labels":{},"value":` + big + `},` +
		`{"name":"big","type":"counter","labels":{},"value":` + big + `}` +
		`]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, metricsPath, strings.NewReader(post))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post = %d, body=%q", rec.Code, rec.Body.String())
	}

	start, end := exportWindow()
	rec = postExport(t, h, exportBody(start, end, `"metrics"`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["error"].(map[string]any)["code"]; got != "invalid_diagnostic_export_data" {
		t.Fatalf("code = %v", got)
	}
}

func TestDiagnosticExportTooLarge(t *testing.T) {
	h := Handler()
	// Fill the tenant with enough retained logs to push the bundle past
	// 10 MiB: 10000 entries of ~1.2 KB each. Timestamps sit a few minutes
	// back so even a slow run keeps them inside the export window.
	message := strings.Repeat("x", 1100)
	base := time.Now().Add(-10 * time.Minute)
	for batch := 0; batch < maxLogIDsPerTenant/maxLogBatch; batch++ {
		var b strings.Builder
		b.WriteString(`{"entries":[`)
		for i := 0; i < maxLogBatch; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			n := batch*maxLogBatch + i
			fmt.Fprintf(&b, `{"id":"log-%06d","timestamp":%q,"level":"info","message":%q,"labels":{}}`,
				n, base.Add(time.Duration(n)*time.Millisecond).UTC().Format(time.RFC3339Nano), message)
		}
		b.WriteString(`]}`)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, logsPath, strings.NewReader(b.String()))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("batch %d = %d, body=%q", batch, rec.Code, rec.Body.String())
		}
	}

	start, end := exportWindow()
	rec := postExport(t, h, exportBody(start, end, `"logs"`))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body=%.200q", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["error"].(map[string]any)["code"]; got != "diagnostic_export_too_large" {
		t.Fatalf("code = %v", got)
	}
}

func TestDiagnosticExportEmptyTenant(t *testing.T) {
	h := Handler()
	start, end := exportWindow()
	rec := postExport(t, h, exportBody(start, end,
		`"metrics","alerts","logs","traces","configuration"`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	sections := decodeBody(t, rec)["sections"].(map[string]any)
	if len(sections) != 5 {
		t.Fatalf("sections = %v", sections)
	}
	metricsSec := sections["metrics"].(map[string]any)
	if len(metricsSec["current"].([]any)) != 0 || len(metricsSec["samples"].([]any)) != 0 {
		t.Fatalf("metrics = %v", metricsSec)
	}
	alerts := sections["alerts"].(map[string]any)
	if len(alerts["alerts"].([]any)) != 0 {
		t.Fatalf("alerts = %v", alerts)
	}
	plan := alerts["notification_plan"].(map[string]any)
	if len(plan["deliveries"].([]any)) != 0 || len(plan["unrouted_alert_ids"].([]any)) != 0 {
		t.Fatalf("plan = %v", plan)
	}
	if len(alerts["slo_status"].(map[string]any)["slos"].([]any)) != 0 {
		t.Fatalf("slo_status = %v", alerts["slo_status"])
	}
	if len(sections["logs"].(map[string]any)["entries"].([]any)) != 0 {
		t.Fatalf("logs = %v", sections["logs"])
	}
	if len(sections["traces"].(map[string]any)["spans"].([]any)) != 0 {
		t.Fatalf("traces = %v", sections["traces"])
	}
	cfg := sections["configuration"].(map[string]any)
	for _, key := range []string{"alert_rules", "silences", "inhibit_rules", "notification_routes", "slos"} {
		if len(cfg[key].([]any)) != 0 {
			t.Fatalf("configuration.%s = %v", key, cfg[key])
		}
	}
	sampling := cfg["sampling_policy"].(map[string]any)
	if sampling["log_rate"] != 1.0 || sampling["trace_rate"] != 1.0 {
		t.Fatalf("sampling_policy = %v", sampling)
	}
	discovery := cfg["discovery_targets"].(map[string]any)
	if discovery["generation"] != 0.0 || len(discovery["targets"].([]any)) != 0 {
		t.Fatalf("discovery_targets = %v", discovery)
	}
}
