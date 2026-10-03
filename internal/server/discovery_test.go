package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func reloadDiscovery(t *testing.T, h http.Handler, body string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return tenantJSON(t, h, http.MethodPost, discoveryReloadPath, body, tenants...)
}

func getDiscovery(t *testing.T, h http.Handler, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodGet, discoveryTargetsPath, "", "", tenants...)
}

func discoverySnapshotOf(t *testing.T, rec *httptest.ResponseRecorder) (int64, []any) {
	t.Helper()
	payload := decodeBody(t, rec)
	gen, ok := payload["generation"].(float64)
	if !ok {
		t.Fatalf("generation missing or not a number: %v", payload)
	}
	targets, ok := payload["targets"].([]any)
	if !ok {
		t.Fatalf("targets missing or not an array: %v", payload)
	}
	return int64(gen), targets
}

func TestDiscoveryFreshTenantIsEmptySnapshot(t *testing.T) {
	h := Handler()

	rec := getDiscovery(t, h, "fresh-tenant")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	gen, targets := discoverySnapshotOf(t, rec)
	if gen != 0 || len(targets) != 0 {
		t.Fatalf("fresh snapshot = generation %d targets %v, want 0 and empty", gen, targets)
	}
	if strings.Contains(rec.Body.String(), `"targets":null`) {
		t.Fatalf("empty targets must encode as [], body=%q", rec.Body.String())
	}
}

func TestDiscoveryReloadAndQueryRoundTrip(t *testing.T) {
	h := Handler()

	// Submitted out of id order; the response must sort by id.
	body := `{"targets":[
		{"id":"b_tgt","url":"https://b.example.com:8443/scrape","labels":{"job":"b"},"enabled":false},
		{"id":"a_tgt","url":"http://a.example.com/metrics","labels":{"job":"a","zone":"z1"},"enabled":true}
	]}`
	rec := reloadDiscovery(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("reload status = %d body=%q", rec.Code, rec.Body.String())
	}
	gen, targets := discoverySnapshotOf(t, rec)
	if gen != 1 {
		t.Fatalf("first reload generation = %d, want 1", gen)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %v", targets)
	}
	first, _ := targets[0].(map[string]any)
	second, _ := targets[1].(map[string]any)
	if first["id"] != "a_tgt" || second["id"] != "b_tgt" {
		t.Fatalf("targets not sorted by id: %v", targets)
	}
	if first["url"] != "http://a.example.com/metrics" || first["enabled"] != true {
		t.Fatalf("first target = %v", first)
	}
	labels, _ := first["labels"].(map[string]any)
	if labels["job"] != "a" || labels["zone"] != "z1" {
		t.Fatalf("first labels = %v", labels)
	}
	if second["enabled"] != false {
		t.Fatalf("second target = %v", second)
	}

	// The query endpoint returns the same snapshot.
	rec = getDiscovery(t, h)
	gen, targets = discoverySnapshotOf(t, rec)
	if gen != 1 || len(targets) != 2 {
		t.Fatalf("query snapshot = generation %d targets %v", gen, targets)
	}
}

