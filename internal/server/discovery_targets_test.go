package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func discoveryRequest(t *testing.T, h http.Handler, method, target, body, contentType, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if tenant != "" {
		req.Header.Set(tenantHeader, tenant)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func reloadTargets(t *testing.T, h http.Handler, tenant, body string) *httptest.ResponseRecorder {
	t.Helper()
	return discoveryRequest(t, h, http.MethodPost, discoveryTargetsReloadPath, body, "application/json", tenant)
}

func getTargets(t *testing.T, h http.Handler, tenant, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	target := discoveryTargetsPath
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	return discoveryRequest(t, h, http.MethodGet, target, "", "", tenant)
}

func TestDiscoveryTargetsLifecycle(t *testing.T) {
	h := Handler()

	// Fresh tenant: generation 0, empty (non-null) targets array.
	rec := getTargets(t, h, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("initial get status = %d, body=%q", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["generation"] != float64(0) {
		t.Fatalf("initial generation = %v", body["generation"])
	}
	targets, ok := body["targets"].([]any)
	if !ok || len(targets) != 0 {
		t.Fatalf("initial targets = %v", body["targets"])
	}

	// Reload two targets, submitted out of id order.
	rec = reloadTargets(t, h, "", `{"targets":[
		{"id":"b_t","url":"https://example.com:8443/scrape","labels":{"job":"b"},"enabled":false},
		{"id":"a_t","url":"http://127.0.0.1:9090/metrics","labels":{"job":"a","zone":"z1"},"enabled":true}
	]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reload status = %d, body=%q", rec.Code, rec.Body.String())
	}
	body = decodeBody(t, rec)
	if body["generation"] != float64(1) {
		t.Fatalf("generation after first reload = %v", body["generation"])
	}
	targets = body["targets"].([]any)
	if len(targets) != 2 {
		t.Fatalf("targets = %v", targets)
	}
	// Response is sorted by id.
	first := targets[0].(map[string]any)
	second := targets[1].(map[string]any)
	if first["id"] != "a_t" || second["id"] != "b_t" {
		t.Fatalf("targets not sorted by id: %v", targets)
	}
	if first["url"] != "http://127.0.0.1:9090/metrics" || first["enabled"] != true {
		t.Fatalf("first target = %v", first)
	}
	labels := first["labels"].(map[string]any)
	if labels["job"] != "a" || labels["zone"] != "z1" {
		t.Fatalf("first labels = %v", labels)
	}

	// GET reflects the same snapshot.
	rec = getTargets(t, h, "", "")
	body = decodeBody(t, rec)
	if body["generation"] != float64(1) || len(body["targets"].([]any)) != 2 {
		t.Fatalf("get after reload = %v", body)
	}

	// Reordering targets and label keys is not a change: same generation.
	rec = reloadTargets(t, h, "", `{"targets":[
		{"id":"a_t","url":"http://127.0.0.1:9090/metrics","labels":{"zone":"z1","job":"a"},"enabled":true},
		{"id":"b_t","url":"https://example.com:8443/scrape","labels":{"job":"b"},"enabled":false}
	]}`)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["generation"] != float64(1) {
		t.Fatalf("reordered reload = %d %q", rec.Code, rec.Body.String())
	}

	// A content change bumps the generation.
	rec = reloadTargets(t, h, "", `{"targets":[
		{"id":"a_t","url":"http://127.0.0.1:9090/metrics","labels":{"zone":"z1","job":"a"},"enabled":true},
		{"id":"b_t","url":"https://example.com:8443/scrape","labels":{"job":"b"},"enabled":true}
	]}`)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["generation"] != float64(2) {
		t.Fatalf("changed reload = %d %q", rec.Code, rec.Body.String())
	}

	// Empty array clears the configuration and bumps the generation.
	rec = reloadTargets(t, h, "", `{"targets":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear reload = %d %q", rec.Code, rec.Body.String())
	}
	body = decodeBody(t, rec)
	if body["generation"] != float64(3) || len(body["targets"].([]any)) != 0 {
		t.Fatalf("cleared snapshot = %v", body)
	}
}

func TestDiscoveryTargetsReloadValidation(t *testing.T) {
	h := Handler()

	valid := `{"targets":[{"id":"ok","url":"http://example.com","labels":{},"enabled":true}]}`
	if rec := reloadTargets(t, h, "", valid); rec.Code != http.StatusOK {
		t.Fatalf("seed reload = %d %q", rec.Code, rec.Body.String())
	}

	cases := []struct {
		name string
		body string
	}{
		{"not an object", `[]`},
		{"trailing data", `{"targets":[]} {}`},
		{"missing targets", `{}`},
		{"null targets", `{"targets":null}`},
		{"extra envelope field", `{"targets":[],"extra":1}`},
		{"missing id", `{"targets":[{"url":"http://h","labels":{},"enabled":true}]}`},
		{"missing url", `{"targets":[{"id":"a","labels":{},"enabled":true}]}`},
		{"missing labels", `{"targets":[{"id":"a","url":"http://h","enabled":true}]}`},
		{"missing enabled", `{"targets":[{"id":"a","url":"http://h","labels":{}}]}`},
		{"extra target field", `{"targets":[{"id":"a","url":"http://h","labels":{},"enabled":true,"x":1}]}`},
		{"null labels", `{"targets":[{"id":"a","url":"http://h","labels":null,"enabled":true}]}`},
		{"non-string label value", `{"targets":[{"id":"a","url":"http://h","labels":{"k":1},"enabled":true}]}`},
		{"bad label key", `{"targets":[{"id":"a","url":"http://h","labels":{"1k":"v"},"enabled":true}]}`},
		{"bad id", `{"targets":[{"id":"1a","url":"http://h","labels":{},"enabled":true}]}`},
		{"duplicate id", `{"targets":[{"id":"a","url":"http://h","labels":{},"enabled":true},{"id":"a","url":"http://h2","labels":{},"enabled":false}]}`},
		{"non-boolean enabled", `{"targets":[{"id":"a","url":"http://h","labels":{},"enabled":"yes"}}]`},
		{"relative url", `{"targets":[{"id":"a","url":"/metrics","labels":{},"enabled":true}]}`},
		{"no host", `{"targets":[{"id":"a","url":"http:///metrics","labels":{},"enabled":true}]}`},
		{"empty host with port", `{"targets":[{"id":"a","url":"http://:8080/m","labels":{},"enabled":true}]}`},
		{"bad scheme", `{"targets":[{"id":"a","url":"ftp://h/m","labels":{},"enabled":true}]}`},
		{"userinfo", `{"targets":[{"id":"a","url":"http://user@h/m","labels":{},"enabled":true}]}`},
		{"fragment", `{"targets":[{"id":"a","url":"http://h/m#frag","labels":{},"enabled":true}]}`},
		{"empty fragment", `{"targets":[{"id":"a","url":"http://h/m#","labels":{},"enabled":true}]}`},
		{"non-numeric port", `{"targets":[{"id":"a","url":"http://h:abc/m","labels":{},"enabled":true}]}`},
		{"port out of range", `{"targets":[{"id":"a","url":"http://h:65536/m","labels":{},"enabled":true}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectErrorCode(t, reloadTargets(t, h, "", tc.body), http.StatusBadRequest, "invalid_discovery_targets")
		})
	}

	// Too many targets.
	var sb strings.Builder
	sb.WriteString(`{"targets":[`)
	for i := 0; i < maxDiscoveryTargets+1; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"id":"t_` + strconv.Itoa(i) + `","url":"http://h","labels":{},"enabled":true}`)
	}
	sb.WriteString(`]}`)
	expectErrorCode(t, reloadTargets(t, h, "", sb.String()), http.StatusBadRequest, "invalid_discovery_targets")

	// Exactly the limit is accepted.
	sb.Reset()
	sb.WriteString(`{"targets":[`)
	for i := 0; i < maxDiscoveryTargets; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"id":"t_` + strconv.Itoa(i) + `","url":"http://h","labels":{},"enabled":true}`)
	}
	sb.WriteString(`]}`)
	if rec := reloadTargets(t, h, "", sb.String()); rec.Code != http.StatusOK {
		t.Fatalf("limit reload = %d %q", rec.Code, rec.Body.String())
	}

	// Failed reloads never disturbed the seeded snapshot's content lineage:
	// the last accepted reload was the 1000-target one, so re-seeding the
	// original single target is a change from that, not from a stale state.
	rec := reloadTargets(t, h, "", valid)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-seed = %d %q", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	targets := body["targets"].([]any)
	if len(targets) != 1 || targets[0].(map[string]any)["id"] != "ok" {
		t.Fatalf("snapshot after failed reloads = %v", body)
	}
}

func TestDiscoveryTargetsAcceptedURLs(t *testing.T) {
	h := Handler()
	urls := []string{
		"http://example.com",
		"https://example.com/path?q=1",
		"http://127.0.0.1:8080/metrics",
		"https://[::1]:9090/m",
		"http://h:65535/",
		"HTTP://Example.COM/Path",
	}
	for _, u := range urls {
		body := `{"targets":[{"id":"t","url":"` + u + `","labels":{},"enabled":true}]}`
		if rec := reloadTargets(t, h, "", body); rec.Code != http.StatusOK {
			t.Fatalf("url %q rejected: %d %q", u, rec.Code, rec.Body.String())
		}
	}
}

func TestDiscoveryTargetsMethodsAndMediaType(t *testing.T) {
	h := Handler()

	// Query endpoint is GET-only.
	rec := discoveryRequest(t, h, http.MethodPost, discoveryTargetsPath, `{}`, "application/json", "")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("Allow = %q, want GET", allow)
	}

	// Reload endpoint is POST-only.
	rec = discoveryRequest(t, h, http.MethodGet, discoveryTargetsReloadPath, "", "", "")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("Allow = %q, want POST", allow)
	}

	// Wrong media type on reload.
	rec = discoveryRequest(t, h, http.MethodPost, discoveryTargetsReloadPath, `{"targets":[]}`, "text/plain", "")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")

	// Any query parameter on the query endpoint is rejected.
	expectErrorCode(t, getTargets(t, h, "", "foo=bar"), http.StatusBadRequest, "invalid_discovery_query")
	expectErrorCode(t, getTargets(t, h, "", "generation=1"), http.StatusBadRequest, "invalid_discovery_query")
}

func TestDiscoveryTargetsTenancy(t *testing.T) {
	h := Handler()

	// Invalid tenant wins over every other check.
	rec := discoveryRequest(t, h, http.MethodPost, discoveryTargetsReloadPath, "not json", "text/plain", "bad tenant!")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
	rec = discoveryRequest(t, h, http.MethodGet, discoveryTargetsPath, "", "", "bad tenant!")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")

	// Tenants are isolated: writes under one tenant are invisible to others.
	if rec := reloadTargets(t, h, "alpha", `{"targets":[{"id":"a","url":"http://h","labels":{},"enabled":true}]}`); rec.Code != http.StatusOK {
		t.Fatalf("alpha reload = %d", rec.Code)
	}
	body := decodeBody(t, getTargets(t, h, "beta", ""))
	if body["generation"] != float64(0) || len(body["targets"].([]any)) != 0 {
		t.Fatalf("beta sees alpha state: %v", body)
	}
	body = decodeBody(t, getTargets(t, h, "alpha", ""))
	if body["generation"] != float64(1) || len(body["targets"].([]any)) != 1 {
		t.Fatalf("alpha snapshot = %v", body)
	}

	// A reload under beta does not touch alpha.
	if rec := reloadTargets(t, h, "beta", `{"targets":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("beta reload = %d", rec.Code)
	}
	body = decodeBody(t, getTargets(t, h, "alpha", ""))
	if body["generation"] != float64(1) || len(body["targets"].([]any)) != 1 {
		t.Fatalf("alpha disturbed by beta: %v", body)
	}
}
