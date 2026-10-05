package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func postExport(t *testing.T, h http.Handler, body string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return tenantJSON(t, h, http.MethodPost, diagnosticExportPath, body, tenants...)
}

func exportBody(start, end time.Time, sections ...string) string {
	names := make([]string, len(sections))
	for i, s := range sections {
		names[i] = fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf(`{"start":%q,"end":%q,"sections":[%s]}`,
		start.UTC().Format(time.RFC3339Nano),
		end.UTC().Format(time.RFC3339Nano),
		strings.Join(names, ","))
}

func TestDiagnosticExportEmptyEnvelope(t *testing.T) {
	h := Handler()
	now := time.Now()
	rec := postExport(t, h, exportBody(now.Add(-time.Hour), now.Add(time.Hour), "metrics"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}

	payload := decodeBody(t, rec)
	if payload["schema_version"] != float64(1) {
		t.Fatalf("schema_version = %v, want 1", payload["schema_version"])
	}
	if payload["tenant"] != defaultTenant {
		t.Fatalf("tenant = %v, want %q", payload["tenant"], defaultTenant)
	}
	generated, err := time.Parse(time.RFC3339Nano, payload["generated_at"].(string))
	if err != nil {
		t.Fatalf("generated_at not RFC3339Nano: %v", err)
	}
	if generated.Location().String() != "UTC" {
		t.Fatalf("generated_at zone = %v, want UTC", generated.Location())
	}

	rng := payload["range"].(map[string]any)
	if rng["start"] == nil || rng["end"] == nil {
		t.Fatalf("range missing bounds: %v", rng)
	}
	sections := payload["sections"].(map[string]any)
	if len(sections) != 1 {
		t.Fatalf("sections = %v, want only metrics", sections)
	}
	metrics := sections["metrics"].(map[string]any)
	if _, ok := metrics["current"]; !ok {
		t.Fatalf("metrics missing current: %v", metrics)
	}
	if _, ok := metrics["samples"]; !ok {
		t.Fatalf("metrics missing samples: %v", metrics)
	}
}

func TestDiagnosticExportSectionsOnlySelectedAndInOrder(t *testing.T) {
	h := Handler()
	now := time.Now()
	body := exportBody(now.Add(-time.Hour), now.Add(time.Hour), "logs", "metrics", "alerts")
	rec := postExport(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}

	raw := rec.Body.Bytes()
	iLogs := bytes.Index(raw, []byte(`"logs":{`))
	iMetrics := bytes.Index(raw, []byte(`"metrics":{`))
	iAlerts := bytes.Index(raw, []byte(`"alerts":{`))
	if iLogs < 0 || iMetrics < 0 || iAlerts < 0 {
		t.Fatalf("missing section markers: %s", raw)
	}
	if !(iLogs < iMetrics && iMetrics < iAlerts) {
		t.Fatalf("sections not in request order: logs=%d metrics=%d alerts=%d", iLogs, iMetrics, iAlerts)
	}

	payload := decodeBody(t, rec)
	sections := payload["sections"].(map[string]any)
	want := map[string]bool{"logs": true, "metrics": true, "alerts": true}
	if len(sections) != len(want) {
		t.Fatalf("sections = %v", sections)
	}
	for name := range want {
		if _, ok := sections[name]; !ok {
			t.Fatalf("missing section %q", name)
		}
	}
}

func TestDiagnosticExportRangeEchoedInUTC(t *testing.T) {
	h := Handler()
	// Offset input must be normalized to UTC on the way out.
	start := time.Date(2026, 10, 2, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))
	end := start.Add(time.Hour)
	body := fmt.Sprintf(`{"start":%q,"end":%q,"sections":["metrics"]}`,
		start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
	rec := postExport(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	rng := decodeBody(t, rec)["range"].(map[string]any)
	if rng["start"] != "2026-10-02T00:00:00Z" || rng["end"] != "2026-10-02T01:00:00Z" {
		t.Fatalf("range = %v", rng)
	}
}

func TestDiagnosticExportMetricsSamples(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)

	// Two series, shared and distinct instants, in deliberately shuffled
	// commit order. Counter samples keep their written increments.
	body := fmt.Sprintf(`{"samples":[
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":3,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/b"},"value":7,"timestamp":%q},
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":2.5,"timestamp":%q},
		{"name":"temp","type":"gauge","labels":{},"value":42,"timestamp":%q},
		{"name":"lat","type":"histogram","labels":{},"value":6,"buckets":[1,5,10],"timestamp":%q}
	]}`,
		ts(base.Add(20*time.Second)),
		ts(base.Add(10*time.Second)),
		ts(base.Add(10*time.Second)),
		ts(base.Add(30*time.Second)),
		ts(base.Add(10*time.Second)))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post metrics = %d, body=%q", rec.Code, rec.Body.String())
	}

	rec := postExport(t, h, exportBody(base, base.Add(time.Minute), "metrics"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	metrics := decodeBody(t, rec)["sections"].(map[string]any)["metrics"].(map[string]any)

	// Current values follow the GET /api/v1/metrics structure.
	current := metrics["current"].([]any)
	if len(current) != 4 {
		t.Fatalf("current len = %d, want 4: %v", len(current), current)
	}
	byName := map[string]map[string]any{}
	for _, item := range current {
		m := item.(map[string]any)
		route := ""
		if v := m["labels"].(map[string]any)["route"]; v != nil {
			route = v.(string)
		}
		byName[m["name"].(string)+route] = m
	}
	cnt := byName["hits/a"]
	if cnt["value"] != float64(5.5) {
		t.Fatalf("counter current = %v, want 5.5", cnt["value"])
	}
	hist := byName["lat"]
	if hist["count"] != float64(1) || hist["sum"] != float64(6) {
		t.Fatalf("histogram current = %v", hist)
	}
	buckets := hist["buckets"].([]any)
	if len(buckets) != 3 || buckets[2].(map[string]any)["count"] != float64(1) {
		t.Fatalf("histogram buckets = %v", buckets)
	}

	// Sample order: timestamp, then name, then canonical label key, then
	// per-series commit order.
	samples := metrics["samples"].([]any)
	if len(samples) != 5 {
		t.Fatalf("samples len = %d, want 5: %v", len(samples), samples)
	}
	type sampleKey struct {
		ts, name, labels string
	}
	got := make([]sampleKey, 0, len(samples))
	for _, raw := range samples {
		s := raw.(map[string]any)
		labels, _ := json.Marshal(s["labels"])
		got = append(got, sampleKey{s["timestamp"].(string), s["name"].(string), string(labels)})
	}
	want := []sampleKey{
		{ts(base.Add(10 * time.Second)), "hits", `{"route":"/a"}`},
		{ts(base.Add(10 * time.Second)), "hits", `{"route":"/b"}`},
		{ts(base.Add(10 * time.Second)), "lat", `{}`},
		{ts(base.Add(20 * time.Second)), "hits", `{"route":"/a"}`},
		{ts(base.Add(30 * time.Second)), "temp", `{}`},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %+v, want %+v\nfull: %v", i, got[i], want[i], got)
		}
	}

	// Counter increments stay raw; every histogram sample carries buckets.
	var inc float64
	var histBuckets []any
	for _, raw := range samples {
		s := raw.(map[string]any)
		if s["name"] == "hits" && s["labels"].(map[string]any)["route"] == "/a" {
			switch s["timestamp"] {
			case ts(base.Add(10 * time.Second)):
				if s["value"] != float64(2.5) {
					t.Fatalf("counter sample = %v, want raw 2.5", s)
				}
			case ts(base.Add(20 * time.Second)):
				inc = s["value"].(float64)
			}
		}
		if s["name"] == "lat" {
			histBuckets = s["buckets"].([]any)
		}
	}
	if inc != 3 {
		t.Fatalf("second counter sample = %v, want raw 3", inc)
	}
	if len(histBuckets) != 3 {
		t.Fatalf("histogram sample buckets = %v, want fixed bounds", histBuckets)
	}
}

func TestDiagnosticExportMetricsHalfOpenWindowAndRetention(t *testing.T) {
	registry := newTenantRegistry()
	h := newHandler(registry)
	store := registry.storeFor(defaultTenant)

	now := time.Now().UTC()
	start := now.Add(-time.Hour).Truncate(time.Second)
	atStart := start
	atEnd := start.Add(30 * time.Minute)
	old := now.Add(-metricRetention).Add(-time.Minute)
	retained := start.Add(15 * time.Minute)

	store.mu.Lock()
	store.series["g"] = &series{
		name:   "g",
		typ:    "gauge",
		labels: map[string]string{},
		history: []historyPoint{
			{ts: old, value: 1},
			{ts: atStart, value: 2},
			{ts: retained, value: 3},
			{ts: atEnd, value: 4},
		},
	}
	store.mu.Unlock()

	sec := func() exportMetricsSection {
		store.mu.RLock()
		defer store.mu.RUnlock()
		return store.buildMetricsSectionLocked(now, start, atEnd)
	}()
	if len(sec.Samples) != 2 {
		t.Fatalf("samples = %v, want at-start and retained only", sec.Samples)
	}
	for _, sm := range sec.Samples {
		if sm.Value == 1 || sm.Value == 4 {
			t.Fatalf("expired or end-boundary sample leaked: %+v", sm)
		}
	}

	// The HTTP path agrees: end instant is excluded.
	end := atEnd
	rec := postExport(t, h, exportBody(start, end, "metrics"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	samples := decodeBody(t, rec)["sections"].(map[string]any)["metrics"].(map[string]any)["samples"].([]any)
	if len(samples) != 2 {
		t.Fatalf("http samples len = %d, want 2", len(samples))
	}
}

func TestDiagnosticExportAlertsSection(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	if rec := postMetrics(t, h, `{"samples":[
		{"name":"hits","type":"counter","labels":{"route":"/a"},"value":10,"timestamp":`+fmt.Sprintf("%q", ts(base))+`},
		{"name":"good_events","type":"counter","labels":{},"value":99,"timestamp":`+fmt.Sprintf("%q", ts(base))+`},
		{"name":"total_events","type":"counter","labels":{},"value":100,"timestamp":`+fmt.Sprintf("%q", ts(base))+`}
	]}`, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("metrics status = %d, body=%q", rec.Code, rec.Body.String())
	}

	if rec := tenantJSON(t, h, http.MethodPut, alertRulesPrefix+"high_hits",
		`{"kind":"threshold","operator":"gt","threshold":5,"metric":"hits","labels":{"route":"/a"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("rule put = %d, body=%q", rec.Code, rec.Body.String())
	}
	if rec := tenantJSON(t, h, http.MethodPut, alertRulesPrefix+"quiet",
		`{"kind":"threshold","operator":"gt","threshold":1,"metric":"missing","labels":{}}`); rec.Code != http.StatusCreated {
		t.Fatalf("rule put = %d", rec.Code)
	}
	if rec := tenantJSON(t, h, http.MethodPut, notificationRoutesPrefix+"nr1",
		`{"rule_ids":["high_hits"],"receiver":"team","priority":100,"comment":null}`); rec.Code != http.StatusCreated {
		t.Fatalf("route put = %d", rec.Code)
	}
	if rec := tenantJSON(t, h, http.MethodPut, slosPrefix+"s1",
		`{"objective":0.95,"good":{"metric":"good_events","labels":{}},"total":{"metric":"total_events","labels":{}},"comment":"ok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("slo put = %d", rec.Code)
	}

	now := time.Now()
	rec := postExport(t, h, exportBody(now.Add(-2*time.Hour), now.Add(time.Hour), "alerts"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	section := decodeBody(t, rec)["sections"].(map[string]any)["alerts"].(map[string]any)

	alerts := section["alerts"].([]any)
	states := map[string]map[string]any{}
	for _, a := range alerts {
		m := a.(map[string]any)
		states[m["id"].(string)] = m
	}
	if states["high_hits"]["state"] != "firing" {
		t.Fatalf("high_hits = %v", states["high_hits"])
	}
	if states["quiet"]["state"] != "no_data" || states["quiet"]["value"] != nil {
		t.Fatalf("quiet = %v", states["quiet"])
	}

	plan := section["notification_plan"].(map[string]any)
	deliveries := plan["deliveries"].([]any)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %v", deliveries)
	}
	d := deliveries[0].(map[string]any)
	if d["alert_id"] != "high_hits" || d["receiver"] != "team" || d["route_id"] != "nr1" {
		t.Fatalf("delivery = %v", d)
	}
	unrouted := plan["unrouted_alert_ids"].([]any)
	if len(unrouted) != 0 {
		t.Fatalf("unrouted = %v, want empty", unrouted)
	}

	slos := section["slos"].([]any)
	if len(slos) != 1 || slos[0].(map[string]any)["state"] != "met" {
		t.Fatalf("slos = %v", slos)
	}
}

func TestDiagnosticExportLogsSection(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	body := fmt.Sprintf(`{"entries":[
		{"id":"id2","timestamp":%q,"level":"error","message":"second","labels":{"app":"web"},"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"},
		{"id":"id1","timestamp":%q,"level":"info","message":"first","labels":{}}
	]}`, ts(base.Add(20*time.Second)), ts(base.Add(10*time.Second)))
	if rec := tenantJSON(t, h, http.MethodPost, logsPath, body); rec.Code != http.StatusAccepted {
		t.Fatalf("logs post = %d, body=%q", rec.Code, rec.Body.String())
	}

	now := time.Now()
	// Window ends before id2: only id1 is returned.
	rec := postExport(t, h, exportBody(base, base.Add(15*time.Second), "logs"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	entries := decodeBody(t, rec)["sections"].(map[string]any)["logs"].(map[string]any)["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["id"] != "id1" {
		t.Fatalf("windowed entries = %v", entries)
	}

	// Full window: ascending timestamp order, complete fields, UTC times.
	rec = postExport(t, h, exportBody(base, base.Add(time.Minute), "logs"))
	entries = decodeBody(t, rec)["sections"].(map[string]any)["logs"].(map[string]any)["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries len = %d, want 2", len(entries))
	}
	if entries[0].(map[string]any)["id"] != "id1" || entries[1].(map[string]any)["id"] != "id2" {
		t.Fatalf("entries not timestamp-ascending: %v", entries)
	}
	full := entries[1].(map[string]any)
	if full["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || full["level"] != "error" {
		t.Fatalf("log fields incomplete: %v", full)
	}
	if !strings.HasSuffix(full["timestamp"].(string), "Z") {
		t.Fatalf("timestamp not UTC: %v", full["timestamp"])
	}

	// Empty windows use an empty array, not null.
	rec = postExport(t, h, exportBody(now.Add(time.Hour), now.Add(2*time.Hour), "logs"))
	entriesAny := decodeBody(t, rec)["sections"].(map[string]any)["logs"].(map[string]any)["entries"]
	if entriesAny == nil || len(entriesAny.([]any)) != 0 {
		t.Fatalf("empty entries = %v, want []", entriesAny)
	}
}

func TestDiagnosticExportTracesSection(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	traceA := "4bf92f3577b34da6a3ce929d0e0e4736"
	traceB := "4bf92f3577b34da6a3ce929d0e0e4737"

	body := fmt.Sprintf(`{"spans":[
		{"trace_id":%q,"span_id":"0000000000000002","parent_span_id":"0000000000000001","service":"web","name":"child","start_time":%q,"end_time":%q,"status":"ok","attributes":{"route":"/x"}},
		{"trace_id":%q,"span_id":"0000000000000001","parent_span_id":null,"service":"web","name":"root","start_time":%q,"end_time":%q,"status":"error","attributes":{}},
		{"trace_id":%q,"span_id":"0000000000000001","parent_span_id":null,"service":"api","name":"other","start_time":%q,"end_time":%q,"status":"unset","attributes":{}}
	]}`,
		traceA, ts(base.Add(20*time.Second)), ts(base.Add(21*time.Second)),
		traceA, ts(base.Add(10*time.Second)), ts(base.Add(22*time.Second)),
		traceB, ts(base.Add(10*time.Second)), ts(base.Add(11*time.Second)))
	if rec := tenantJSON(t, h, http.MethodPost, spansPath, body); rec.Code != http.StatusAccepted {
		t.Fatalf("spans post = %d, body=%q", rec.Code, rec.Body.String())
	}

	// Window ends at +15s: both same-instant root spans qualify, the child
	// at +20s does not.
	rec := postExport(t, h, exportBody(base, base.Add(15*time.Second), "traces"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	spans := decodeBody(t, rec)["sections"].(map[string]any)["traces"].(map[string]any)["spans"].([]any)
	if len(spans) != 2 {
		t.Fatalf("spans len = %d, want 2: %v", len(spans), spans)
	}
	// Same start_time: trace_id ascending, then span_id.
	if spans[0].(map[string]any)["trace_id"] != traceA || spans[1].(map[string]any)["trace_id"] != traceB {
		t.Fatalf("span order = %v", spans)
	}

	// Full window returns the complete span documents in start_time order.
	rec = postExport(t, h, exportBody(base, base.Add(time.Minute), "traces"))
	spans = decodeBody(t, rec)["sections"].(map[string]any)["traces"].(map[string]any)["spans"].([]any)
	if len(spans) != 3 {
		t.Fatalf("spans len = %d, want 3", len(spans))
	}
	if spans[0].(map[string]any)["name"] != "root" || spans[2].(map[string]any)["name"] != "child" {
		t.Fatalf("span start order = %v", spans)
	}
	child := spans[2].(map[string]any)
	parent := child["parent_span_id"]
	if parent != "0000000000000001" {
		t.Fatalf("parent_span_id = %v", parent)
	}
	root := spans[0].(map[string]any)
	if root["parent_span_id"] != nil {
		t.Fatalf("null parent serialized as %v", root["parent_span_id"])
	}
}

func TestDiagnosticExportConfigurationSection(t *testing.T) {
	h := Handler()
	now := time.Now()

	if rec := tenantJSON(t, h, http.MethodPut, alertRulesPrefix+"r1",
		`{"kind":"threshold","operator":"gt","threshold":1,"metric":"m","labels":{}}`); rec.Code != http.StatusCreated {
		t.Fatalf("rule = %d", rec.Code)
	}
	silenceBody := fmt.Sprintf(`{"rule_ids":["r1"],"starts_at":%q,"ends_at":%q,"comment":"win"}`,
		now.Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		now.Add(time.Hour).UTC().Format(time.RFC3339Nano))
	if rec := tenantJSON(t, h, http.MethodPut, silencesPrefix+"s1", silenceBody); rec.Code != http.StatusCreated {
		t.Fatalf("silence = %d", rec.Code)
	}
	if rec := tenantJSON(t, h, http.MethodPut, inhibitRulesPrefix+"i1",
		`{"source_rule_ids":["r1"],"target_rule_ids":["r2"],"comment":""}`); rec.Code != http.StatusCreated {
		t.Fatalf("inhibit = %d", rec.Code)
	}
	if rec := tenantJSON(t, h, http.MethodPut, notificationRoutesPrefix+"n1",
		`{"rule_ids":["r1"],"receiver":"team","priority":5,"comment":"c"}`); rec.Code != http.StatusCreated {
		t.Fatalf("route = %d", rec.Code)
	}
	if rec := tenantJSON(t, h, http.MethodPut, slosPrefix+"o1",
		`{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"tt","labels":{}},"comment":""}`); rec.Code != http.StatusCreated {
		t.Fatalf("slo = %d", rec.Code)
	}
	if rec := tenantJSON(t, h, http.MethodPut, samplingPolicyPath,
		`{"log_rate":0.5,"trace_rate":1}`); rec.Code != http.StatusOK {
		t.Fatalf("sampling = %d", rec.Code)
	}
	if rec := tenantJSON(t, h, http.MethodPost, discoveryTargetsReloadPath,
		`{"targets":[{"id":"web_1","url":"https://example.com/metrics","labels":{"job":"web"},"enabled":true}]}`); rec.Code != http.StatusOK {
		t.Fatalf("discovery reload = %d", rec.Code)
	}

	rec := postExport(t, h, exportBody(now.Add(-time.Hour), now.Add(time.Hour), "configuration"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	cfg := decodeBody(t, rec)["sections"].(map[string]any)["configuration"].(map[string]any)

	rules := cfg["alert_rules"].([]any)
	if len(rules) != 1 || rules[0].(map[string]any)["id"] != "r1" {
		t.Fatalf("alert_rules = %v", rules)
	}
	silences := cfg["silences"].([]any)
	if len(silences) != 1 || silences[0].(map[string]any)["state"] != "active" {
		t.Fatalf("silences = %v (state must be evaluated at generated_at)", silences)
	}
	inhibits := cfg["inhibit_rules"].([]any)
	if len(inhibits) != 1 || inhibits[0].(map[string]any)["id"] != "i1" {
		t.Fatalf("inhibit_rules = %v", inhibits)
	}
	routes := cfg["notification_routes"].([]any)
	if len(routes) != 1 || routes[0].(map[string]any)["id"] != "n1" {
		t.Fatalf("notification_routes = %v", routes)
	}
	slos := cfg["slos"].([]any)
	if len(slos) != 1 || slos[0].(map[string]any)["id"] != "o1" {
		t.Fatalf("slos = %v", slos)
	}
	policy := cfg["sampling_policy"].(map[string]any)
	if policy["log_rate"] != 0.5 || policy["trace_rate"] != float64(1) {
		t.Fatalf("sampling_policy = %v", policy)
	}
	discovery := cfg["discovery_targets"].(map[string]any)
	if discovery["generation"] != float64(1) {
		t.Fatalf("discovery generation = %v", discovery["generation"])
	}
	targets := discovery["targets"].([]any)
	if len(targets) != 1 || targets[0].(map[string]any)["id"] != "web_1" {
		t.Fatalf("discovery targets = %v", targets)
	}
}

func TestDiagnosticExportValidationErrors(t *testing.T) {
	h := Handler()
	now := time.Now()
	valid := exportBody(now.Add(-time.Hour), now, "metrics")

	cases := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantStatus  int
		wantCode    string
	}{
		{"not json", http.MethodPost, "text/plain", valid, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"missing content type", http.MethodPost, "", valid, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"malformed json", http.MethodPost, "application/json", "{not json", http.StatusBadRequest, "invalid_diagnostic_export"},
		{"missing start", http.MethodPost, "application/json", `{"end":"2026-10-02T01:00:00Z","sections":["metrics"]}`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"missing end", http.MethodPost, "application/json", `{"start":"2026-10-02T00:00:00Z","sections":["metrics"]}`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"missing sections", http.MethodPost, "application/json", `{"start":"2026-10-02T00:00:00Z","end":"2026-10-02T01:00:00Z"}`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"unknown field", http.MethodPost, "application/json", `{"start":"2026-10-02T00:00:00Z","end":"2026-10-02T01:00:00Z","sections":["metrics"],"x":1}`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"bad time", http.MethodPost, "application/json", `{"start":"yesterday","end":"2026-10-02T01:00:00Z","sections":["metrics"]}`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"start equals end", http.MethodPost, "application/json", `{"start":"2026-10-02T01:00:00Z","end":"2026-10-02T01:00:00Z","sections":["metrics"]}`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"start after end", http.MethodPost, "application/json", `{"start":"2026-10-02T02:00:00Z","end":"2026-10-02T01:00:00Z","sections":["metrics"]}`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"span over 24h", http.MethodPost, "application/json", `{"start":"2026-10-02T00:00:00Z","end":"2026-10-03T00:00:01Z","sections":["metrics"]}`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"empty sections", http.MethodPost, "application/json", `{"start":"2026-10-02T00:00:00Z","end":"2026-10-02T01:00:00Z","sections":[]}`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"unknown section", http.MethodPost, "application/json", `{"start":"2026-10-02T00:00:00Z","end":"2026-10-02T01:00:00Z","sections":["metrics","secrets"]}`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"duplicate section", http.MethodPost, "application/json", `{"start":"2026-10-02T00:00:00Z","end":"2026-10-02T01:00:00Z","sections":["metrics","metrics"]}`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"wrong type", http.MethodPost, "application/json", `{"start":"2026-10-02T00:00:00Z","end":"2026-10-02T01:00:00Z","sections":"metrics"}`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"null body", http.MethodPost, "application/json", `null`, http.StatusBadRequest, "invalid_diagnostic_export"},
		{"get method", http.MethodGet, "application/json", "", http.StatusMethodNotAllowed, "method_not_allowed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, h, tc.method, diagnosticExportPath, tc.body, tc.contentType)
			expectErrorCode(t, rec, tc.wantStatus, tc.wantCode)
			if tc.wantStatus == http.StatusMethodNotAllowed {
				if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
					t.Fatalf("Allow = %q, want POST", allow)
				}
			}
		})
	}
}

func TestDiagnosticExportTooLarge(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)

	// 10000 retained logs with ~1100-byte messages produce a package well
	// beyond 10 MiB. Commits run in batches of 500.
	message := strings.Repeat("x", 1100)
	const total = maxLogIDsPerTenant
	for batch := 0; batch < total/maxLogBatch; batch++ {
		var entries strings.Builder
		for i := 0; i < maxLogBatch; i++ {
			n := batch*maxLogBatch + i
			entry := fmt.Sprintf(`{"id":"log%07d","timestamp":%q,"level":"info","message":%q,"labels":{}}`,
				n, ts(base.Add(time.Duration(n)*time.Millisecond)), message)
			if i > 0 {
				entries.WriteByte(',')
			}
			entries.WriteString(entry)
		}
		rec := tenantJSON(t, h, http.MethodPost, logsPath, `{"entries":[`+entries.String()+`]}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("batch %d = %d, body=%q", batch, rec.Code, rec.Body.String())
		}
	}

	now := time.Now()
	rec := postExport(t, h, exportBody(base.Add(-time.Hour), now.Add(time.Hour), "logs"))
	expectErrorCode(t, rec, http.StatusRequestEntityTooLarge, "diagnostic_export_too_large")
	if rec.Body.Len() > 1000 {
		t.Fatalf("oversized response leaked %d bytes of partial content", rec.Body.Len())
	}
}

func TestDiagnosticExportNonFiniteData(t *testing.T) {
	registry := newTenantRegistry()
	h := newHandler(registry)
	store := registry.storeFor(defaultTenant)
	now := time.Now()

	// Writes reject non-finite values, so plant an overflowed series state
	// directly; marshaling the current value must fail the whole export.
	store.mu.Lock()
	store.series[seriesKey("bad", map[string]string{})] = &series{
		name:   "bad",
		typ:    "gauge",
		labels: map[string]string{},
		value:  math.Inf(1),
	}
	store.mu.Unlock()

	rec := postExport(t, h, exportBody(now.Add(-time.Hour), now.Add(time.Hour), "metrics"))
	expectErrorCode(t, rec, http.StatusUnprocessableEntity, "invalid_diagnostic_export_data")

	// A section without non-finite data still succeeds.
	rec = postExport(t, h, exportBody(now.Add(-time.Hour), now.Add(time.Hour), "logs"))
	if rec.Code != http.StatusOK {
		t.Fatalf("logs-only status = %d, body=%q", rec.Code, rec.Body.String())
	}
}

func TestDiagnosticExportTenantIsolation(t *testing.T) {
	h := Handler()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[{"name":"hits","type":"counter","labels":{},"value":3,"timestamp":%q}]}`, ts(base))
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post = %d", rec.Code)
	}

	// Another tenant sees none of the default tenant's data.
	now := time.Now()
	rec := postExport(t, h, exportBody(base.Add(-time.Minute), now.Add(time.Hour), "metrics", "configuration"), "team_a")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)
	if payload["tenant"] != "team_a" {
		t.Fatalf("tenant = %v", payload["tenant"])
	}
	sections := payload["sections"].(map[string]any)
	metrics := sections["metrics"].(map[string]any)
	if len(metrics["current"].([]any)) != 0 || len(metrics["samples"].([]any)) != 0 {
		t.Fatalf("tenant leaked metrics: %v", metrics)
	}
	cfg := sections["configuration"].(map[string]any)
	if len(cfg["alert_rules"].([]any)) != 0 {
		t.Fatalf("tenant leaked configuration: %v", cfg)
	}

	// invalid_tenant wins before body validation.
	rec = tenantJSON(t, h, http.MethodPost, diagnosticExportPath, `{not json`, "bad tenant!")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
}
