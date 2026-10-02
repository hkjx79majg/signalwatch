package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func silenceRequest(t *testing.T, h http.Handler, method, id, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	target := silencesPath
	if id != "" {
		target = silencesPath + "/" + id
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func putSilence(t *testing.T, h http.Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return silenceRequest(t, h, http.MethodPut, id, body, "application/json")
}

func listSilences(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	return silenceRequest(t, h, http.MethodGet, "", "", "")
}

func silenceBody(start, end time.Time, ids []string, comment string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = fmt.Sprintf("%q", id)
	}
	return fmt.Sprintf(`{"rule_ids":[%s],"starts_at":%q,"ends_at":%q,"comment":%q}`,
		strings.Join(quoted, ","), start.Format(time.RFC3339), end.Format(time.RFC3339), comment)
}

func TestSilenceLifecycle(t *testing.T) {
	h := Handler()
	now := time.Now()

	body := silenceBody(now.Add(-time.Hour), now.Add(time.Hour), []string{"r_other"}, "maintenance")
	rec := putSilence(t, h, "win1", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%q", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)
	if created["id"] != "win1" || created["comment"] != "maintenance" || created["status"] != "active" {
		t.Fatalf("unexpected create payload: %v", created)
	}
	if ids := created["rule_ids"].([]any); len(ids) != 1 || ids[0] != "r_other" {
		t.Fatalf("rule_ids = %v", created["rule_ids"])
	}
	if created["starts_at"] == nil || created["ends_at"] == nil {
		t.Fatalf("timestamps missing: %v", created)
	}

	// Replace atomically: same id, different content -> 200.
	body2 := silenceBody(now.Add(time.Hour), now.Add(2*time.Hour), []string{"r_other", "r2"}, "later")
	rec = putSilence(t, h, "win1", body2)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body=%q", rec.Code, rec.Body.String())
	}
	replaced := decodeBody(t, rec)
	if replaced["status"] != "upcoming" || replaced["comment"] != "later" {
		t.Fatalf("replaced payload = %v", replaced)
	}
	if ids := replaced["rule_ids"].([]any); len(ids) != 2 {
		t.Fatalf("replaced rule_ids = %v", replaced["rule_ids"])
	}

	// GET returns the replacement with upcoming status.
	rec = silenceRequest(t, h, http.MethodGet, "win1", "", "")
	if rec.Code != http.StatusOK || decodeBody(t, rec)["status"] != "upcoming" {
		t.Fatalf("get = %d %q", rec.Code, rec.Body.String())
	}

	// Expired silences are retained and reported as expired.
	putSilence(t, h, "old", silenceBody(now.Add(-2*time.Hour), now.Add(-time.Hour), []string{"r1"}, "past"))
	// Collection sorted by id, statuses computed from current time.
	rec = listSilences(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	list := decodeBody(t, rec)["silences"].([]any)
	if len(list) != 2 {
		t.Fatalf("silences = %v", list)
	}
	first := list[0].(map[string]any)
	second := list[1].(map[string]any)
	if first["id"] != "old" || first["status"] != "expired" {
		t.Fatalf("first = %v", first)
	}
	if second["id"] != "win1" || second["status"] != "upcoming" {
		t.Fatalf("second = %v", second)
	}

	// DELETE then 404s; the expired silence is only gone after explicit delete.
	rec = silenceRequest(t, h, http.MethodDelete, "old", "", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d body=%q", rec.Code, rec.Body.String())
	}
	expectErrorCode(t, silenceRequest(t, h, http.MethodGet, "old", "", ""), http.StatusNotFound, "silence_not_found")
	expectErrorCode(t, silenceRequest(t, h, http.MethodDelete, "old", "", ""), http.StatusNotFound, "silence_not_found")
}

func TestSilenceEmptyCollection(t *testing.T) {
	h := Handler()
	rec := listSilences(t, h)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"silences":[]}` {
		t.Fatalf("empty silences = %d %q", rec.Code, rec.Body.String())
	}
}

func TestSilenceAllowsUnknownRuleIDs(t *testing.T) {
	h := Handler()
	now := time.Now()
	// Maintenance windows may be scheduled before the rules exist.
	rec := putSilence(t, h, "win", silenceBody(now.Add(-time.Hour), now.Add(time.Hour), []string{"ghost_rule"}, "window"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with unknown rule id = %d %q", rec.Code, rec.Body.String())
	}
}

func TestInvalidSilenceBodies(t *testing.T) {
	h := Handler()
	now := time.Now()
	start := now.Add(-time.Hour).Format(time.RFC3339)
	end := now.Add(time.Hour).Format(time.RFC3339)
	cases := map[string]string{
		"not json":          `{`,
		"not object":        `[1]`,
		"null":              `null`,
		"two values":        `{"rule_ids":["r1"],"starts_at":"` + start + `","ends_at":"` + end + `","comment":"c"} {}`,
		"unknown field":     `{"rule_ids":["r1"],"starts_at":"` + start + `","ends_at":"` + end + `","comment":"c","extra":1}`,
		"missing rule_ids":  `{"starts_at":"` + start + `","ends_at":"` + end + `","comment":"c"}`,
		"missing starts_at": `{"rule_ids":["r1"],"ends_at":"` + end + `","comment":"c"}`,
		"missing ends_at":   `{"rule_ids":["r1"],"starts_at":"` + start + `","comment":"c"}`,
		"missing comment":   `{"rule_ids":["r1"],"starts_at":"` + start + `","ends_at":"` + end + `"}`,
		"null rule_ids":     `{"rule_ids":null,"starts_at":"` + start + `","ends_at":"` + end + `","comment":"c"}`,
		"empty rule_ids":    `{"rule_ids":[],"starts_at":"` + start + `","ends_at":"` + end + `","comment":"c"}`,
		"dup rule ids":      `{"rule_ids":["r1","r1"],"starts_at":"` + start + `","ends_at":"` + end + `","comment":"c"}`,
		"bad rule id":       `{"rule_ids":["1r"],"starts_at":"` + start + `","ends_at":"` + end + `","comment":"c"}`,
		"rule id number":    `{"rule_ids":[1],"starts_at":"` + start + `","ends_at":"` + end + `","comment":"c"}`,
		"starts not string": `{"rule_ids":["r1"],"starts_at":1,"ends_at":"` + end + `","comment":"c"}`,
		"comment number":    `{"rule_ids":["r1"],"starts_at":"` + start + `","ends_at":"` + end + `","comment":1}`,
		"comment null":      `{"rule_ids":["r1"],"starts_at":"` + start + `","ends_at":"` + end + `","comment":null}`,
		"bad timestamp":     `{"rule_ids":["r1"],"starts_at":"tomorrow","ends_at":"` + end + `","comment":"c"}`,
		"no timezone":       `{"rule_ids":["r1"],"starts_at":"2026-01-01T00:00:00","ends_at":"` + end + `","comment":"c"}`,
		"date only":         `{"rule_ids":["r1"],"starts_at":"2026-01-01","ends_at":"` + end + `","comment":"c"}`,
		"start equals end":  `{"rule_ids":["r1"],"starts_at":"` + start + `","ends_at":"` + start + `","comment":"c"}`,
		"start after end":   `{"rule_ids":["r1"],"starts_at":"` + end + `","ends_at":"` + start + `","comment":"c"}`,
		"id in body":        `{"id":"x","rule_ids":["r1"],"starts_at":"` + start + `","ends_at":"` + end + `","comment":"c"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			expectErrorCode(t, putSilence(t, h, "win", body), http.StatusBadRequest, "invalid_silence")
		})
	}

	// A failed create leaves nothing behind.
	if rec := silenceRequest(t, h, http.MethodGet, "win", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("win should not exist, got %d", rec.Code)
	}
	// A failed replace keeps the old silence unchanged.
	good := silenceBody(now.Add(-time.Hour), now.Add(time.Hour), []string{"keep_rule"}, "orig")
	if rec := putSilence(t, h, "keep", good); rec.Code != http.StatusCreated {
		t.Fatalf("setup = %d", rec.Code)
	}
	expectErrorCode(t, putSilence(t, h, "keep", `{"rule_ids":[]}`), http.StatusBadRequest, "invalid_silence")
	rec := silenceRequest(t, h, http.MethodGet, "keep", "", "")
	got := decodeBody(t, rec)
	if got["comment"] != "orig" {
		t.Fatalf("original silence changed after invalid replace: %v", rec.Body.String())
	}
	if ids := got["rule_ids"].([]any); len(ids) != 1 || ids[0] != "keep_rule" {
		t.Fatalf("original rule_ids changed: %v", rec.Body.String())
	}
}

