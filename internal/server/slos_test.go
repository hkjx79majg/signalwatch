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

func getSLOStatus(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, sloStatusPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("slo-status status = %d body=%q", rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)
}

func validSLOBody() string {
	return `{"objective":0.9,
	         "good":{"metric":"good_events","labels":{"svc":"a"}},
	         "total":{"metric":"total_events","labels":{"svc":"a"}},
	         "comment":"c1"}`
}

func TestSLOLifecycle(t *testing.T) {
	h := Handler()

	rec := putSLO(t, h, "slo_a", validSLOBody())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%q", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)
	if created["id"] != "slo_a" || created["objective"] != 0.9 || created["comment"] != "c1" {
		t.Fatalf("unexpected create payload: %v", created)
	}
	good := created["good"].(map[string]any)
	if good["metric"] != "good_events" || good["labels"].(map[string]any)["svc"] != "a" {
		t.Fatalf("unexpected good selector: %v", created["good"])
	}

	// Atomic replace -> 200.
	replace := `{"objective":0.95,
	            "good":{"metric":"g2","labels":{}},
	            "total":{"metric":"t2","labels":{}},
	            "comment":"c2"}`
	rec = putSLO(t, h, "slo_a", replace)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace status = %d body=%q", rec.Code, rec.Body.String())
	}
	replaced := decodeBody(t, rec)
	if replaced["objective"] != 0.95 || replaced["comment"] != "c2" {
		t.Fatalf("replaced payload: %v", replaced)
	}
	if replaced["good"].(map[string]any)["metric"] != "g2" {
		t.Fatalf("replaced good selector: %v", replaced["good"])
	}

	// GET returns the replacement.
	rec = sloRequest(t, h, http.MethodGet, "slo_a", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	if got := decodeBody(t, rec); got["comment"] != "c2" {
		t.Fatalf("get returned stale definition: %v", got)
	}

	// Listing sorted by id.
	putSLO(t, h, "slo_b", validSLOBody())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, slosPath, nil))
	list := decodeBody(t, rec)["slos"].([]any)
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	if list[0].(map[string]any)["id"] != "slo_a" || list[1].(map[string]any)["id"] != "slo_b" {
		t.Fatalf("list not sorted by id: %v", list)
	}

	// Missing single resource.
	expectErrorCode(t, sloRequest(t, h, http.MethodGet, "missing", "", ""), http.StatusNotFound, "slo_not_found")
	expectErrorCode(t, sloRequest(t, h, http.MethodDelete, "missing", "", ""), http.StatusNotFound, "slo_not_found")

	// Delete then 404.
	rec = sloRequest(t, h, http.MethodDelete, "slo_a", "", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%q", rec.Code, rec.Body.String())
	}
	expectErrorCode(t, sloRequest(t, h, http.MethodGet, "slo_a", "", ""), http.StatusNotFound, "slo_not_found")
}