func TestDiscoveryGenerationOnlyIncrementsOnEffectiveChange(t *testing.T) {
	h := Handler()

	reload := func(body string) int64 {
		rec := reloadDiscovery(t, h, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("reload status = %d body=%q", rec.Code, rec.Body.String())
		}
		gen, _ := discoverySnapshotOf(t, rec)
		return gen
	}

	if gen := reload(`{"targets":[{"id":"a","url":"http://a.example.com","labels":{"k":"v","z":"q"},"enabled":true}]}`); gen != 1 {
		t.Fatalf("gen = %d, want 1", gen)
	}
	// Reordered targets and reordered label keys are not a change.
	if gen := reload(`{"targets":[{"id":"a","url":"http://a.example.com","labels":{"z":"q","k":"v"},"enabled":true}]}`); gen != 1 {
		t.Fatalf("reordered labels gen = %d, want 1", gen)
	}
	if gen := reload(`{"targets":[
		{"id":"b","url":"http://b.example.com","labels":{},"enabled":false},
		{"id":"a","url":"http://a.example.com","labels":{"k":"v","z":"q"},"enabled":true}
	]}`); gen != 2 {
		t.Fatalf("added target gen = %d, want 2", gen)
	}
	// Same set, different submission order: not a change.
	if gen := reload(`{"targets":[
		{"id":"a","url":"http://a.example.com","labels":{"z":"q","k":"v"},"enabled":true},
		{"id":"b","url":"http://b.example.com","labels":{},"enabled":false}
	]}`); gen != 2 {
		t.Fatalf("reordered targets gen = %d, want 2", gen)
	}
	// Each effective change bumps the generation exactly once.
	if gen := reload(`{"targets":[
		{"id":"a","url":"http://a.example.com","labels":{"k":"v","z":"q"},"enabled":false},
		{"id":"b","url":"http://b.example.com","labels":{},"enabled":false}
	]}`); gen != 3 {
		t.Fatalf("enabled flip gen = %d, want 3", gen)
	}
	if gen := reload(`{"targets":[
		{"id":"a","url":"http://a.example.com","labels":{"k":"v","z":"q"},"enabled":false},
		{"id":"b","url":"http://b.example.com","labels":{"k":"w"},"enabled":false}
	]}`); gen != 4 {
		t.Fatalf("label change gen = %d, want 4", gen)
	}
	if gen := reload(`{"targets":[
		{"id":"a","url":"https://a.example.com","labels":{"k":"v","z":"q"},"enabled":false},
		{"id":"b","url":"http://b.example.com","labels":{"k":"w"},"enabled":false}
	]}`); gen != 5 {
		t.Fatalf("url change gen = %d, want 5", gen)
	}
	// An empty array clears the configuration and counts as a change.
	rec := reloadDiscovery(t, h, `{"targets":[]}`)
	gen, targets := discoverySnapshotOf(t, rec)
	if gen != 6 || len(targets) != 0 {
		t.Fatalf("clear gen = %d targets = %v, want 6 and empty", gen, targets)
	}
	// Clearing an already-empty configuration is not a change.
	rec = reloadDiscovery(t, h, `{"targets":[]}`)
	gen, _ = discoverySnapshotOf(t, rec)
	if gen != 6 {
		t.Fatalf("re-clear gen = %d, want 6", gen)
	}
}