func TestSilencePutMediaTypeAndMethods(t *testing.T) {
	h := Handler()
	now := time.Now()
	body := silenceBody(now.Add(-time.Hour), now.Add(time.Hour), []string{"r1"}, "c")

	rec := silenceRequest(t, h, http.MethodPut, "win", body, "text/plain")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")
	rec = silenceRequest(t, h, http.MethodPut, "win", body, "")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")

	// Unsupported methods on item -> 405 with Allow.
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		rec := silenceRequest(t, h, method, "win", "", "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, PUT, DELETE" {
			t.Fatalf("%s item = %d allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	// Unsupported methods on collection -> 405 Allow GET.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, silencesPath, nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
			t.Fatalf("%s collection = %d allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

func TestSilenceIDValidation(t *testing.T) {
	h := Handler()
	now := time.Now()
	body := silenceBody(now.Add(-time.Hour), now.Add(time.Hour), []string{"r1"}, "c")
	for _, id := range []string{"1bad", "a-b"} {
		rec := silenceRequest(t, h, http.MethodGet, id, "", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("get id %q = %d, want 404", id, rec.Code)
		}
		expectErrorCode(t, silenceRequest(t, h, http.MethodPut, id, body, "application/json"),
			http.StatusBadRequest, "invalid_silence")
		expectErrorCode(t, silenceRequest(t, h, http.MethodDelete, id, "", ""),
			http.StatusNotFound, "silence_not_found")
	}
	// Trailing slash / nested path must not be treated as an id.
	for _, target := range []string{silencesPrefix, silencesPrefix + "win/extra"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("nested path %s = %d, want 404", target, rec.Code)
		}
	}
	// Valid ids create successfully.
	for _, id := range []string{"a", "_x", "A9_b"} {
		if rec := putSilence(t, h, id, body); rec.Code != http.StatusCreated {
			t.Fatalf("valid id %q -> %d %q", id, rec.Code, rec.Body.String())
		}
	}
}

func seedFiringRule(t *testing.T, h http.Handler, id string) {
	t.Helper()
	if rec := postMetrics(t, h, `{"samples":[{"name":"hits","type":"counter","labels":{},"value":10}]}`, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("seed metrics = %d", rec.Code)
	}
	body := `{"kind":"threshold","operator":"gt","threshold":1,"metric":"hits","labels":{}}`
	if rec := putRule(t, h, id, body); rec.Code != http.StatusCreated {
		t.Fatalf("create rule %s = %d", id, rec.Code)
	}
	// A second rule that stays inactive for the negative case.
	body2 := `{"kind":"threshold","operator":"gt","threshold":100,"metric":"hits","labels":{}}`
	if rec := putRule(t, h, id+"_quiet", body2); rec.Code != http.StatusCreated {
		t.Fatalf("create quiet rule = %d", rec.Code)
	}
}

func TestAlertSilencing(t *testing.T) {
	h := Handler()
	seedFiringRule(t, h, "r1")
	now := time.Now()

	// Active silence covering r1: firing alert is marked silenced.
	putSilence(t, h, "s_active", silenceBody(now.Add(-time.Hour), now.Add(time.Hour), []string{"r1"}, "maint"))
	alerts := alertsByID(t, getAlerts(t, h))
	a := alerts["r1"].(map[string]any)
	if a["silenced"] != true {
		t.Fatalf("firing alert not silenced: %v", a)
	}
	if ids := a["silence_ids"].([]any); len(ids) != 1 || ids[0] != "s_active" {
		t.Fatalf("silence_ids = %v", a["silence_ids"])
	}
	// Inactive alerts are never silenced.
	q := alerts["r1_quiet"].(map[string]any)
	if q["silenced"] != false || len(q["silence_ids"].([]any)) != 0 {
		t.Fatalf("inactive alert silenced: %v", q)
	}

	// Upcoming and expired silences do not apply.
	putSilence(t, h, "s_future", silenceBody(now.Add(time.Hour), now.Add(2*time.Hour), []string{"r1"}, "soon"))
	putSilence(t, h, "s_past", silenceBody(now.Add(-2*time.Hour), now.Add(-time.Hour), []string{"r1"}, "done"))
	// A silence covering a different rule does not apply.
	putSilence(t, h, "s_other", silenceBody(now.Add(-time.Hour), now.Add(time.Hour), []string{"r9"}, "elsewhere"))
	a = alertsByID(t, getAlerts(t, h))["r1"].(map[string]any)
	if ids := a["silence_ids"].([]any); len(ids) != 1 || ids[0] != "s_active" {
		t.Fatalf("non-active or unrelated silences applied: %v", ids)
	}

	// Multiple active hits are collected and sorted lexicographically.
	putSilence(t, h, "s_b", silenceBody(now.Add(-time.Minute), now.Add(time.Minute), []string{"r1", "r1_quiet"}, "b"))
	putSilence(t, h, "s_a", silenceBody(now.Add(-time.Minute), now.Add(time.Minute), []string{"r1"}, "a"))
	a = alertsByID(t, getAlerts(t, h))["r1"].(map[string]any)
	if a["silenced"] != true {
		t.Fatalf("firing alert lost silenced flag: %v", a)
	}
	got := a["silence_ids"].([]any)
	want := []string{"s_a", "s_active", "s_b"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("silence_ids = %v, want %v", got, want)
	}
	// r1_quiet is inactive despite being listed in s_b.
	q = alertsByID(t, getAlerts(t, h))["r1_quiet"].(map[string]any)
	if q["silenced"] != false || len(q["silence_ids"].([]any)) != 0 {
		t.Fatalf("inactive rule silenced via s_b: %v", q)
	}

	// Deleting the active silences removes the marks; expired ones remain listed.
	silenceRequest(t, h, http.MethodDelete, "s_active", "", "")
	silenceRequest(t, h, http.MethodDelete, "s_a", "", "")
	silenceRequest(t, h, http.MethodDelete, "s_b", "", "")
	a = alertsByID(t, getAlerts(t, h))["r1"].(map[string]any)
	if a["silenced"] != false || len(a["silence_ids"].([]any)) != 0 {
		t.Fatalf("alert still silenced after delete: %v", a)
	}
	all := decodeBody(t, listSilences(t, h))["silences"].([]any)
	if len(all) != 3 {
		t.Fatalf("expired/upcoming silences should be retained, got %v", all)
	}
}

func TestSilencingBoundaryWindow(t *testing.T) {
	h := Handler()
	seedFiringRule(t, h, "edge")
	now := time.Now()

	// Window starting now is active: starts_at <= now < ends_at.
	putSilence(t, h, "starts_now", silenceBody(now.Add(-time.Second), now.Add(time.Hour), []string{"edge"}, "go"))
	a := alertsByID(t, getAlerts(t, h))["edge"].(map[string]any)
	if a["silenced"] != true {
		t.Fatalf("window starting around now not active: %v", a)
	}

	// Window ending now (or a second ago) is expired and must not silence.
	putSilence(t, h, "ends_now", silenceBody(now.Add(-2*time.Hour), now.Add(-time.Second), []string{"edge"}, "end"))
	a = alertsByID(t, getAlerts(t, h))["edge"].(map[string]any)
	if ids := a["silence_ids"].([]any); len(ids) != 1 || ids[0] != "starts_now" {
		t.Fatalf("ended window applied at boundary: %v", ids)
	}
}
