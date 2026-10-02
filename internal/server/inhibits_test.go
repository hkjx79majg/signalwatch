package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func inhibitRequest(t *testing.T, h http.Handler, method, id, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	target := inhibitRulesPath + "/" + id
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func putInhibitRule(t *testing.T, h http.Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return inhibitRequest(t, h, http.MethodPut, id, body, "application/json")
}

func TestInhibitRuleLifecycle(t *testing.T) {
	h := Handler()

	body := `{"source_rule_ids":["root_a"],"target_rule_ids":["child_b","child_c"],"comment":"root cause"}`
	rec := putInhibitRule(t, h, "inh1", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%q", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)
	if created["id"] != "inh1" || created["comment"] != "root cause" {
		t.Fatalf("unexpected create payload: %v", created)
	}
	srcs := created["source_rule_ids"].([]any)
	tgts := created["target_rule_ids"].([]any)
	if len(srcs) != 1 || srcs[0] != "root_a" || len(tgts) != 2 || tgts[0] != "child_b" || tgts[1] != "child_c" {
		t.Fatalf("unexpected id arrays: %v", created)
	}

	// Replace atomically: same id, different content -> 200.
	body2 := `{"source_rule_ids":["root_x"],"target_rule_ids":["child_y"],"comment":""}`
	rec = putInhibitRule(t, h, "inh1", body2)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["comment"]; got != "" {
		t.Fatalf("replaced rule = %v", rec.Body.String())
	}

	// GET returns the replacement.
	rec = inhibitRequest(t, h, http.MethodGet, "inh1", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	if got := decodeBody(t, rec)["source_rule_ids"].([]any)[0]; got != "root_x" {
		t.Fatalf("get rule = %v", rec.Body.String())
	}

	// Collection sorted by id.
	if rec := putInhibitRule(t, h, "aaa", body2); rec.Code != http.StatusCreated {
		t.Fatalf("second create status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, inhibitRulesPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	rules := decodeBody(t, rec)["rules"].([]any)
	if len(rules) != 2 || rules[0].(map[string]any)["id"] != "aaa" ||
		rules[1].(map[string]any)["id"] != "inh1" {
		t.Fatalf("rules order = %v", rules)
	}

	// DELETE then 404s.
	rec = inhibitRequest(t, h, http.MethodDelete, "inh1", "", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d body=%q", rec.Code, rec.Body.String())
	}
	expectErrorCode(t, inhibitRequest(t, h, http.MethodGet, "inh1", "", ""), http.StatusNotFound, "inhibit_rule_not_found")
	expectErrorCode(t, inhibitRequest(t, h, http.MethodDelete, "inh1", "", ""), http.StatusNotFound, "inhibit_rule_not_found")
}

func TestInhibitRuleEmptyCollection(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, inhibitRulesPath, nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"rules":[]}` {
		t.Fatalf("empty rules = %d %q", rec.Code, rec.Body.String())
	}
}

func TestInhibitRuleValidation(t *testing.T) {
	h := Handler()

	bad := []string{
		`{"source_rule_ids":[],"target_rule_ids":["b"],"comment":"c"}`,            // empty sources
		`{"source_rule_ids":["a"],"target_rule_ids":[],"comment":"c"}`,            // empty targets
		`{"source_rule_ids":["a","a"],"target_rule_ids":["b"],"comment":"c"}`,     // duplicate sources
		`{"source_rule_ids":["a"],"target_rule_ids":["b","b"],"comment":"c"}`,     // duplicate targets
		`{"source_rule_ids":["a","b"],"target_rule_ids":["b","c"],"comment":"c"}`, // cross
		`{"source_rule_ids":["1bad"],"target_rule_ids":["b"],"comment":"c"}`,      // invalid id
		`{"source_rule_ids":["a"],"target_rule_ids":["b"],"comment":"c","x":1}`,   // extra field
		`{"source_rule_ids":["a"],"target_rule_ids":["b"]}`,                       // missing comment
		`{"source_rule_ids":["a"],"comment":"c"}`,                                 // missing targets
		`{"source_rule_ids":"a","target_rule_ids":["b"],"comment":"c"}`,           // wrong type
		`{"source_rule_ids":[1],"target_rule_ids":["b"],"comment":"c"}`,           // wrong element type
		`{"source_rule_ids":["a"],"target_rule_ids":["b"],"comment":3}`,           // wrong comment type
		`["a"]`, // non-object
		`{"source_rule_ids":["a"],"target_rule_ids":["b"],"comment":"c"} {}`, // trailing JSON
		`null`, // null
	}
	for _, body := range bad {
		rec := putInhibitRule(t, h, "inh", body)
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_inhibit_rule")
	}

	// Failed replacement must not change the stored rule.
	good := `{"source_rule_ids":["a"],"target_rule_ids":["b"],"comment":"keep"}`
	if rec := putInhibitRule(t, h, "inh", good); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	expectErrorCode(t, putInhibitRule(t, h, "inh", `{"source_rule_ids":["a"],"target_rule_ids":["a"],"comment":"x"}`),
		http.StatusBadRequest, "invalid_inhibit_rule")
	rec := inhibitRequest(t, h, http.MethodGet, "inh", "", "")
	if got := decodeBody(t, rec)["comment"]; got != "keep" {
		t.Fatalf("rule after failed replace = %v", rec.Body.String())
	}

	// Invalid path id on PUT is a bad rule; on GET/DELETE it is not found.
	expectErrorCode(t, putInhibitRule(t, h, "1bad", good), http.StatusBadRequest, "invalid_inhibit_rule")
	expectErrorCode(t, inhibitRequest(t, h, http.MethodGet, "1bad", "", ""), http.StatusNotFound, "inhibit_rule_not_found")

	// Wrong media type.
	expectErrorCode(t, inhibitRequest(t, h, http.MethodPut, "inh2", good, "text/plain"),
		http.StatusUnsupportedMediaType, "unsupported_media_type")
}

func TestInhibitRuleMethods(t *testing.T) {
	h := Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, inhibitRulesPath, nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("collection POST = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, inhibitRulesPath+"/x", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, PUT, DELETE" {
		t.Fatalf("item POST = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestInhibitionEvaluation(t *testing.T) {
	h := Handler()

	// root fires on hits > 3; child fires on hits > 1; quiet never fires.
	putRule(t, h, "root", `{"kind":"threshold","operator":"gt","threshold":3,"metric":"hits","labels":{}}`)
	putRule(t, h, "child", `{"kind":"threshold","operator":"gt","threshold":1,"metric":"hits","labels":{}}`)
	putRule(t, h, "quiet", `{"kind":"threshold","operator":"gt","threshold":100,"metric":"hits","labels":{}}`)
	putRule(t, h, "nodata", `{"kind":"threshold","operator":"gt","threshold":1,"metric":"absent","labels":{}}`)

	if rec := postMetrics(t, h, `{"samples":[{"name":"hits","type":"gauge","labels":{},"value":5}]}`, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("seed = %d", rec.Code)
	}

	// Rules may reference alerts that do not exist yet.
	putInhibitRule(t, h, "z_inh", `{"source_rule_ids":["root","ghost"],"target_rule_ids":["child","quiet","nodata"],"comment":"c"}`)
	putInhibitRule(t, h, "a_inh", `{"source_rule_ids":["root"],"target_rule_ids":["child"],"comment":"c"}`)
	putInhibitRule(t, h, "miss", `{"source_rule_ids":["quiet"],"target_rule_ids":["child"],"comment":"c"}`)

	rec := getAlerts(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("alerts = %d", rec.Code)
	}
	alerts := decodeBody(t, rec)["alerts"].([]any)
	byID := make(map[string]map[string]any, len(alerts))
	for _, a := range alerts {
		m := a.(map[string]any)
		byID[m["id"].(string)] = m
	}

	child := byID["child"]
	if child["state"] != "firing" || child["inhibited"] != true {
		t.Fatalf("child = %v", child)
	}
	ids := child["inhibition_ids"].([]any)
	if len(ids) != 2 || ids[0] != "a_inh" || ids[1] != "z_inh" {
		t.Fatalf("child inhibition_ids = %v", ids)
	}

	root := byID["root"]
	if root["state"] != "firing" || root["inhibited"] != false ||
		len(root["inhibition_ids"].([]any)) != 0 {
		t.Fatalf("root = %v", root)
	}

	quiet := byID["quiet"]
	if quiet["state"] != "inactive" || quiet["inhibited"] != false ||
		len(quiet["inhibition_ids"].([]any)) != 0 {
		t.Fatalf("quiet = %v", quiet)
	}

	nodata := byID["nodata"]
	if nodata["state"] != "no_data" || nodata["inhibited"] != false ||
		len(nodata["inhibition_ids"].([]any)) != 0 {
		t.Fatalf("nodata = %v", nodata)
	}
}

func TestInhibitionSilencedAndInhibitedSourcesStillTrigger(t *testing.T) {
	h := Handler()

	putRule(t, h, "root", `{"kind":"threshold","operator":"gt","threshold":3,"metric":"hits","labels":{}}`)
	putRule(t, h, "mid", `{"kind":"threshold","operator":"gt","threshold":2,"metric":"hits","labels":{}}`)
	putRule(t, h, "leaf", `{"kind":"threshold","operator":"gt","threshold":1,"metric":"hits","labels":{}}`)
	if rec := postMetrics(t, h, `{"samples":[{"name":"hits","type":"gauge","labels":{},"value":5}]}`, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("seed = %d", rec.Code)
	}

	// mid is inhibited by root; leaf is inhibited by mid (itself inhibited)
	// and by root (silenced). Both still count as firing sources.
	putInhibitRule(t, h, "inh_mid", `{"source_rule_ids":["root"],"target_rule_ids":["mid"],"comment":"c"}`)
	putInhibitRule(t, h, "inh_leaf", `{"source_rule_ids":["mid","root"],"target_rule_ids":["leaf"],"comment":"c"}`)

	// Silence root: silenced sources still trigger inhibition.
	silBody := `{"rule_ids":["root"],"starts_at":"2020-01-01T00:00:00Z","ends_at":"2099-01-01T00:00:00Z","comment":"s"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, silencesPath+"/sil1", strings.NewReader(silBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("silence = %d %q", rec.Code, rec.Body.String())
	}

	rec = getAlerts(t, h)
	alerts := decodeBody(t, rec)["alerts"].([]any)
	byID := make(map[string]map[string]any, len(alerts))
	for _, a := range alerts {
		m := a.(map[string]any)
		byID[m["id"].(string)] = m
	}

	root := byID["root"]
	if root["state"] != "firing" || root["silenced"] != true || root["inhibited"] != false {
		t.Fatalf("root = %v", root)
	}
	mid := byID["mid"]
	if mid["state"] != "firing" || mid["inhibited"] != true ||
		len(mid["inhibition_ids"].([]any)) != 1 || mid["inhibition_ids"].([]any)[0] != "inh_mid" {
		t.Fatalf("mid = %v", mid)
	}
	leaf := byID["leaf"]
	if leaf["state"] != "firing" || leaf["inhibited"] != true ||
		len(leaf["inhibition_ids"].([]any)) != 1 || leaf["inhibition_ids"].([]any)[0] != "inh_leaf" {
		t.Fatalf("leaf = %v", leaf)
	}
}
