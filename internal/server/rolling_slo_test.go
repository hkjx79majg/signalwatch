package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func rollingSLORequest(t *testing.T, h http.Handler, method, path, body, contentType, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	reader := strings.NewReader(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, reader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if tenant != "" {
		req.Header.Set("X-SignalWatch-Tenant", tenant)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func createRollingSLO(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return rollingSLORequest(t, h, http.MethodPost, rollingSLOsPath, body, "application/json", "")
}

func mustCreateRollingSLO(t *testing.T, h http.Handler, body string) {
	t.Helper()
	if rec := createRollingSLO(t, h, body); rec.Code != http.StatusCreated {
		t.Fatalf("create rolling SLO = %d body=%q", rec.Code, rec.Body.String())
	}
}

func postRollingSLORecord(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return rollingSLORequest(t, h, http.MethodPost, rollingSLORecordsPath, body, "application/json", "")
}

func mustPostRollingSLORecord(t *testing.T, h http.Handler, body string) {
	t.Helper()
	rec := postRollingSLORecord(t, h, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("record = %d body=%q", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["accepted"]; got != 1.0 {
		t.Fatalf("accepted = %v", got)
	}
}

func getRollingSLOReport(t *testing.T, h http.Handler, name, at string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{"name": {name}, "at": {at}}
	return rollingSLORequest(t, h, http.MethodGet, rollingSLOReportPath+"?"+q.Encode(), "", "", "")
}

func mustGetRollingSLOReport(t *testing.T, h http.Handler, name, at string) map[string]any {
	t.Helper()
	rec := getRollingSLOReport(t, h, name, at)
	if rec.Code != http.StatusOK {
		t.Fatalf("report = %d body=%q", rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)
}

func TestRollingSLOCreateAndList(t *testing.T) {
	h := Handler()

	// Empty collection baseline.
	rec := rollingSLORequest(t, h, http.MethodGet, rollingSLOsPath, "", "", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"slos":[]}` {
		t.Fatalf("empty list = %d %q", rec.Code, rec.Body.String())
	}

	rec = createRollingSLO(t, h, `{"name":"checkout","objective":0.99,"window_seconds":3600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d body=%q", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)
	if created["name"] != "checkout" || created["objective"] != 0.99 || created["window_seconds"] != 3600.0 {
		t.Fatalf("create payload = %v", created)
	}

	// Fractional windows round-trip.
	mustCreateRollingSLO(t, h, `{"name":"api","objective":0.9,"window_seconds":0.5}`)

	rec = rollingSLORequest(t, h, http.MethodGet, rollingSLOsPath, "", "", "")
	slos := decodeBody(t, rec)["slos"].([]any)
	if len(slos) != 2 || slos[0].(map[string]any)["name"] != "api" ||
		slos[1].(map[string]any)["name"] != "checkout" {
		t.Fatalf("list order = %v", slos)
	}
	if slos[0].(map[string]any)["window_seconds"] != 0.5 {
		t.Fatalf("fractional window = %v", slos[0])
	}
}

func TestRollingSLOCreateInvalid(t *testing.T) {
	h := Handler()

	cases := map[string]string{
		"not an object":      `["x"]`,
		"multi json":         `{} {}`,
		"empty object":       `{}`,
		"missing name":       `{"objective":0.9,"window_seconds":60}`,
		"missing objective":  `{"name":"a","window_seconds":60}`,
		"missing window":     `{"name":"a","objective":0.9}`,
		"extra field":        `{"name":"a","objective":0.9,"window_seconds":60,"x":1}`,
		"name empty":         `{"name":"","objective":0.9,"window_seconds":60}`,
		"name blank":         `{"name":"   ","objective":0.9,"window_seconds":60}`,
		"name tab newline":   `{"name":" \t\n ","objective":0.9,"window_seconds":60}`,
		"name number":        `{"name":3,"objective":0.9,"window_seconds":60}`,
		"name null":          `{"name":null,"objective":0.9,"window_seconds":60}`,
		"objective zero":     `{"name":"a","objective":0,"window_seconds":60}`,
		"objective one":      `{"name":"a","objective":1,"window_seconds":60}`,
		"objective negative": `{"name":"a","objective":-0.5,"window_seconds":60}`,
		"objective above":    `{"name":"a","objective":1.5,"window_seconds":60}`,
		"objective string":   `{"name":"a","objective":"0.9","window_seconds":60}`,
		"objective null":     `{"name":"a","objective":null,"window_seconds":60}`,
		"window zero":        `{"name":"a","objective":0.9,"window_seconds":0}`,
		"window negative":    `{"name":"a","objective":0.9,"window_seconds":-60}`,
		"window tiny":        `{"name":"a","objective":0.9,"window_seconds":1e-12}`,
		"window huge":        `{"name":"a","objective":0.9,"window_seconds":1e30}`,
		"window string":      `{"name":"a","objective":0.9,"window_seconds":"60"}`,
		"window null":        `{"name":"a","objective":0.9,"window_seconds":null}`,
	}
	for name, body := range cases {
		if rec := createRollingSLO(t, h, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400, body=%q", name, rec.Code, rec.Body.String())
		} else {
			expectErrorCode(t, rec, http.StatusBadRequest, "invalid_rolling_slo")
		}
	}

	// Non-JSON media types are rejected before body validation.
	expectErrorCode(t, rollingSLORequest(t, h, http.MethodPost, rollingSLOsPath, `{"name":"a","objective":0.9,"window_seconds":60}`, "text/plain", ""),
		http.StatusUnsupportedMediaType, "unsupported_media_type")
	expectErrorCode(t, rollingSLORequest(t, h, http.MethodPost, rollingSLOsPath, `{"name":"a","objective":0.9,"window_seconds":60}`, "", ""),
		http.StatusUnsupportedMediaType, "unsupported_media_type")

	// No failed attempt registered anything.
	rec := rollingSLORequest(t, h, http.MethodGet, rollingSLOsPath, "", "", "")
	if strings.TrimSpace(rec.Body.String()) != `{"slos":[]}` {
		t.Fatalf("invalid creates left state: %q", rec.Body.String())
	}
}

func TestRollingSLODuplicateNameRejected(t *testing.T) {
	h := Handler()
	mustCreateRollingSLO(t, h, `{"name":"checkout","objective":0.99,"window_seconds":3600}`)

	// Re-registration is an error, never a replace.
	expectErrorCode(t, createRollingSLO(t, h, `{"name":"checkout","objective":0.5,"window_seconds":60}`),
		http.StatusBadRequest, "invalid_rolling_slo")

	// The original definition is untouched.
	rec := rollingSLORequest(t, h, http.MethodGet, rollingSLOsPath, "", "", "")
	slos := decodeBody(t, rec)["slos"].([]any)
	if len(slos) != 1 {
		t.Fatalf("slos = %v", slos)
	}
	first := slos[0].(map[string]any)
	if first["objective"] != 0.99 || first["window_seconds"] != 3600.0 {
		t.Fatalf("definition replaced by duplicate: %v", first)
	}

	// Names are case-sensitive.
	mustCreateRollingSLO(t, h, `{"name":"Checkout","objective":0.5,"window_seconds":60}`)
}

func TestRollingSLORecordValidation(t *testing.T) {
	h := Handler()
	mustCreateRollingSLO(t, h, `{"name":"checkout","objective":0.9,"window_seconds":3600}`)

	cases := map[string]string{
		"not an object":      `["x"]`,
		"multi json":         `{} {}`,
		"missing name":       `{"timestamp":"2026-10-05T10:00:00Z","total":1,"failed":0}`,
		"missing timestamp":  `{"name":"checkout","total":1,"failed":0}`,
		"missing total":      `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","failed":0}`,
		"missing failed":     `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":1}`,
		"extra field":        `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":1,"failed":0,"x":1}`,
		"timestamp naive":    `{"name":"checkout","timestamp":"2026-10-05T10:00:00","total":1,"failed":0}`,
		"timestamp bad":      `{"name":"checkout","timestamp":"not-a-time","total":1,"failed":0}`,
		"timestamp number":   `{"name":"checkout","timestamp":3,"total":1,"failed":0}`,
		"total negative":     `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":-1,"failed":0}`,
		"total fractional":   `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":1.5,"failed":0}`,
		"total string":       `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":"1","failed":0}`,
		"total null":         `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":null,"failed":0}`,
		"failed negative":    `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":1,"failed":-1}`,
		"failed fractional":  `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":2,"failed":0.5}`,
		"failed above total": `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":2,"failed":3}`,
		"failed string":      `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":2,"failed":"1"}`,
		"failed null":        `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":2,"failed":null}`,
		"name number":        `{"name":7,"timestamp":"2026-10-05T10:00:00Z","total":1,"failed":0}`,
		"total too large":    `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":1e19,"failed":0}`,
	}
	for name, body := range cases {
		if rec := postRollingSLORecord(t, h, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400, body=%q", name, rec.Code, rec.Body.String())
		} else {
			expectErrorCode(t, rec, http.StatusBadRequest, "invalid_rolling_slo_record")
		}
	}

	// Zero requests with zero failures is legal.
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":0,"failed":0}`)

	// Non-JSON media types are rejected before body validation.
	expectErrorCode(t, rollingSLORequest(t, h, http.MethodPost, rollingSLORecordsPath, `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":1,"failed":0}`, "text/plain", ""),
		http.StatusUnsupportedMediaType, "unsupported_media_type")

	// Unknown names are KeyError-style not found, after body validation.
	expectErrorCode(t, postRollingSLORecord(t, h, `{"name":"missing","timestamp":"2026-10-05T10:00:00Z","total":1,"failed":0}`),
		http.StatusNotFound, "rolling_slo_not_found")

	// No partial data: rejected writes left nothing beyond the one legal
	// zero-count record, which contributes zero events.
	report := mustGetRollingSLOReport(t, h, "checkout", "2026-10-05T11:00:00Z")
	if report["total_requests"] != 0.0 || report["failed_requests"] != 0.0 {
		t.Fatalf("rejected writes left data: %v", report)
	}
}

func TestRollingSLORecordAccumulatesSameInstantAndOutOfOrder(t *testing.T) {
	h := Handler()
	mustCreateRollingSLO(t, h, `{"name":"checkout","objective":0.9,"window_seconds":3600}`)

	// Multiple writes at the same instant accumulate.
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:15:00Z","total":10,"failed":1}`)
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:15:00Z","total":5,"failed":2}`)

	// Out-of-order writes land as if written in time order.
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:45:00Z","total":100,"failed":5}`)
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:30:00Z","total":7,"failed":0}`)

	report := mustGetRollingSLOReport(t, h, "checkout", "2026-10-05T11:00:00Z")
	if report["total_requests"] != 122.0 || report["failed_requests"] != 8.0 {
		t.Fatalf("accumulated report = %v", report)
	}

	// Same totals written in order into a second SLO give identical reports.
	mustCreateRollingSLO(t, h, `{"name":"ordered","objective":0.9,"window_seconds":3600}`)
	mustPostRollingSLORecord(t, h, `{"name":"ordered","timestamp":"2026-10-05T10:15:00Z","total":10,"failed":1}`)
	mustPostRollingSLORecord(t, h, `{"name":"ordered","timestamp":"2026-10-05T10:15:00Z","total":5,"failed":2}`)
	mustPostRollingSLORecord(t, h, `{"name":"ordered","timestamp":"2026-10-05T10:30:00Z","total":7,"failed":0}`)
	mustPostRollingSLORecord(t, h, `{"name":"ordered","timestamp":"2026-10-05T10:45:00Z","total":100,"failed":5}`)

	ordered := mustGetRollingSLOReport(t, h, "ordered", "2026-10-05T11:00:00Z")
	for _, field := range []string{"total_requests", "failed_requests", "success_rate",
		"allowed_failures", "error_budget_remaining_ratio", "burn_rate"} {
		if report[field] != ordered[field] {
			t.Fatalf("%s: out-of-order %v != in-order %v", field, report[field], ordered[field])
		}
	}
}

func TestRollingSLOReportWindowBounds(t *testing.T) {
	h := Handler()
	mustCreateRollingSLO(t, h, `{"name":"checkout","objective":0.9,"window_seconds":3600}`)

	// Window for at=11:00:00Z is (10:00:00Z, 11:00:00Z]: start exclusive,
	// end inclusive, later data never counted early.
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":1000,"failed":1000}`)
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:00:00.000000001Z","total":2,"failed":1}`)
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T11:00:00Z","total":4,"failed":1}`)
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T11:00:00.000000001Z","total":2000,"failed":2000}`)

	report := mustGetRollingSLOReport(t, h, "checkout", "2026-10-05T11:00:00Z")
	if report["window_start"] != "2026-10-05T10:00:00Z" || report["window_end"] != "2026-10-05T11:00:00Z" {
		t.Fatalf("window bounds = %v", report)
	}
	if report["total_requests"] != 6.0 || report["failed_requests"] != 2.0 {
		t.Fatalf("windowed counts = %v", report)
	}

	// One nanosecond later the future record joins, while the record that was
	// just inside the start is now exactly at the excluded boundary.
	report = mustGetRollingSLOReport(t, h, "checkout", "2026-10-05T11:00:00.000000001Z")
	if report["total_requests"] != 2004.0 || report["failed_requests"] != 2001.0 {
		t.Fatalf("shifted window counts = %v", report)
	}
	if report["window_end"] != "2026-10-05T11:00:00.000000001Z" {
		t.Fatalf("shifted window end = %v", report)
	}
}

func TestRollingSLOErrorBudgetAndBurnRate(t *testing.T) {
	h := Handler()
	mustCreateRollingSLO(t, h, `{"name":"checkout","objective":0.9,"window_seconds":3600}`)
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:30:00Z","total":200,"failed":10}`)

	report := mustGetRollingSLOReport(t, h, "checkout", "2026-10-05T11:00:00Z")
	if report["total_requests"] != 200.0 || report["failed_requests"] != 10.0 ||
		!closeTo(report["success_rate"], 0.95) ||
		!closeTo(report["allowed_failures"], 20) ||
		!closeTo(report["error_budget_remaining_ratio"], 0.5) ||
		!closeTo(report["burn_rate"], 0.5) {
		t.Fatalf("budget report = %v", report)
	}

	// Zero-count batches add nothing; burning past the budget floors the
	// remaining ratio at zero.
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:45:00Z","total":0,"failed":0}`)
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:46:00Z","total":100,"failed":30}`)
	report = mustGetRollingSLOReport(t, h, "checkout", "2026-10-05T11:00:00Z")
	if report["total_requests"] != 300.0 || report["failed_requests"] != 40.0 ||
		!closeTo(report["allowed_failures"], 30) ||
		report["error_budget_remaining_ratio"] != 0.0 ||
		!closeTo(report["burn_rate"], (40.0/300.0)/0.1) {
		t.Fatalf("exhausted budget report = %v", report)
	}
}

func TestRollingSLOEmptyWindowDefaults(t *testing.T) {
	h := Handler()
	mustCreateRollingSLO(t, h, `{"name":"checkout","objective":0.99,"window_seconds":60}`)

	// A record outside the window does not disturb the empty-window defaults.
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:00:00Z","total":50,"failed":50}`)

	report := mustGetRollingSLOReport(t, h, "checkout", "2026-10-05T11:00:00Z")
	if report["total_requests"] != 0.0 || report["failed_requests"] != 0.0 ||
		report["success_rate"] != 1.0 || report["error_budget_remaining_ratio"] != 1.0 ||
		report["burn_rate"] != 0.0 || report["allowed_failures"] != 0.0 {
		t.Fatalf("empty window report = %v", report)
	}
}

func TestRollingSLOReportImmutableAndSLOsIsolated(t *testing.T) {
	h := Handler()
	mustCreateRollingSLO(t, h, `{"name":"a","objective":0.9,"window_seconds":3600}`)
	mustCreateRollingSLO(t, h, `{"name":"b","objective":0.5,"window_seconds":60}`)

	mustPostRollingSLORecord(t, h, `{"name":"a","timestamp":"2026-10-05T10:30:00Z","total":10,"failed":1}`)
	before := mustGetRollingSLOReport(t, h, "a", "2026-10-05T11:00:00Z")

	// Writes to another SLO never pollute this one.
	mustPostRollingSLORecord(t, h, `{"name":"b","timestamp":"2026-10-05T10:59:30Z","total":1000,"failed":999}`)
	after := mustGetRollingSLOReport(t, h, "a", "2026-10-05T11:00:00Z")
	for _, field := range []string{"total_requests", "failed_requests", "success_rate",
		"allowed_failures", "error_budget_remaining_ratio", "burn_rate"} {
		if before[field] != after[field] {
			t.Fatalf("%s changed after writes to another SLO: %v -> %v", field, before[field], after[field])
		}
	}
	if before["total_requests"] != 10.0 || before["failed_requests"] != 1.0 {
		t.Fatalf("isolated report = %v", before)
	}

	// The previously returned report is a snapshot: later writes to the same
	// SLO do not rewrite it, only fresh queries see the new data.
	mustPostRollingSLORecord(t, h, `{"name":"a","timestamp":"2026-10-05T10:45:00Z","total":90,"failed":9}`)
	fresh := mustGetRollingSLOReport(t, h, "a", "2026-10-05T11:00:00Z")
	if before["total_requests"] != 10.0 || before["failed_requests"] != 1.0 {
		t.Fatalf("returned report mutated: %v", before)
	}
	if fresh["total_requests"] != 100.0 || fresh["failed_requests"] != 10.0 {
		t.Fatalf("fresh report = %v", fresh)
	}

	// SLO b saw only its own record.
	reportB := mustGetRollingSLOReport(t, h, "b", "2026-10-05T11:00:00Z")
	if reportB["total_requests"] != 1000.0 || reportB["failed_requests"] != 999.0 {
		t.Fatalf("b report = %v", reportB)
	}
}

func TestRollingSLOTimezoneHandling(t *testing.T) {
	h := Handler()
	mustCreateRollingSLO(t, h, `{"name":"checkout","objective":0.9,"window_seconds":3600}`)

	// The same instant written with different offsets is one instant.
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T12:30:00+02:00","total":10,"failed":1}`)
	mustPostRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:30:00Z","total":10,"failed":1}`)

	report := mustGetRollingSLOReport(t, h, "checkout", "2026-10-05T11:00:00Z")
	if report["total_requests"] != 20.0 || report["failed_requests"] != 2.0 {
		t.Fatalf("offset report = %v", report)
	}

	// Querying with an offset denoting the same instant gives the same window.
	report = mustGetRollingSLOReport(t, h, "checkout", "2026-10-05T13:00:00+02:00")
	if report["total_requests"] != 20.0 || report["window_end"] != "2026-10-05T11:00:00Z" ||
		report["window_start"] != "2026-10-05T10:00:00Z" {
		t.Fatalf("offset query report = %v", report)
	}

	// Naive query times are rejected.
	expectErrorCode(t, getRollingSLOReport(t, h, "checkout", "2026-10-05T11:00:00"),
		http.StatusBadRequest, "invalid_rolling_slo_query")
}

func TestRollingSLOReportQueryValidation(t *testing.T) {
	h := Handler()
	mustCreateRollingSLO(t, h, `{"name":"checkout","objective":0.9,"window_seconds":3600}`)

	bad := []string{
		"",                            // nothing
		"name=checkout",               // missing at
		"at=2026-10-05T11:00:00Z",     // missing name
		"name=checkout&at=not-a-time", // bad at
		"name=checkout&at=2026-10-05T11:00:00Z&at=2026-10-05T12:00:00Z", // duplicate at
		"name=a&name=b&at=2026-10-05T11:00:00Z",                         // duplicate name
		"name=checkout&at=2026-10-05T11:00:00Z&x=1",                     // unknown param
	}
	for _, q := range bad {
		rec := rollingSLORequest(t, h, http.MethodGet, rollingSLOReportPath+"?"+q, "", "", "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: status = %d, want 400, body=%q", q, rec.Code, rec.Body.String())
		} else {
			expectErrorCode(t, rec, http.StatusBadRequest, "invalid_rolling_slo_query")
		}
	}

	// Unknown names are KeyError-style not found.
	expectErrorCode(t, getRollingSLOReport(t, h, "missing", "2026-10-05T11:00:00Z"),
		http.StatusNotFound, "rolling_slo_not_found")
}

func TestRollingSLOMethodNotAllowed(t *testing.T) {
	h := Handler()

	rec := rollingSLORequest(t, h, http.MethodPut, rollingSLOsPath, "", "", "")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow != "GET, POST" {
		t.Fatalf("collection Allow = %q", allow)
	}

	rec = rollingSLORequest(t, h, http.MethodGet, rollingSLORecordsPath, "", "", "")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("records Allow = %q", allow)
	}

	rec = rollingSLORequest(t, h, http.MethodPost, rollingSLOReportPath, "", "", "")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("report Allow = %q", allow)
	}
}

func TestRollingSLOTenantIsolation(t *testing.T) {
	h := Handler()

	// Definitions and records live per tenant.
	rec := rollingSLORequest(t, h, http.MethodPost, rollingSLOsPath,
		`{"name":"checkout","objective":0.9,"window_seconds":3600}`, "application/json", "team_a")
	if rec.Code != http.StatusCreated {
		t.Fatalf("tenant create = %d body=%q", rec.Code, rec.Body.String())
	}
	rec = rollingSLORequest(t, h, http.MethodPost, rollingSLORecordsPath,
		`{"name":"checkout","timestamp":"2026-10-05T10:30:00Z","total":10,"failed":1}`, "application/json", "team_a")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("tenant record = %d body=%q", rec.Code, rec.Body.String())
	}

	// The default tenant sees neither the definition nor its records.
	rec = rollingSLORequest(t, h, http.MethodGet, rollingSLOsPath, "", "", "")
	if strings.TrimSpace(rec.Body.String()) != `{"slos":[]}` {
		t.Fatalf("default tenant list = %q", rec.Body.String())
	}
	expectErrorCode(t, getRollingSLOReport(t, h, "checkout", "2026-10-05T11:00:00Z"),
		http.StatusNotFound, "rolling_slo_not_found")
	expectErrorCode(t, postRollingSLORecord(t, h, `{"name":"checkout","timestamp":"2026-10-05T10:30:00Z","total":10,"failed":0}`),
		http.StatusNotFound, "rolling_slo_not_found")

	// The owning tenant reads its report.
	rec = rollingSLORequest(t, h, http.MethodGet,
		rollingSLOReportPath+"?name=checkout&at=2026-10-05T11:00:00Z", "", "", "team_a")
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant report = %d body=%q", rec.Code, rec.Body.String())
	}
	report := decodeBody(t, rec)
	if report["total_requests"] != 10.0 || report["failed_requests"] != 1.0 {
		t.Fatalf("tenant report = %v", report)
	}
}

func TestRollingSLOConcurrentWritesAccumulate(t *testing.T) {
	h := Handler()
	mustCreateRollingSLO(t, h, `{"name":"checkout","objective":0.9,"window_seconds":3600}`)

	const goroutines, perGoroutine = 16, 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				// Errorf, unlike Fatalf, is safe to call from another
				// goroutine; it does not call runtime.Goexit.
				if rec := postRollingSLORecord(t, h,
					`{"name":"checkout","timestamp":"2026-10-05T10:30:00Z","total":2,"failed":1}`); rec.Code != http.StatusAccepted {
					t.Errorf("record = %d body=%q", rec.Code, rec.Body.String())
				}
			}
		}()
	}
	wg.Wait()

	report := mustGetRollingSLOReport(t, h, "checkout", "2026-10-05T11:00:00Z")
	want := float64(goroutines * perGoroutine * 2)
	if report["total_requests"] != want || report["failed_requests"] != want/2 {
		t.Fatalf("concurrent totals = %v, want %v", report, want)
	}
}
