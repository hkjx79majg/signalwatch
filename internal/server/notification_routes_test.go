package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func routeRequest(t *testing.T, h http.Handler, method, id, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	target := notificationRoutesPath
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

func putRoute(t *testing.T, h http.Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return routeRequest(t, h, http.MethodPut, id, body, "application/json")
}

func getPlan(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, notificationPlanPath, nil))
	return rec
}

func TestNotificationRouteLifecycle(t *testing.T) {
	h := Handler()

	body := `{"rule_ids":["rule_a","rule_b"],"receiver":"team_a","priority":10,"comment":"c1"}`
	rec := putRoute(t, h, "nr1", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%q", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)
	if created["id"] != "nr1" || created["receiver"] != "team_a" ||
		created["priority"] != float64(10) || created["comment"] != "c1" {
		t.Fatalf("unexpected create payload: %v", created)
	}
	ids := created["rule_ids"].([]any)
	if len(ids) != 2 || ids[0] != "rule_a" || ids[1] != "rule_b" {
		t.Fatalf("unexpected rule_ids: %v", created)
	}

	// Routes may reference alert rules that do not exist.
	if rec := putRoute(t, h, "ghost", `{"rule_ids":["nope"],"receiver":"r","priority":0,"comment":null}`); rec.Code != http.StatusCreated {
		t.Fatalf("ghost reference create = %d body=%q", rec.Code, rec.Body.String())
	}

	// Atomic replace -> 200.
	body2 := `{"rule_ids":["other"],"receiver":"team_b","priority":1000,"comment":""}`
	rec = putRoute(t, h, "nr1", body2)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body=%q", rec.Code, rec.Body.String())
	}
	replaced := decodeBody(t, rec)
	if replaced["receiver"] != "team_b" || replaced["priority"] != float64(1000) ||
		replaced["comment"] != "" || replaced["rule_ids"].([]any)[0] != "other" {
		t.Fatalf("replaced route = %v", rec.Body.String())
	}

	// GET returns the replacement with the path id.
	rec = routeRequest(t, h, http.MethodGet, "nr1", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	if got := decodeBody(t, rec); got["id"] != "nr1" || got["receiver"] != "team_b" {
		t.Fatalf("get route = %v", rec.Body.String())
	}

	// Collection sorted by id.
	rec = routeRequest(t, h, http.MethodGet, "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	routes := decodeBody(t, rec)["routes"].([]any)
	if len(routes) != 2 || routes[0].(map[string]any)["id"] != "ghost" ||
		routes[1].(map[string]any)["id"] != "nr1" {
		t.Fatalf("routes order = %v", routes)
	}

	// DELETE then 404s.
	rec = routeRequest(t, h, http.MethodDelete, "nr1", "", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d body=%q", rec.Code, rec.Body.String())
	}
	expectErrorCode(t, routeRequest(t, h, http.MethodGet, "nr1", "", ""), http.StatusNotFound, "notification_route_not_found")
	expectErrorCode(t, routeRequest(t, h, http.MethodDelete, "nr1", "", ""), http.StatusNotFound, "notification_route_not_found")
}

func TestNotificationRouteEmptyCollection(t *testing.T) {
	h := Handler()
	rec := routeRequest(t, h, http.MethodGet, "", "", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"routes":[]}` {
		t.Fatalf("empty routes = %d %q", rec.Code, rec.Body.String())
	}
	rec = getPlan(t, h)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"deliveries":[],"unrouted_alert_ids":[]}` {
		t.Fatalf("empty plan = %d %q", rec.Code, rec.Body.String())
	}
}

func TestNotificationRouteInvalidBodies(t *testing.T) {
	h := Handler()
	valid := `{"rule_ids":["rule_a"],"receiver":"team_a","priority":10,"comment":"c"}`

	cases := map[string]string{
		"empty rule_ids":       `{"rule_ids":[],"receiver":"team_a","priority":10,"comment":"c"}`,
		"duplicate rule_ids":   `{"rule_ids":["a","a"],"receiver":"team_a","priority":10,"comment":"c"}`,
		"bad rule id":          `{"rule_ids":["1bad"],"receiver":"team_a","priority":10,"comment":"c"}`,
		"bad receiver":         `{"rule_ids":["a"],"receiver":"not/a","priority":10,"comment":"c"}`,
		"negative priority":    `{"rule_ids":["a"],"receiver":"team_a","priority":-1,"comment":"c"}`,
		"priority over 1000":   `{"rule_ids":["a"],"receiver":"team_a","priority":1001,"comment":"c"}`,
		"fractional priority":  `{"rule_ids":["a"],"receiver":"team_a","priority":5.5,"comment":"c"}`,
		"string priority":      `{"rule_ids":["a"],"receiver":"team_a","priority":"10","comment":"c"}`,
		"numeric comment":      `{"rule_ids":["a"],"receiver":"team_a","priority":10,"comment":1}`,
		"missing rule_ids":     `{"receiver":"team_a","priority":10,"comment":"c"}`,
		"missing receiver":     `{"rule_ids":["a"],"priority":10,"comment":"c"}`,
		"missing priority":     `{"rule_ids":["a"],"receiver":"team_a","comment":"c"}`,
		"missing comment":      `{"rule_ids":["a"],"receiver":"team_a","priority":10}`,
		"unknown field":        `{"rule_ids":["a"],"receiver":"team_a","priority":10,"comment":"c","extra":1}`,
		"not an object":        `[1,2,3]`,
		"malformed json":       `{"rule_ids":`,
		"multiple json values": valid + "\n" + valid,
		"rule_ids wrong type":  `{"rule_ids":"a","receiver":"team_a","priority":10,"comment":"c"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := putRoute(t, h, "nr_bad", body)
			expectErrorCode(t, rec, http.StatusBadRequest, "invalid_notification_route")
		})
	}

	// A failed create leaves nothing behind.
	if rec := routeRequest(t, h, http.MethodGet, "nr_bad", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("failed create persisted route: %d", rec.Code)
	}

	// Failed replacement must not change the existing route.
	if rec := putRoute(t, h, "nr_keep", valid); rec.Code != http.StatusCreated {
		t.Fatalf("setup create = %d", rec.Code)
	}
	if rec := putRoute(t, h, "nr_keep", cases["negative priority"]); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid replace = %d", rec.Code)
	}
	got := decodeBody(t, routeRequest(t, h, http.MethodGet, "nr_keep", "", ""))
	if got["receiver"] != "team_a" || got["priority"] != float64(10) || got["comment"] != "c" {
		t.Fatalf("route mutated by failed replace: %v", got)
	}
}

func TestNotificationRouteNullComment(t *testing.T) {
	h := Handler()
	rec := putRoute(t, h, "nr_null", `{"rule_ids":["a"],"receiver":"team_a","priority":0,"comment":null}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("null comment create = %d body=%q", rec.Code, rec.Body.String())
	}
	var parsed struct {
		Comment any `json:"comment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Comment != nil {
		t.Fatalf("comment = %v, want null", parsed.Comment)
	}

	// GET echoes null as well.
	rec = routeRequest(t, h, http.MethodGet, "nr_null", "", "")
	parsed.Comment = "sentinel"
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Comment != nil {
		t.Fatalf("comment = %v, want null", parsed.Comment)
	}
}

func TestNotificationRouteUnsupportedMediaType(t *testing.T) {
	h := Handler()
	rec := routeRequest(t, h, http.MethodPut, "nr1", `{}`, "text/plain")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")
	rec = routeRequest(t, h, http.MethodPut, "nr1", `{}`, "")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")
}

func TestNotificationRouteMethodNotAllowed(t *testing.T) {
	h := Handler()

	rec := routeRequest(t, h, http.MethodPost, "", `{}`, "application/json")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("collection Allow = %q, want GET", got)
	}

	rec = routeRequest(t, h, http.MethodPatch, "nr1", `{}`, "application/json")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if got := rec.Header().Get("Allow"); got != "GET, PUT, DELETE" {
		t.Fatalf("item Allow = %q, want GET, PUT, DELETE", got)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, notificationPlanPath, nil))
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("plan Allow = %q, want GET", got)
	}
}

func TestNotificationRoutePathShape(t *testing.T) {
	h := Handler()
	// Nested paths are outside the item subtree: not found.
	expectErrorCode(t, routeRequest(t, h, http.MethodGet, "nr1/child", "", ""), http.StatusNotFound, "notification_route_not_found")
	delNested := httptest.NewRecorder()
	h.ServeHTTP(delNested, httptest.NewRequest(http.MethodDelete, notificationRoutesPath+"/nr1/child", nil))
	expectErrorCode(t, delNested, http.StatusNotFound, "notification_route_not_found")
	// An identifier-shaped but syntactically invalid id on PUT is a bad route.
	expectErrorCode(t, putRoute(t, h, "1bad", `{"rule_ids":["a"],"receiver":"r","priority":0,"comment":""}`),
		http.StatusBadRequest, "invalid_notification_route")
	// Same invalid id on GET/DELETE is simply not found.
	expectErrorCode(t, routeRequest(t, h, http.MethodGet, "1bad", "", ""), http.StatusNotFound, "notification_route_not_found")
}

func TestNotificationPlanSelection(t *testing.T) {
	h := Handler()
	seedFiringRules(t, h, "a_fire", "b_fire", "c_fire")

	// d_idle is inactive and must never appear.
	postMetrics(t, h, `{"samples":[{"name":"d_idle_m","type":"gauge","labels":{},"value":0}]}`, "")
	putRule(t, h, "d_idle", `{"kind":"threshold","operator":"gt","threshold":1,"metric":"d_idle_m","labels":{}}`)

	// a_fire: covered by r_high (priority 5) and r_low (priority 10) -> r_high.
	// b_fire: covered by r_b2 and r_low, both priority 10 -> id tie -> r_b2.
	// c_fire: no covering route -> unrouted.
	// d_idle is inactive and must never appear.
	mustPut := func(id, body string) {
		t.Helper()
		if rec := putRoute(t, h, id, body); rec.Code != http.StatusCreated {
			t.Fatalf("route %s = %d %q", id, rec.Code, rec.Body.String())
		}
	}
	mustPut("r_low", `{"rule_ids":["a_fire","b_fire"],"receiver":"team_low","priority":10,"comment":""}`)
	mustPut("r_high", `{"rule_ids":["a_fire"],"receiver":"team_high","priority":5,"comment":""}`)
	mustPut("r_b2", `{"rule_ids":["b_fire"],"receiver":"team_b","priority":10,"comment":""}`)

	rec := getPlan(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("plan status = %d body=%q", rec.Code, rec.Body.String())
	}
	plan := decodeBody(t, rec)

	deliveries := plan["deliveries"].([]any)
	if len(deliveries) != 2 {
		t.Fatalf("deliveries = %v", deliveries)
	}
	d0 := deliveries[0].(map[string]any)
	d1 := deliveries[1].(map[string]any)
	if d0["alert_id"] != "a_fire" || d0["receiver"] != "team_high" || d0["route_id"] != "r_high" {
		t.Fatalf("delivery[0] = %v", d0)
	}
	if d1["alert_id"] != "b_fire" || d1["receiver"] != "team_b" || d1["route_id"] != "r_b2" {
		t.Fatalf("delivery[1] = %v", d1)
	}
	for _, d := range deliveries {
		m := d.(map[string]any)
		if len(m) != 3 {
			t.Fatalf("delivery has extra fields: %v", m)
		}
	}

	unrouted := plan["unrouted_alert_ids"].([]any)
	if len(unrouted) != 1 || unrouted[0] != "c_fire" {
		t.Fatalf("unrouted = %v", unrouted)
	}
}

func TestNotificationPlanExcludesSilencedAndInhibited(t *testing.T) {
	h := Handler()
	seedFiringRules(t, h, "a_fire", "b_fire", "c_fire", "src_fire")

	// Silence b_fire across the whole test window.
	silBody := `{"rule_ids":["b_fire"],"starts_at":"2000-01-01T00:00:00Z","ends_at":"2100-01-01T00:00:00Z","comment":"mute"}`
	if rec := silenceRequest(t, h, http.MethodPut, "sil1", silBody); rec.Code != http.StatusCreated {
		t.Fatalf("silence create = %d %q", rec.Code, rec.Body.String())
	}
	// src_fire inhibits c_fire.
	inhBody := `{"source_rule_ids":["src_fire"],"target_rule_ids":["c_fire"],"comment":"inh"}`
	if rec := putInhibit(t, h, "inh1", inhBody); rec.Code != http.StatusCreated {
		t.Fatalf("inhibit create = %d %q", rec.Code, rec.Body.String())
	}
	// One route covers everything.
	if rec := putRoute(t, h, "r_all", `{"rule_ids":["a_fire","b_fire","c_fire","src_fire","d_idle"],"receiver":"team","priority":0,"comment":""}`); rec.Code != http.StatusCreated {
		t.Fatalf("route create = %d", rec.Code)
	}

	plan := decodeBody(t, getPlan(t, h))
	deliveries := plan["deliveries"].([]any)
	got := make(map[string]map[string]any, len(deliveries))
	for _, d := range deliveries {
		m := d.(map[string]any)
		got[m["alert_id"].(string)] = m
	}
	if len(got) != 2 {
		t.Fatalf("deliveries = %v", deliveries)
	}
	if _, ok := got["a_fire"]; !ok {
		t.Fatalf("a_fire missing: %v", got)
	}
	if _, ok := got["src_fire"]; !ok {
		t.Fatalf("src_fire (an inhibition source) missing: %v", got)
	}
	if _, bad := got["b_fire"]; bad {
		t.Fatalf("silenced b_fire was delivered: %v", got)
	}
	if _, bad := got["c_fire"]; bad {
		t.Fatalf("inhibited c_fire was delivered: %v", got)
	}
	if unrouted := plan["unrouted_alert_ids"].([]any); len(unrouted) != 0 {
		t.Fatalf("unrouted = %v, want []", unrouted)
	}
}
