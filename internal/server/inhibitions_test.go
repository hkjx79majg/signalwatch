package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func inhibitRequest(t *testing.T, h http.Handler, method, id, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	target := inhibitRulesPath
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

func putInhibit(t *testing.T, h http.Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return inhibitRequest(t, h, http.MethodPut, id, body, "application/json")
}

func TestInhibitRuleLifecycle(t *testing.T) {
	h := Handler()

	body := `{"source_rule_ids":["src_a","src_b"],"target_rule_ids":["tgt_a"],"comment":"c1"}`
	rec := putInhibit(t, h, "ih1", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%q", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)
	if created["id"] != "ih1" || created["comment"] != "c1" {
		t.Fatalf("unexpected create payload: %v", created)
	}
	srcs := created["source_rule_ids"].([]any)
	tgts := created["target_rule_ids"].([]any)
	if len(srcs) != 2 || srcs[0] != "src_a" || srcs[1] != "src_b" ||
		len(tgts) != 1 || tgts[0] != "tgt_a" {
		t.Fatalf("unexpected id lists: %v", created)
	}

	// Atomic replace: same id, different content -> 200.
	body2 := `{"source_rule_ids":["other"],"target_rule_ids":["t1","t2"],"comment":""}`
	rec = putInhibit(t, h, "ih1", body2)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["comment"]; got != "" {
		t.Fatalf("replaced rule = %v", rec.Body.String())
	}

	// GET returns the replacement with the path id.
	rec = inhibitRequest(t, h, http.MethodGet, "ih1", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	got := decodeBody(t, rec)
	if got["id"] != "ih1" || got["source_rule_ids"].([]any)[0] != "other" {
		t.Fatalf("get rule = %v", rec.Body.String())
	}

	// Collection sorted by id.
	if rec := putInhibit(t, h, "alpha", body2); rec.Code != http.StatusCreated {
		t.Fatalf("second create status = %d", rec.Code)
	}
	rec = inhibitRequest(t, h, http.MethodGet, "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	rules := decodeBody(t, rec)["rules"].([]any)
	if len(rules) != 2 || rules[0].(map[string]any)["id"] != "alpha" ||
		rules[1].(map[string]any)["id"] != "ih1" {
		t.Fatalf("rules order = %v", rules)
	}

	// DELETE then 404s.
	rec = inhibitRequest(t, h, http.MethodDelete, "ih1", "", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d body=%q", rec.Code, rec.Body.String())
	}
	expectErrorCode(t, inhibitRequest(t, h, http.MethodGet, "ih1", "", ""), http.StatusNotFound, "inhibit_rule_not_found")
	expectErrorCode(t, inhibitRequest(t, h, http.MethodDelete, "ih1", "", ""), http.StatusNotFound, "inhibit_rule_not_found")
}

func TestInhibitRuleEmptyCollection(t *testing.T) {
	h := Handler()
	rec := inhibitRequest(t, h, http.MethodGet, "", "", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"rules":[]}` {
		t.Fatalf("empty rules = %d %q", rec.Code, rec.Body.String())
	}
}

func TestInvalidInhibitRuleBodies(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"not json":              `{`,
		"not object":            `[1]`,
		"null":                  `null`,
		"two values":            `{"source_rule_ids":["s"],"target_rule_ids":["t"],"comment":"c"} {}`,
		"unknown field":         `{"source_rule_ids":["s"],"target_rule_ids":["t"],"comment":"c","extra":1}`,
		"id in body":            `{"id":"x","source_rule_ids":["s"],"target_rule_ids":["t"],"comment":"c"}`,
		"missing sources":       `{"target_rule_ids":["t"],"comment":"c"}`,
		"missing targets":       `{"source_rule_ids":["s"],"comment":"c"}`,
		"missing comment":       `{"source_rule_ids":["s"],"target_rule_ids":["t"]}`,
		"null sources":          `{"source_rule_ids":null,"target_rule_ids":["t"],"comment":"c"}`,
		"null targets":          `{"source_rule_ids":["s"],"target_rule_ids":null,"comment":"c"}`,
		"null comment":          `{"source_rule_ids":["s"],"target_rule_ids":["t"],"comment":null}`,
		"empty sources":         `{"source_rule_ids":[],"target_rule_ids":["t"],"comment":"c"}`,
		"empty targets":         `{"source_rule_ids":["s"],"target_rule_ids":[],"comment":"c"}`,
		"duplicate sources":     `{"source_rule_ids":["s","s"],"target_rule_ids":["t"],"comment":"c"}`,
		"duplicate targets":     `{"source_rule_ids":["s"],"target_rule_ids":["t","t"],"comment":"c"}`,
		"intersection":          `{"source_rule_ids":["a","b"],"target_rule_ids":["b","c"],"comment":"c"}`,
		"bad source id":         `{"source_rule_ids":["1s"],"target_rule_ids":["t"],"comment":"c"}`,
		"bad target id":         `{"source_rule_ids":["s"],"target_rule_ids":["t-"],"comment":"c"}`,
		"sources not array":     `{"source_rule_ids":"s","target_rule_ids":["t"],"comment":"c"}`,
		"targets not array":     `{"source_rule_ids":["s"],"target_rule_ids":"t","comment":"c"}`,
		"comment not string":    `{"source_rule_ids":["s"],"target_rule_ids":["t"],"comment":1}`,
		"source element number": `{"source_rule_ids":[1],"target_rule_ids":["t"],"comment":"c"}`,
		"source element null":   `{"source_rule_ids":[null],"target_rule_ids":["t"],"comment":"c"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			expectErrorCode(t, putInhibit(t, h, "r1", body), http.StatusBadRequest, "invalid_inhibit_rule")
		})
	}

	// A failed create leaves nothing behind; a failed replace keeps the old rule.
	if rec := inhibitRequest(t, h, http.MethodGet, "r1", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("r1 should not exist, got %d", rec.Code)
	}
	good := `{"source_rule_ids":["s"],"target_rule_ids":["t"],"comment":"keep"}`
	if rec := putInhibit(t, h, "keep", good); rec.Code != http.StatusCreated {
		t.Fatalf("setup = %d", rec.Code)
	}
	bad := `{"source_rule_ids":[],"target_rule_ids":["t"],"comment":"x"}`
	expectErrorCode(t, putInhibit(t, h, "keep", bad), http.StatusBadRequest, "invalid_inhibit_rule")
	rec := inhibitRequest(t, h, http.MethodGet, "keep", "", "")
	if got := decodeBody(t, rec)["comment"]; got != "keep" {
		t.Fatalf("original rule changed after invalid replace: %v", rec.Body.String())
	}
}

func TestInhibitRulePutMediaTypeAndMethods(t *testing.T) {
	h := Handler()
	body := `{"source_rule_ids":["s"],"target_rule_ids":["t"],"comment":"c"}`

	rec := inhibitRequest(t, h, http.MethodPut, "r1", body, "text/plain")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")
	rec = inhibitRequest(t, h, http.MethodPut, "r1", body, "")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")

	// Unsupported methods on item -> 405 with Allow.
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		rec := inhibitRequest(t, h, method, "r1", "", "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, PUT, DELETE" {
			t.Fatalf("%s item = %d allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	// Unsupported methods on collection -> 405 Allow GET.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := inhibitRequest(t, h, method, "", "", "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
			t.Fatalf("%s collection = %d allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

func TestInhibitRuleIDValidation(t *testing.T) {
	h := Handler()
	body := `{"source_rule_ids":["s"],"target_rule_ids":["t"],"comment":"c"}`
	for _, id := range []string{"1bad", "a-b"} {
		rec := inhibitRequest(t, h, http.MethodGet, id, "", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("get id %q = %d, want 404", id, rec.Code)
		}
		expectErrorCode(t, inhibitRequest(t, h, http.MethodPut, id, body, "application/json"),
			http.StatusBadRequest, "invalid_inhibit_rule")
		expectErrorCode(t, inhibitRequest(t, h, http.MethodDelete, id, "", ""),
			http.StatusNotFound, "inhibit_rule_not_found")
	}
	// Trailing slash / nested path must not be treated as an id.
	for _, target := range []string{inhibitRulesPrefix, inhibitRulesPrefix + "r1/extra"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("nested path %s = %d, want 404", target, rec.Code)
		}
	}
	// Valid ids create successfully.
	for _, id := range []string{"a", "_x", "A9_b"} {
		if rec := putInhibit(t, h, id, body); rec.Code != http.StatusCreated {
			t.Fatalf("valid id %q -> %d %q", id, rec.Code, rec.Body.String())
		}
	}
}

// seedInhibitionScenario creates independent metrics feeding one rule per
// alert id: every listed rule fires. Additional inactive/no_data rules can be
// derived by pointing at other metrics in individual tests.
func seedFiringRules(t *testing.T, h http.Handler, ids ...string) {
	t.Helper()
	samples := make([]string, 0, len(ids))
	for _, id := range ids {
		samples = append(samples, `{"name":"`+id+`_m","type":"gauge","labels":{},"value":10}`)
	}
	body := `{"samples":[` + strings.Join(samples, ",") + `]}`
	if rec := postMetrics(t, h, body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("seed metrics = %d %q", rec.Code, rec.Body.String())
	}
	for _, id := range ids {
		ruleBody := `{"kind":"threshold","operator":"gt","threshold":1,"metric":"` + id + `_m","labels":{}}`
		if rec := putRule(t, h, id, ruleBody); rec.Code != http.StatusCreated {
			t.Fatalf("seed rule %s = %d %q", id, rec.Code, rec.Body.String())
		}
	}
}

func alertMap(t *testing.T, h http.Handler) map[string]map[string]any {
	t.Helper()
	raw := alertsByID(t, getAlerts(t, h))
	out := make(map[string]map[string]any, len(raw))
	for id, v := range raw {
		out[id] = v.(map[string]any)
	}
	return out
}

func TestInhibitionHitsFiringTargets(t *testing.T) {
	h := Handler()
	seedFiringRules(t, h, "src", "tgt")

	// "quiet" is inactive: gauge 0, threshold gt 1.
	postMetrics(t, h, `{"samples":[{"name":"quiet_m","type":"gauge","labels":{},"value":0}]}`, "")
	putRule(t, h, "quiet", `{"kind":"threshold","operator":"gt","threshold":1,"metric":"quiet_m","labels":{}}`)
	// "blind" has no series at all -> no_data.
	putRule(t, h, "blind", `{"kind":"threshold","operator":"gt","threshold":1,"metric":"blind_m","labels":{}}`)

	// src firing inhibits tgt; quiet (inactive) and blind (no_data) are
	// targets too but must not be marked inhibited.
	body := `{"source_rule_ids":["src"],"target_rule_ids":["tgt","quiet","blind"],"comment":""}`
	if rec := putInhibit(t, h, "ih1", body); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}

	alerts := alertMap(t, h)
	a := alerts["tgt"]
	if a["state"] != "firing" || a["value"] != float64(10) || a["inhibited"] != true {
		t.Fatalf("target alert = %v", a)
	}
	if ids := a["inhibition_ids"].([]any); len(ids) != 1 || ids[0] != "ih1" {
		t.Fatalf("inhibition_ids = %v", ids)
	}
	for _, id := range []string{"quiet", "blind"} {
		a := alerts[id]
		if a["inhibited"] != false || len(a["inhibition_ids"].([]any)) != 0 {
			t.Fatalf("non-firing alert %s unexpectedly inhibited: %v", id, a)
		}
	}
	if a := alerts["src"]; a["inhibited"] != false {
		t.Fatalf("source should not inhibit itself: %v", a)
	}

	// Alert ordering stays by rule id: blind, quiet, src, tgt.
	gotOrder := alertIDs(t, getAlerts(t, h))
	want := []string{"blind", "quiet", "src", "tgt"}
	if strings.Join(gotOrder, ",") != strings.Join(want, ",") {
		t.Fatalf("alert order = %v, want %v", gotOrder, want)
	}
}

func TestInhibitionNeedsExistingFiringSource(t *testing.T) {
	h := Handler()
	seedFiringRules(t, h, "src", "tgt")

	// Rule references a not-yet-existing alert rule; no existing source fires
	// yet -> no inhibition.
	body := `{"source_rule_ids":["ghost"],"target_rule_ids":["tgt"],"comment":""}`
	putInhibit(t, h, "ih_ghost", body)
	if a := alertMap(t, h)["tgt"]; a["inhibited"] != false {
		t.Fatalf("nonexistent source must not inhibit: %v", a)
	}

	// Existing source firing + nonexistent source -> at least one existing
	// source fires, so the rule hits.
	body2 := `{"source_rule_ids":["ghost","src"],"target_rule_ids":["tgt"],"comment":""}`
	putInhibit(t, h, "ih_mix", body2)
	a := alertMap(t, h)["tgt"]
	if a["inhibited"] != true {
		t.Fatalf("mixed sources should inhibit: %v", a)
	}
	if ids := a["inhibition_ids"].([]any); len(ids) != 1 || ids[0] != "ih_mix" {
		t.Fatalf("inhibition_ids = %v, want [ih_mix]", ids)
	}

	// Source stops firing -> inhibition disappears.
	postMetrics(t, h, `{"samples":[{"name":"src_m","type":"gauge","labels":{},"value":0}]}`, "")
	if a := alertMap(t, h)["tgt"]; a["inhibited"] != false || len(a["inhibition_ids"].([]any)) != 0 {
		t.Fatalf("inactive source must not inhibit: %v", a)
	}
}

func TestSilencedOrInhibitedSourceStillTriggers(t *testing.T) {
	h := Handler()
	seedFiringRules(t, h, "root", "mid", "leaf")

	// root inhibits mid, mid inhibits leaf: a chain.
	putInhibit(t, h, "z_rule", `{"source_rule_ids":["root"],"target_rule_ids":["mid"],"comment":""}`)
	putInhibit(t, h, "a_rule", `{"source_rule_ids":["mid"],"target_rule_ids":["leaf"],"comment":""}`)

	// Silence root over a wide window; it must still act as a source.
	silBody := `{"rule_ids":["root"],
		"starts_at":"2000-01-01T00:00:00Z","ends_at":"2100-01-01T00:00:00Z","comment":"mute"}`
	rec := silenceRequest(t, h, http.MethodPut, "sil1", silBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("silence create = %d %q", rec.Code, rec.Body.String())
	}

	alerts := alertMap(t, h)
	root := alerts["root"]
	if root["silenced"] != true || root["inhibited"] != false {
		t.Fatalf("root = %v", root)
	}
	mid := alerts["mid"]
	// mid is itself inhibited, yet must still trigger the rule for leaf.
	if mid["state"] != "firing" || mid["inhibited"] != true ||
		mid["inhibition_ids"].([]any)[0] != "z_rule" {
		t.Fatalf("mid = %v", mid)
	}
	leaf := alerts["leaf"]
	if leaf["inhibited"] != true || leaf["inhibition_ids"].([]any)[0] != "a_rule" {
		t.Fatalf("leaf = %v", leaf)
	}
}

func TestInhibitionIDsSortedAndPreserveSilence(t *testing.T) {
	h := Handler()
	seedFiringRules(t, h, "src", "tgt")

	// Two rules hit the same target; ids must come back sorted.
	putInhibit(t, h, "z_rule", `{"source_rule_ids":["src"],"target_rule_ids":["tgt"],"comment":""}`)
	putInhibit(t, h, "a_rule", `{"source_rule_ids":["src"],"target_rule_ids":["tgt"],"comment":""}`)
	// Silence tgt concurrently: silenced and inhibited coexist.
	silBody := `{"rule_ids":["tgt"],
		"starts_at":"2000-01-01T00:00:00Z","ends_at":"2100-01-01T00:00:00Z","comment":"mute"}`
	if rec := silenceRequest(t, h, http.MethodPut, "sil1", silBody); rec.Code != http.StatusCreated {
		t.Fatalf("silence create = %d", rec.Code)
	}

	a := alertMap(t, h)["tgt"]
	if a["state"] != "firing" || a["silenced"] != true || a["inhibited"] != true {
		t.Fatalf("target = %v", a)
	}
	if ids := a["silence_ids"].([]any); len(ids) != 1 || ids[0] != "sil1" {
		t.Fatalf("silence_ids = %v", ids)
	}
	ids := a["inhibition_ids"].([]any)
	if len(ids) != 2 || ids[0] != "a_rule" || ids[1] != "z_rule" {
		t.Fatalf("inhibition_ids = %v, want sorted [a_rule z_rule]", ids)
	}
}

func silenceRequest(t *testing.T, h http.Handler, method, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, silencesPath+"/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}