func TestSLOInvalidBodies(t *testing.T) {
	h := Handler()

	cases := map[string]string{
		"missing objective":    `{"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"missing good":         `{"objective":0.9,"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"missing total":        `{"objective":0.9,"good":{"metric":"g","labels":{}},"comment":"c"}`,
		"missing comment":      `{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}}}`,
		"extra field":          `{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c","extra":1}`,
		"objective negative":   `{"objective":-0.1,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"objective above one":  `{"objective":1.01,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"objective wrong type": `{"objective":"0.9","good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"null labels":          `{"objective":0.9,"good":{"metric":"g","labels":null},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"bad metric":           `{"objective":0.9,"good":{"metric":"1bad","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"bad label key":        `{"objective":0.9,"good":{"metric":"g","labels":{"a.b":"v"}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"missing side part":    `{"objective":0.9,"good":{"labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"extra side field":     `{"objective":0.9,"good":{"metric":"g","labels":{},"x":1},"total":{"metric":"t","labels":{}},"comment":"c"}`,
		"comment null":         `{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":null}`,
		"not an object":        `[1,2,3]`,
		"multiple values":      `{"objective":0.9,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"c"}{}`,
	}
	for name, body := range cases {
		rec := putSLO(t, h, "bad", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400, body=%q", name, rec.Code, rec.Body.String())
			continue
		}
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_slo")
	}

	// Boundary objectives 0 and 1 are valid.
	for name, obj := range map[string]string{"zero": "0", "one": "1", "half": "0.5"} {
		body := `{"objective":` + obj + `,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":""}`
		if rec := putSLO(t, h, "bound_"+name, body); rec.Code != http.StatusCreated {
			t.Errorf("objective %s: status = %d body=%q", obj, rec.Code, rec.Body.String())
		}
	}
}

func TestSLOFailedReplaceKeepsOld(t *testing.T) {
	h := Handler()
	putSLO(t, h, "slo_a", validSLOBody())

	rec := putSLO(t, h, "slo_a", `{"objective":2,"good":{"metric":"g","labels":{}},"total":{"metric":"t","labels":{}},"comment":"x"}`)
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_slo")

	rec = sloRequest(t, h, http.MethodGet, "slo_a", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get after failed replace = %d", rec.Code)
	}
	if got := decodeBody(t, rec); got["comment"] != "c1" || got["objective"] != 0.9 {
		t.Fatalf("old definition changed: %v", got)
	}
}

func TestSLOMediaAndMethods(t *testing.T) {
	h := Handler()

	// Non-JSON media type -> 415.
	rec := sloRequest(t, h, http.MethodPut, "slo_a", validSLOBody(), "text/plain")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")

	// Invalid id in path on PUT -> 400 invalid_slo.
	rec = putSLO(t, h, "1bad", validSLOBody())
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_slo")
	// Invalid id on GET -> 404.
	expectErrorCode(t, sloRequest(t, h, http.MethodGet, "1bad", "", ""), http.StatusNotFound, "slo_not_found")
	// Nested path -> 404.
	expectErrorCode(t, sloRequest(t, h, http.MethodGet, "a/b", "", ""), http.StatusNotFound, "slo_not_found")

	// Method not allowed.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, slosPath, nil))
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Errorf("collection Allow = %q, want GET", got)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, slosPath+"/x", nil))
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if got := rec.Header().Get("Allow"); got != "GET, PUT, DELETE" {
		t.Errorf("item Allow = %q, want GET, PUT, DELETE", got)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, sloStatusPath, nil))
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestSLOStatusEvaluation(t *testing.T) {
	h := Handler()

	// good=90 across two matching series; total=100.
	post := func(body string) {
		t.Helper()
		rec := postMetrics(t, h, body, "application/json")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("post metrics = %d body=%q", rec.Code, rec.Body.String())
		}
	}
	post(`{"samples":[
		{"name":"good","type":"counter","labels":{"svc":"a","route":"/x"},"value":60},
		{"name":"good","type":"counter","labels":{"svc":"a","route":"/y"},"value":30},
		{"name":"good","type":"counter","labels":{"svc":"other"},"value":1000},
		{"name":"total","type":"counter","labels":{"svc":"a"},"value":100},
		{"name":"good","type":"gauge","labels":{"svc":"a"},"value":999},
		{"name":"total","type":"histogram","labels":{"svc":"h"},"value":1,"buckets":[1,2]}
	]}`)

	// met: compliance 0.9 >= objective 0.9.
	putSLO(t, h, "s1", `{"objective":0.9,
		"good":{"metric":"good","labels":{"svc":"a"}},
		"total":{"metric":"total","labels":{"svc":"a"}},
		"comment":""}`)
	// breached: 0.9 < 0.95; budget remaining negative.
	putSLO(t, h, "s2", `{"objective":0.95,
		"good":{"metric":"good","labels":{"svc":"a"}},
		"total":{"metric":"total","labels":{"svc":"a"}},
		"comment":""}`)
	// no_data: good side matches only a gauge.
	putSLO(t, h, "s3", `{"objective":0.9,
		"good":{"metric":"good","labels":{"svc":"gaugeside"}},
		"total":{"metric":"total","labels":{"svc":"a"}},
		"comment":""}`)
	// no_data: total side matches nothing at all.
	putSLO(t, h, "s4", `{"objective":0.9,
		"good":{"metric":"good","labels":{"svc":"a"}},
		"total":{"metric":"nope","labels":{}},
		"comment":""}`)
	// no_data: total counters sum to zero.
	post(`{"samples":[{"name":"zero_total","type":"counter","labels":{"svc":"a"},"value":0}]}`)
	putSLO(t, h, "s5", `{"objective":0.9,
		"good":{"metric":"good","labels":{"svc":"a"}},
		"total":{"metric":"zero_total","labels":{"svc":"a"}},
		"comment":""}`)
	// invalid_data: good > total.
	putSLO(t, h, "s6", `{"objective":0.9,
		"good":{"metric":"total","labels":{"svc":"a"}},
		"total":{"metric":"good","labels":{"svc":"a"}},
		"comment":""}`)
	// histogram-only side is no_data.
	putSLO(t, h, "s7", `{"objective":0.9,
		"good":{"metric":"good","labels":{"svc":"a"}},
		"total":{"metric":"total","labels":{"svc":"h"}},
		"comment":""}`)

	status := getSLOStatus(t, h)["slos"].([]any)
	if len(status) != 7 {
		t.Fatalf("status len = %d, want 7: %v", len(status), status)
	}
	byID := map[string]map[string]any{}
	for _, item := range status {
		m := item.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if ids := []string{
		status[0].(map[string]any)["id"].(string),
		status[6].(map[string]any)["id"].(string),
	}; ids[0] != "s1" || ids[1] != "s7" {
		t.Fatalf("status not sorted by id: %v", ids)
	}

	s1 := byID["s1"]
	if s1["state"] != "met" {
		t.Fatalf("s1 state = %v", s1["state"])
	}
	if s1["good_events"] != 90.0 || s1["total_events"] != 100.0 || s1["compliance"] != 0.9 {
		t.Fatalf("s1 values = %v", s1)
	}
	if bt, _ := s1["error_budget_total"].(float64); bt < 9.99 || bt > 10.01 {
		t.Fatalf("s1 budget total = %v", s1["error_budget_total"])
	}
	if br, _ := s1["error_budget_remaining"].(float64); br < -1e-9 || br > 1e-9 {
		t.Fatalf("s1 budget remaining = %v, want ~0", s1["error_budget_remaining"])
	}
	if br, _ := s1["error_budget_remaining_ratio"].(float64); br < -1e-9 || br > 1e-9 {
		t.Fatalf("s1 budget ratio = %v, want ~0", s1["error_budget_remaining_ratio"])
	}

	s2 := byID["s2"]
	if s2["state"] != "breached" {
		t.Fatalf("s2 state = %v", s2["state"])
	}
	if bt, _ := s2["error_budget_total"].(float64); bt < 4.99 || bt > 5.01 {
		t.Fatalf("s2 budget total = %v", s2["error_budget_total"])
	}
	if br, _ := s2["error_budget_remaining"].(float64); br < -5.01 || br > -4.99 {
		t.Fatalf("s2 budget remaining = %v, want -5", s2["error_budget_remaining"])
	}
	if rr, _ := s2["error_budget_remaining_ratio"].(float64); rr < -1.01 || rr > -0.99 {
		t.Fatalf("s2 budget ratio = %v, want -1", s2["error_budget_remaining_ratio"])
	}

	for _, id := range []string{"s3", "s4", "s5", "s7"} {
		m := byID[id]
		if m["state"] != "no_data" {
			t.Fatalf("%s state = %v, want no_data", id, m["state"])
		}
		for _, field := range []string{
			"good_events", "total_events", "compliance",
			"error_budget_total", "error_budget_remaining", "error_budget_remaining_ratio",
		} {
			if m[field] != nil {
				t.Fatalf("%s %s = %v, want null", id, field, m[field])
			}
		}
	}

	s6 := byID["s6"]
	if s6["state"] != "invalid_data" {
		t.Fatalf("s6 state = %v, want invalid_data", s6["state"])
	}
	for _, field := range []string{
		"good_events", "total_events", "compliance",
		"error_budget_total", "error_budget_remaining", "error_budget_remaining_ratio",
	} {
		if s6[field] != nil {
			t.Fatalf("s6 %s = %v, want null", field, s6[field])
		}
	}
}

func TestSLOStatusEmpty(t *testing.T) {
	h := Handler()
	status := getSLOStatus(t, h)["slos"].([]any)
	if len(status) != 0 {
		t.Fatalf("empty status = %v", status)
	}
}