func TestDiscoveryReloadValidationFailuresKeepOldSnapshot(t *testing.T) {
	h := Handler()

	good := `{"targets":[{"id":"keep","url":"http://keep.example.com","labels":{"job":"k"},"enabled":true}]}`
	if rec := reloadDiscovery(t, h, good); rec.Code != http.StatusOK {
		t.Fatalf("seed reload: %d %s", rec.Code, rec.Body.String())
	}

	invalid := []string{
		`{}`,                       // targets missing
		`{"targets":null}`,         // targets null
		`{"targets":{}}`,           // targets not an array
		`{"targets":[],"extra":1}`, // extra envelope field
		`not json`,
		`[{"id":"x","url":"http://x.example.com","labels":{},"enabled":true}]`, // body not an object
		`{"targets":[{"id":"x","url":"http://x.example.com","labels":{},"enabled":true}]} trailing`,
		`{"targets":[{"url":"http://x.example.com","labels":{},"enabled":true}]}`,                    // id missing
		`{"targets":[{"id":"x","labels":{},"enabled":true}]}`,                                        // url missing
		`{"targets":[{"id":"x","url":"http://x.example.com","enabled":true}]}`,                       // labels missing
		`{"targets":[{"id":"x","url":"http://x.example.com","labels":{}}]}`,                          // enabled missing
		`{"targets":[{"id":"x","url":"http://x.example.com","labels":{},"enabled":true,"extra":1}]}`, // extra target field
		`{"targets":[{"id":"1x","url":"http://x.example.com","labels":{},"enabled":true}]}`,          // bad id
		`{"targets":[{"id":"","url":"http://x.example.com","labels":{},"enabled":true}]}`,            // empty id
		`{"targets":[{"id":"x","url":"http://x.example.com","labels":null,"enabled":true}]}`,         // null labels
		`{"targets":[{"id":"x","url":"http://x.example.com","labels":{"1k":"v"},"enabled":true}]}`,   // bad label key
		`{"targets":[{"id":"x","url":"http://x.example.com","labels":{"k":1},"enabled":true}]}`,      // non-string label value
		`{"targets":[{"id":"x","url":"http://x.example.com","labels":{},"enabled":"yes"}]}`,          // non-bool enabled
		`{"targets":[{"id":"x","url":"ftp://x.example.com","labels":{},"enabled":true}]}`,            // bad scheme
		`{"targets":[{"id":"x","url":"//x.example.com/path","labels":{},"enabled":true}]}`,           // no scheme
		`{"targets":[{"id":"x","url":"http:///path","labels":{},"enabled":true}]}`,                   // empty host
		`{"targets":[{"id":"x","url":"http://user@x.example.com","labels":{},"enabled":true}]}`,      // userinfo
		`{"targets":[{"id":"x","url":"http://user:pw@x.example.com","labels":{},"enabled":true}]}`,   // userinfo+password
		`{"targets":[{"id":"x","url":"http://x.example.com/#frag","labels":{},"enabled":true}]}`,     // fragment
		`{"targets":[{"id":"x","url":"http://x.example.com:abc/","labels":{},"enabled":true}]}`,      // non-numeric port
		`{"targets":[{"id":"x","url":"http://x.example.com:99999/","labels":{},"enabled":true}]}`,    // port out of range
		`{"targets":[{"id":"x","url":"http://x.example.com","labels":{},"enabled":true},
		              {"id":"x","url":"http://y.example.com","labels":{},"enabled":false}]}`, // duplicate id
		`{"targets":["not-an-object"]}`, // target not an object
	}
	for _, body := range invalid {
		rec := reloadDiscovery(t, h, body)
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_discovery_targets")
	}

	// Over the limit: 1001 valid targets are rejected; exactly 1000 pass.
	var b strings.Builder
	b.WriteString(`{"targets":[`)
	for i := 0; i < maxDiscoveryTargets+1; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"t` + strconv.Itoa(i) + `","url":"http://t.example.com","labels":{},"enabled":true}`)
	}
	b.WriteString(`]}`)
	over := b.String()
	expectErrorCode(t, reloadDiscovery(t, h, over), http.StatusBadRequest, "invalid_discovery_targets")

	// The old snapshot and generation survived every rejection.
	rec := getDiscovery(t, h)
	gen, targets := discoverySnapshotOf(t, rec)
	if gen != 1 || len(targets) != 1 {
		t.Fatalf("snapshot changed after rejections: gen=%d targets=%v", gen, targets)
	}
	item, _ := targets[0].(map[string]any)
	if item["id"] != "keep" {
		t.Fatalf("snapshot target = %v, want keep", item)
	}

	// Exactly 1000 targets are accepted.
	b.Reset()
	b.WriteString(`{"targets":[`)
	for i := 0; i < maxDiscoveryTargets; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"t` + strconv.Itoa(i) + `","url":"http://t.example.com","labels":{},"enabled":true}`)
	}
	b.WriteString(`]}`)
	if rec := reloadDiscovery(t, h, b.String()); rec.Code != http.StatusOK {
		t.Fatalf("1000-target reload: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDiscoveryReloadValidURLVariants(t *testing.T) {
	h := Handler()

	body := `{"targets":[
		{"id":"a","url":"http://example.com","labels":{},"enabled":true},
		{"id":"b","url":"https://example.com:443/path?q=1","labels":{},"enabled":false},
		{"id":"c","url":"http://192.0.2.1:8080/metrics","labels":{},"enabled":true},
		{"id":"d","url":"http://[2001:db8::1]:9090/","labels":{},"enabled":true},
		{"id":"e","url":"HTTP://EXAMPLE.COM/Path","labels":{},"enabled":true}
	]}`
	if rec := reloadDiscovery(t, h, body); rec.Code != http.StatusOK {
		t.Fatalf("valid URL variants rejected: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDiscoveryReloadUnsupportedMediaType(t *testing.T) {
	h := Handler()

	body := `{"targets":[]}`
	for _, ct := range []string{"text/plain", "application/jsonx", ""} {
		rec := doRequest(t, h, http.MethodPost, discoveryReloadPath, body, ct)
		expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")
	}
	// A charset parameter is still application/json.
	rec := doRequest(t, h, http.MethodPost, discoveryReloadPath, body, "application/json; charset=utf-8")
	if rec.Code != http.StatusOK {
		t.Fatalf("charset media type: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDiscoveryQueryRejectsAnyQueryParameter(t *testing.T) {
	h := Handler()

	for _, target := range []string{
		discoveryTargetsPath + "?x=1",
		discoveryTargetsPath + "?generation=0",
		discoveryTargetsPath + "?%%%",
	} {
		rec := doRequest(t, h, http.MethodGet, target, "", "")
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_discovery_query")
	}
}

func TestDiscoveryMethodNotAllowed(t *testing.T) {
	h := Handler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := doRequest(t, h, method, discoveryTargetsPath, `{"targets":[]}`, "application/json")
		expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("query endpoint %s Allow = %q, want GET", method, allow)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := doRequest(t, h, method, discoveryReloadPath, "", "")
		expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("reload endpoint %s Allow = %q, want POST", method, allow)
		}
	}
}

func TestDiscoveryTargetsAreTenantIsolated(t *testing.T) {
	h := Handler()

	bodyA := `{"targets":[{"id":"a","url":"http://a.example.com","labels":{},"enabled":true}]}`
	bodyB := `{"targets":[{"id":"b","url":"http://b.example.com","labels":{},"enabled":false}]}`
	if rec := reloadDiscovery(t, h, bodyA, "team-a"); rec.Code != http.StatusOK {
		t.Fatalf("team-a reload: %d %s", rec.Code, rec.Body.String())
	}
	if rec := reloadDiscovery(t, h, bodyB, "team-b"); rec.Code != http.StatusOK {
		t.Fatalf("team-b reload: %d %s", rec.Code, rec.Body.String())
	}

	// Each tenant sees exactly its own snapshot; unseen tenants stay empty.
	check := func(tenant, wantID string, wantGen, wantLen int) {
		t.Helper()
		rec := getDiscovery(t, h, tenant)
		gen, targets := discoverySnapshotOf(t, rec)
		if gen != int64(wantGen) || len(targets) != wantLen {
			t.Fatalf("tenant %q: gen=%d targets=%v, want gen=%d len=%d", tenant, gen, targets, wantGen, wantLen)
		}
		if wantLen > 0 {
			item, _ := targets[0].(map[string]any)
			if item["id"] != wantID {
				t.Fatalf("tenant %q target = %v, want id %q", tenant, item, wantID)
			}
		}
	}
	check("team-a", "a", 1, 1)
	check("team-b", "b", 1, 1)
	check("default", "", 0, 0)
	check("team-c", "", 0, 0)

	// Replacing team-a's configuration must not touch team-b's.
	if rec := reloadDiscovery(t, h, `{"targets":[]}`, "team-a"); rec.Code != http.StatusOK {
		t.Fatalf("team-a clear: %d %s", rec.Code, rec.Body.String())
	}
	check("team-a", "", 2, 0)
	check("team-b", "b", 1, 1)
}

func TestDiscoveryInvalidTenantTakesPrecedence(t *testing.T) {
	h := Handler()

	// Invalid tenant beats media-type, body, method and query validation.
	rec := doRequest(t, h, http.MethodPost, discoveryReloadPath, `{not json`, "text/plain", "bad tenant")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
	rec = doRequest(t, h, http.MethodGet, discoveryTargetsPath+"?x=1", "", "", "bad tenant")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
	rec = doRequest(t, h, http.MethodDelete, discoveryTargetsPath, "", "", "bad tenant")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
	rec = doRequest(t, h, http.MethodGet, discoveryReloadPath, "", "", "bad tenant")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")

	// Nothing was mutated: default tenant is still the empty baseline.
	gen, targets := discoverySnapshotOf(t, getDiscovery(t, h))
	if gen != 0 || len(targets) != 0 {
		t.Fatalf("default snapshot mutated: gen=%d targets=%v", gen, targets)
	}
}

func TestDiscoveryUnknownSubpathsKeepBaseline404(t *testing.T) {
	h := Handler()

	for _, target := range []string{
		discoveryTargetsPath + "/",
		discoveryTargetsPath + "/anything",
		discoveryReloadPath + "/",
	} {
		rec := doRequest(t, h, http.MethodGet, target, "", "", "team-a")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want baseline 404", target, rec.Code)
		}
	}
}
