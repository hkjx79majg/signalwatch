package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sloRequest(t *testing.T, h http.Handler, method, id, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	target := slosPath
	if id != "" {
		target += "/" + id
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func putSLO(t *testing.T, h http.Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return sloRequest(t, h, http.MethodPut, id, body, "application/json")
}

func getSLOStatus(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, sloStatusPath, nil))
	return rec
}

const validSLOBody = `{"objective":0.9,"good":{"metric":"good_events","labels":{"route":"/a"}},"total":{"metric":"total_events","labels":{}},"comment":"c"}`

func TestSLOLifecycle(t *testing.T) {
	h := Handler()

	rec := putSLO(t, h, "slo_b", validSLOBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%q", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)
	if created["id"] != "slo_b" || created["objective"] != 0.9 || created["comment"] != "c" {
		t.Fatalf("unexpected create payload: %v", created)
	}
	good := created["good"].(map[string]any)
	if good["metric"] != "good_events" || good["labels"].(map[string]any)["route"] != "/a" {
		t.Fatalf("unexpected good selector: %v", created)
	}

	// Atomic replace -> 200.
	body2 := `{"objective":0.5,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":""}`
	rec = putSLO(t, h, "slo_b", body2)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body=%q", rec.Code, rec.Body.String())
	}
	replaced := decodeBody(t, rec)
	if replaced["objective"] != 0.5 || replaced["comment"] != "" {
		t.Fatalf("replaced slo = %v", rec.Body.String())
	}

	// GET returns the replacement with the path id.
	rec = sloRequest(t, h, http.MethodGet, "slo_b", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	if got := decodeBody(t, rec); got["id"] != "slo_b" || got["objective"] != 0.5 {
		t.Fatalf("get slo = %v", rec.Body.String())
	}

	// Collection sorted by id.
	if rec := putSLO(t, h, "slo_a", validSLOBody); rec.Code != http.StatusCreated {
		t.Fatalf("create slo_a = %d body=%q", rec.Code, rec.Body.String())
	}
	rec = sloRequest(t, h, http.MethodGet, "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	slos := decodeBody(t, rec)["slos"].([]any)
	if len(slos) != 2 || slos[0].(map[string]any)["id"] != "slo_a" ||
		slos[1].(map[string]any)["id"] != "slo_b" {
		t.Fatalf("slos order = %v", slos)
	}

	// DELETE then 404s.
	rec = sloRequest(t, h, http.MethodDelete, "slo_b", "", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d body=%q", rec.Code, rec.Body.String())
	}
	expectErrorCode(t, sloRequest(t, h, http.MethodGet, "slo_b", "", ""), http.StatusNotFound, "slo_not_found")
	expectErrorCode(t, sloRequest(t, h, http.MethodDelete, "slo_b", "", ""), http.StatusNotFound, "slo_not_found")
}

func TestSLOEmptyCollections(t *testing.T) {
	h := Handler()
	rec := sloRequest(t, h, http.MethodGet, "", "", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"slos":[]}` {
		t.Fatalf("empty slos = %d %q", rec.Code, rec.Body.String())
	}
	rec = getSLOStatus(t, h)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"slos":[]}` {
		t.Fatalf("empty status = %d %q", rec.Code, rec.Body.String())
	}
}

func TestSLOInvalidBodies(t *testing.T) {
	h := Handler()

	cases := map[string]string{
		"not an object":       `["x"]`,
		"multi json":          `{} {}`,
		"missing objective":   `{"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"missing good":        `{"objective":0.9,"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"missing total":       `{"objective":0.9,"good":{"metric":"g","labels":{}},"comment":"c"}`,
		"missing comment":     `{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}}}`,
		"extra field":         `{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c","x":1}`,
		"objective string":    `{"objective":"0.9","good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"objective zero":      `{"objective":0,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"objective one":       `{"objective":1,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"objective negative":  `{"objective":-0.1,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"objective above one": `{"objective":1.5,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"good not object":     `{"objective":0.9,"good":"g","total":{"metric":"t","labels":{}},"comment":"c"}`,
		"good missing metric": `{"objective":0.9,"good":{"labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"good extra field":    `{"objective":0.9,"good":{"metric":"g","labels":{},"x":1},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"bad metric ident":    `{"objective":0.9,"good":{"metric":"9bad","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"null labels":         `{"objective":0.9,"good":{"metric":"g","labels":null},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"bad label key":       `{"objective":0.9,"good":{"metric":"g","labels":{"a-b":"x"}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"label value number":  `{"objective":0.9,"good":{"metric":"g","labels":{"a":1}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"comment null":        `{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":null}`,
		"comment number":      `{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":3}`,
	}
	for name, body := range cases {
		if rec := putSLO(t, h, "s1", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400, body=%q", name, rec.Code, rec.Body.String())
		} else {
			expectErrorCode(t, rec, http.StatusBadRequest, "invalid_slo")
		}
	}

	// A failed replacement must keep the old definition.
	if rec := putSLO(t, h, "s1", validSLOBody); rec.Code != http.StatusCreated {
		t.Fatalf("seed create = %d body=%q", rec.Code, rec.Body.String())
	}
	bad := `{"objective":2,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`
	expectErrorCode(t, putSLO(t, h, "s1", bad), http.StatusBadRequest, "invalid_slo")
	rec := sloRequest(t, h, http.MethodGet, "s1", "", "")
	if got := decodeBody(t, rec); got["objective"] != 0.9 || got["comment"] != "c" {
		t.Fatalf("old definition lost after failed replace: %v", got)
	}
}

func TestSLOBadIDsAndMediaTypes(t *testing.T) {
	h := Handler()

	// Invalid identifier on PUT is a bad request; on GET/DELETE it is not found.
	expectErrorCode(t, putSLO(t, h, "9bad", validSLOBody), http.StatusBadRequest, "invalid_slo")
	expectErrorCode(t, sloRequest(t, h, http.MethodGet, "9bad", "", ""), http.StatusNotFound, "slo_not_found")
	expectErrorCode(t, sloRequest(t, h, http.MethodDelete, "9bad", "", ""), http.StatusNotFound, "slo_not_found")

	// Nested paths are outside the resource subtree.
	expectErrorCode(t, sloRequest(t, h, http.MethodPut, "a/b", validSLOBody, "application/json"), http.StatusNotFound, "slo_not_found")
	expectErrorCode(t, sloRequest(t, h, http.MethodGet, "a/b", "", ""), http.StatusNotFound, "slo_not_found")

	// Non-JSON media types are rejected before any body validation.
	expectErrorCode(t, sloRequest(t, h, http.MethodPut, "s1", validSLOBody, "text/plain"), http.StatusUnsupportedMediaType, "unsupported_media_type")
	expectErrorCode(t, sloRequest(t, h, http.MethodPut, "s1", validSLOBody, ""), http.StatusUnsupportedMediaType, "unsupported_media_type")
	expectErrorCode(t, sloRequest(t, h, http.MethodPut, "s1", "not json", "text/plain"), http.StatusUnsupportedMediaType, "unsupported_media_type")
	if rec := putSLO(t, h, "s1", validSLOBody); rec.Code != http.StatusCreated {
		t.Fatalf("json with parameters should be accepted base type: %d", rec.Code)
	}
}

func TestSLOMethodNotAllowed(t *testing.T) {
	h := Handler()

	rec := sloRequest(t, h, http.MethodPost, "", "", "")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("collection Allow = %q", allow)
	}

	rec = sloRequest(t, h, http.MethodPost, "s1", validSLOBody, "application/json")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow != "GET, PUT, DELETE" {
		t.Fatalf("item Allow = %q", allow)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, sloStatusPath, nil))
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("status Allow = %q", allow)
	}
}

func statusItem(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	rec := getSLOStatus(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	for _, item := range decodeBody(t, rec)["slos"].([]any) {
		m := item.(map[string]any)
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("no status item for %q in %q", id, rec.Body.String())
	return nil
}

func closeTo(got any, want float64) bool {
	v, ok := got.(float64)
	if !ok {
		return false
	}
	diff := v - want
	return diff > -1e-9 && diff < 1e-9
}

func TestSLOStatusMetAndBreached(t *testing.T) {
	h := Handler()

	putSLO(t, h, "met", `{"objective":0.9,"good":{"metric":"good_events","labels":{}},"total":{"metric":"total_events","labels":{}},"comment":""}`)
	putSLO(t, h, "breached", `{"objective":0.99,"good":{"metric":"good_events","labels":{}},"total":{"metric":"total_events","labels":{}},"comment":""}`)

	postMetrics(t, h, `{"samples":[
		{"name":"good_events","type":"counter","labels":{"route":"/a"},"value":90},
		{"name":"good_events","type":"counter","labels":{"route":"/b"},"value":5},
		{"name":"total_events","type":"counter","labels":{},"value":100}
	]}`, "")

	// met: compliance 0.95 >= 0.9; budget total 10, bad events 5, remaining 5.
	m := statusItem(t, h, "met")
	if m["state"] != "met" || m["good_events"] != 95.0 || m["total_events"] != 100.0 ||
		!closeTo(m["compliance"], 0.95) || !closeTo(m["error_budget_total"], 10) ||
		!closeTo(m["error_budget_remaining"], 5) || !closeTo(m["error_budget_remaining_ratio"], 0.5) {
		t.Fatalf("met status = %v", m)
	}

	// breached: compliance 0.95 < 0.99; budget total 1, remaining -4 (negative allowed).
	b := statusItem(t, h, "breached")
	if b["state"] != "breached" || !closeTo(b["error_budget_total"], 1) ||
		!closeTo(b["error_budget_remaining"], -4) || !closeTo(b["error_budget_remaining_ratio"], -4) {
		t.Fatalf("breached status = %v", b)
	}
}

func TestSLOStatusIgnoresGaugeAndHistogram(t *testing.T) {
	h := Handler()
	putSLO(t, h, "s1", `{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":""}`)

	// Only gauges and histograms exist: no usable counters -> no_data.
	postMetrics(t, h, `{"samples":[
		{"name":"g","type":"gauge","labels":{},"value":7},
		{"name":"t","type":"histogram","labels":{},"value":1,"buckets":[1,2]}
	]}`, "")
	m := statusItem(t, h, "s1")
	if m["state"] != "no_data" {
		t.Fatalf("state = %v", m)
	}
	for _, field := range []string{"good_events", "total_events", "compliance",
		"error_budget_total", "error_budget_remaining", "error_budget_remaining_ratio"} {
		if m[field] != nil {
			t.Fatalf("%s = %v, want null", field, m[field])
		}
	}
}

func TestSLOStatusNoDataAndInvalidData(t *testing.T) {
	h := Handler()
	putSLO(t, h, "no_counters", `{"objective":0.9,"good":{"metric":"missing_g","labels":{}},"total":{"metric":"missing_t","labels":{}},"comment":""}`)
	putSLO(t, h, "zero_total", `{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"z","labels":{}},"comment":""}`)
	putSLO(t, h, "good_gt_total", `{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":""}`)

	postMetrics(t, h, `{"samples":[
		{"name":"g","type":"counter","labels":{},"value":10},
		{"name":"t","type":"counter","labels":{},"value":5},
		{"name":"z","type":"counter","labels":{},"value":0}
	]}`, "")

	m := statusItem(t, h, "no_counters")
	if m["state"] != "no_data" || m["good_events"] != nil || m["total_events"] != nil {
		t.Fatalf("no_counters = %v", m)
	}

	m = statusItem(t, h, "zero_total")
	if m["state"] != "no_data" || m["compliance"] != nil {
		t.Fatalf("zero_total = %v", m)
	}

	// good (10) > total (5): invalid_data, every numeric field null.
	m = statusItem(t, h, "good_gt_total")
	if m["state"] != "invalid_data" {
		t.Fatalf("good_gt_total = %v", m)
	}
	for _, field := range []string{"good_events", "total_events", "compliance",
		"error_budget_total", "error_budget_remaining", "error_budget_remaining_ratio"} {
		if m[field] != nil {
			t.Fatalf("%s = %v, want null", field, m[field])
		}
	}
}

func TestSLOStatusSortedAndLabelScoped(t *testing.T) {
	h := Handler()
	putSLO(t, h, "b_slo", `{"objective":0.5,"good":{"metric":"hits","labels":{"route":"/a","result":"ok"}},"total":{"metric":"hits","labels":{"route":"/a"}},"comment":""}`)
	putSLO(t, h, "a_slo", `{"objective":0.5,"good":{"metric":"hits","labels":{"route":"/a","result":"ok"}},"total":{"metric":"hits","labels":{"route":"/a"}},"comment":""}`)

	postMetrics(t, h, `{"samples":[
		{"name":"hits","type":"counter","labels":{"route":"/a","result":"ok"},"value":3},
		{"name":"hits","type":"counter","labels":{"route":"/a","result":"err"},"value":1},
		{"name":"hits","type":"counter","labels":{"route":"/b","result":"ok"},"value":100}
	]}`, "")

	rec := getSLOStatus(t, h)
	slos := decodeBody(t, rec)["slos"].([]any)
	if len(slos) != 2 || slos[0].(map[string]any)["id"] != "a_slo" ||
		slos[1].(map[string]any)["id"] != "b_slo" {
		t.Fatalf("status order = %v", slos)
	}
	// Selector matches series containing all given labels; route /b excluded.
	m := slos[0].(map[string]any)
	if m["good_events"] != 3.0 || m["total_events"] != 4.0 || m["compliance"] != 0.75 ||
		m["state"] != "met" {
		t.Fatalf("scoped status = %v", m)
	}
}
