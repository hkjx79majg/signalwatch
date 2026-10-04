package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// expositionServer starts a test server serving the given body with status
// 200, or the given status code with an empty body when body is empty.
func expositionServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func reloadOne(t *testing.T, h http.Handler, tenant, id, url, labels string, enabled bool) {
	t.Helper()
	body := fmt.Sprintf(`{"targets":[{"id":%q,"url":%q,"labels":%s,"enabled":%t}]}`, id, url, labels, enabled)
	if rec := reloadTargets(t, h, tenant, body); rec.Code != http.StatusOK {
		t.Fatalf("seed reload = %d %q", rec.Code, rec.Body.String())
	}
}

func scrapeRequest(t *testing.T, h http.Handler, tenant, body string) *httptest.ResponseRecorder {
	t.Helper()
	return discoveryRequest(t, h, http.MethodPost, discoveryTargetsScrapePath, body, "application/json", tenant)
}

func scrapeResults(t *testing.T, rec *httptest.ResponseRecorder) (float64, []any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, body=%q", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)
	gen, ok := payload["generation"].(float64)
	if !ok {
		t.Fatalf("generation missing: %v", payload)
	}
	results, ok := payload["results"].([]any)
	if !ok {
		t.Fatalf("results missing: %v", payload)
	}
	return gen, results
}

func scrapedValue(t *testing.T, h http.Handler, tenant, name string, selector string) (float64, map[string]any) {
	t.Helper()
	target := "/api/v1/metrics?name=" + name + selector
	rec := discoveryRequest(t, h, http.MethodGet, target, "", "", tenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics get = %d %q", rec.Code, rec.Body.String())
	}
	series := decodeBody(t, rec)["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("series for %s = %v", name, series)
	}
	item := series[0].(map[string]any)
	v, _ := item["value"].(float64)
	return v, item
}

func TestDiscoveryScrapeGaugeAndCounter(t *testing.T) {
	h := Handler()
	srv := expositionServer(t, http.StatusOK, `# TYPE temp gauge
# TYPE hits counter
temp{room="a"} 21.5
hits 100
`)
	reloadOne(t, h, "", "web", srv.URL, `{"job":"web"}`, true)

	gen, results := scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":["web"]}`))
	if gen != 1 {
		t.Fatalf("generation = %v", gen)
	}
	if len(results) != 1 {
		t.Fatalf("results = %v", results)
	}
	r0 := results[0].(map[string]any)
	if r0["id"] != "web" || r0["status"] != "ok" || r0["accepted"] != float64(2) {
		t.Fatalf("result = %v", r0)
	}
	if _, hasCode := r0["code"]; hasCode {
		t.Fatalf("success result carries code: %v", r0)
	}

	// Gauge overwrites; labels merged with static labels plus target_id.
	v, item := scrapedValue(t, h, "", "temp", "")
	if v != 21.5 {
		t.Fatalf("temp = %v", v)
	}
	labels := item["labels"].(map[string]any)
	if labels["job"] != "web" || labels["room"] != "a" || labels["target_id"] != "web" {
		t.Fatalf("labels = %v", labels)
	}
	if item["type"] != "gauge" {
		t.Fatalf("temp type = %v", item["type"])
	}

	// First counter scrape commits the full cumulative value.
	if v, _ := scrapedValue(t, h, "", "hits", ""); v != 100 {
		t.Fatalf("hits = %v", v)
	}
}

func TestDiscoveryScrapeCounterDeltaAndReset(t *testing.T) {
	h := Handler()
	body := `# TYPE hits counter
hits 100
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	reloadOne(t, h, "", "t", srv.URL, `{}`, true)

	scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":["t"]}`))
	if v, _ := scrapedValue(t, h, "", "hits", ""); v != 100 {
		t.Fatalf("first hits = %v", v)
	}

	// Non-decreasing reading commits only the delta.
	body = "# TYPE hits counter\nhits 130\n"
	_, results := scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":["t"]}`))
	if results[0].(map[string]any)["accepted"] != float64(1) {
		t.Fatalf("results = %v", results)
	}
	if v, _ := scrapedValue(t, h, "", "hits", ""); v != 130 {
		t.Fatalf("hits after delta = %v", v)
	}

	// A decrease is a reset: the new reading commits in full.
	body = "# TYPE hits counter\nhits 20\n"
	scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":["t"]}`))
	if v, _ := scrapedValue(t, h, "", "hits", ""); v != 150 {
		t.Fatalf("hits after reset = %v", v)
	}
}

func TestDiscoveryScrapeAllEnabled(t *testing.T) {
	h := Handler()
	srvA := expositionServer(t, http.StatusOK, "# TYPE g gauge\ng 1\n")
	srvB := expositionServer(t, http.StatusOK, "# TYPE g gauge\ng 2\n")
	srvC := expositionServer(t, http.StatusOK, "# TYPE g gauge\ng 3\n")
	body := fmt.Sprintf(`{"targets":[
		{"id":"b","url":%q,"labels":{},"enabled":true},
		{"id":"a","url":%q,"labels":{},"enabled":true},
		{"id":"c","url":%q,"labels":{},"enabled":false}
	]}`, srvB.URL, srvA.URL, srvC.URL)
	if rec := reloadTargets(t, h, "", body); rec.Code != http.StatusOK {
		t.Fatalf("reload = %d %q", rec.Code, rec.Body.String())
	}

	// Empty array scrapes every enabled target; disabled ones are skipped.
	gen, results := scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":[]}`))
	if gen != 1 || len(results) != 2 {
		t.Fatalf("gen=%v results=%v", gen, results)
	}
	if results[0].(map[string]any)["id"] != "a" || results[1].(map[string]any)["id"] != "b" {
		t.Fatalf("results not sorted by id: %v", results)
	}
	for _, r := range results {
		if r.(map[string]any)["status"] != "ok" {
			t.Fatalf("result = %v", r)
		}
	}
	if v, _ := scrapedValue(t, h, "", "g", "&label.target_id=a"); v != 1 {
		t.Fatalf("g{target_id=a} = %v", v)
	}
	if v, _ := scrapedValue(t, h, "", "g", "&label.target_id=b"); v != 2 {
		t.Fatalf("g{target_id=b} = %v", v)
	}
}

func TestDiscoveryScrapeNoEnabledTargets(t *testing.T) {
	h := Handler()
	reloadOne(t, h, "", "off", "http://127.0.0.1:1/m", `{}`, false)
	gen, results := scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":[]}`))
	if gen != 1 || len(results) != 0 {
		t.Fatalf("gen=%v results=%v", gen, results)
	}
}

func TestDiscoveryScrapeFailuresDoNotRollback(t *testing.T) {
	h := Handler()
	okSrv := expositionServer(t, http.StatusOK, "# TYPE g gauge\ng 7\n")
	errSrv := expositionServer(t, http.StatusInternalServerError, "")
	badSrv := expositionServer(t, http.StatusOK, "# TYPE g gauge\ng notanumber\n")
	body := fmt.Sprintf(`{"targets":[
		{"id":"a_ok","url":%q,"labels":{},"enabled":true},
		{"id":"b_http","url":%q,"labels":{},"enabled":true},
		{"id":"c_down","url":"http://127.0.0.1:1/m","labels":{},"enabled":true},
		{"id":"d_bad","url":%q,"labels":{},"enabled":true}
	]}`, okSrv.URL, errSrv.URL, badSrv.URL)
	if rec := reloadTargets(t, h, "", body); rec.Code != http.StatusOK {
		t.Fatalf("reload = %d %q", rec.Code, rec.Body.String())
	}

	_, results := scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":[]}`))
	if len(results) != 4 {
		t.Fatalf("results = %v", results)
	}
	want := []struct {
		id     string
		status string
		code   string
	}{
		{"a_ok", "ok", ""},
		{"b_http", "error", "target_http_error"},
		{"c_down", "error", "target_unreachable"},
		{"d_bad", "error", "invalid_exposition"},
	}
	for i, w := range want {
		r := results[i].(map[string]any)
		if r["id"] != w.id || r["status"] != w.status {
			t.Fatalf("results[%d] = %v", i, r)
		}
		if w.code == "" {
			if r["accepted"] != float64(1) {
				t.Fatalf("results[%d] accepted = %v", i, r["accepted"])
			}
			continue
		}
		if r["code"] != w.code || r["accepted"] != float64(0) {
			t.Fatalf("results[%d] = %v", i, r)
		}
	}

	// The successful target committed despite the others failing.
	if v, _ := scrapedValue(t, h, "", "g", ""); v != 7 {
		t.Fatalf("g = %v", v)
	}
}

func TestDiscoveryScrapeExpositionValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"unknown type", "# TYPE m summary\nm 1\n"},
		{"no type declaration", "m 1\n"},
		{"duplicate label", "# TYPE m gauge\nm{a=\"1\",a=\"2\"} 1\n"},
		{"bad metric name", "# TYPE m gauge\n1m 1\n"},
		{"bad label name", "# TYPE m gauge\nm{1a=\"v\"} 1\n"},
		{"nan value", "# TYPE m gauge\nm NaN\n"},
		{"inf value", "# TYPE m counter\nm +Inf\n"},
		{"missing value", "# TYPE m gauge\nm\n"},
		{"trailing token", "# TYPE m gauge\nm 1 12345\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Handler()
			srv := expositionServer(t, http.StatusOK, tc.body)
			reloadOne(t, h, "", "t", srv.URL, `{}`, true)
			_, results := scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":["t"]}`))
			r := results[0].(map[string]any)
			if r["status"] != "error" || r["code"] != "invalid_exposition" || r["accepted"] != float64(0) {
				t.Fatalf("result = %v", r)
			}
			// Nothing was written.
			rec := discoveryRequest(t, h, http.MethodGet, "/api/v1/metrics?name=m", "", "", "")
			if series := decodeBody(t, rec)["series"].([]any); len(series) != 0 {
				t.Fatalf("series after invalid exposition = %v", series)
			}
		})
	}
}

func TestDiscoveryScrapeLabelConflict(t *testing.T) {
	h := Handler()
	// Sample label clashes with a different static value.
	srvA := expositionServer(t, http.StatusOK, "# TYPE g gauge\ng{job=\"other\"} 1\n")
	// Sample uses the reserved target_id label.
	srvB := expositionServer(t, http.StatusOK, "# TYPE g gauge\ng{target_id=\"x\"} 1\n")
	// Static labels use the reserved target_id label.
	srvC := expositionServer(t, http.StatusOK, "# TYPE g gauge\ng 1\n")
	// Same name and value in both sources merges fine.
	srvD := expositionServer(t, http.StatusOK, "# TYPE g gauge\ng{job=\"web\"} 5\n")

	body := fmt.Sprintf(`{"targets":[
		{"id":"a","url":%q,"labels":{"job":"web"},"enabled":true},
		{"id":"b","url":%q,"labels":{},"enabled":true},
		{"id":"c","url":%q,"labels":{"target_id":"x"},"enabled":true},
		{"id":"d","url":%q,"labels":{"job":"web"},"enabled":true}
	]}`, srvA.URL, srvB.URL, srvC.URL, srvD.URL)
	if rec := reloadTargets(t, h, "", body); rec.Code != http.StatusOK {
		t.Fatalf("reload = %d %q", rec.Code, rec.Body.String())
	}

	_, results := scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":[]}`))
	for i, id := range []string{"a", "b", "c"} {
		r := results[i].(map[string]any)
		if r["id"] != id || r["status"] != "error" || r["code"] != "label_conflict" {
			t.Fatalf("results[%d] = %v", i, r)
		}
	}
	if r := results[3].(map[string]any); r["id"] != "d" || r["status"] != "ok" {
		t.Fatalf("results[3] = %v", r)
	}
	if v, _ := scrapedValue(t, h, "", "g", ""); v != 5 {
		t.Fatalf("g = %v", v)
	}
}

func TestDiscoveryScrapeMetricConflict(t *testing.T) {
	h := Handler()
	// Seed a gauge series that the scrape will collide with as a counter.
	rec := discoveryRequest(t, h, http.MethodPost, "/api/v1/metrics",
		`{"samples":[{"name":"m","type":"gauge","labels":{"target_id":"t"},"value":1}]}`,
		"application/json", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("seed metrics = %d %q", rec.Code, rec.Body.String())
	}

	srv := expositionServer(t, http.StatusOK, "# TYPE m counter\nm 10\n")
	reloadOne(t, h, "", "t", srv.URL, `{}`, true)

	_, results := scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":["t"]}`))
	r := results[0].(map[string]any)
	if r["status"] != "error" || r["code"] != "metric_conflict" || r["accepted"] != float64(0) {
		t.Fatalf("result = %v", r)
	}
	// Existing series unchanged.
	v, item := scrapedValue(t, h, "", "m", "")
	if item["type"] != "gauge" || v != 1 {
		t.Fatalf("series after conflict = %v", item)
	}

	// Baseline was not updated either: a later compatible scrape still
	// treats the reading as the first one.
	srv2 := expositionServer(t, http.StatusOK, "# TYPE m gauge\nm 10\n")
	reloadOne(t, h, "", "t", srv2.URL, `{}`, true)
	_, results = scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":["t"]}`))
	if r := results[0].(map[string]any); r["status"] != "ok" {
		t.Fatalf("rescrape = %v", r)
	}
	if v, _ := scrapedValue(t, h, "", "m", ""); v != 10 {
		t.Fatalf("m after rescrape = %v", v)
	}
}

func TestDiscoveryScrapeRequestValidation(t *testing.T) {
	h := Handler()
	if rec := reloadTargets(t, h, "", `{"targets":[
		{"id":"a","url":"http://127.0.0.1:1/m","labels":{},"enabled":true},
		{"id":"b","url":"http://127.0.0.1:1/m","labels":{},"enabled":false}
	]}`); rec.Code != http.StatusOK {
		t.Fatalf("reload = %d %q", rec.Code, rec.Body.String())
	}

	badBodies := []string{
		`[]`,
		`{}`,
		`{"target_ids":null}`,
		`{"target_ids":[],"extra":1}`,
		`{"target_ids":["a"],"x":[]}`,
		`{"target_ids":"a"}`,
		`{"target_ids":[1]}`,
		`{"target_ids":["1bad"]}`,
		`{"target_ids":["a","a"]}`,
		`{"target_ids":["a"],} `,
		`{"target_ids":["a"]} {}`,
	}
	for _, body := range badBodies {
		expectErrorCode(t, scrapeRequest(t, h, "", body), http.StatusBadRequest, "invalid_discovery_scrape")
	}

	// Unknown id wins over nothing else; disabled is a conflict.
	expectErrorCode(t, scrapeRequest(t, h, "", `{"target_ids":["nope"]}`), http.StatusNotFound, "discovery_target_not_found")
	expectErrorCode(t, scrapeRequest(t, h, "", `{"target_ids":["a","nope"]}`), http.StatusNotFound, "discovery_target_not_found")
	expectErrorCode(t, scrapeRequest(t, h, "", `{"target_ids":["b"]}`), http.StatusConflict, "discovery_target_disabled")
	expectErrorCode(t, scrapeRequest(t, h, "", `{"target_ids":["a","b"]}`), http.StatusConflict, "discovery_target_disabled")
	// Not-found is reported before disabled.
	expectErrorCode(t, scrapeRequest(t, h, "", `{"target_ids":["b","nope"]}`), http.StatusNotFound, "discovery_target_not_found")
}

func TestDiscoveryScrapeMethodsMediaTypeAndTenant(t *testing.T) {
	h := Handler()

	// POST-only with Allow: POST.
	rec := discoveryRequest(t, h, http.MethodGet, discoveryTargetsScrapePath, "", "", "")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("Allow = %q, want POST", allow)
	}
	rec = discoveryRequest(t, h, http.MethodDelete, discoveryTargetsScrapePath, "", "", "")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")

	// JSON only.
	rec = discoveryRequest(t, h, http.MethodPost, discoveryTargetsScrapePath, `{"target_ids":[]}`, "text/plain", "")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")

	// invalid_tenant beats every other check.
	rec = discoveryRequest(t, h, http.MethodPost, discoveryTargetsScrapePath, "not json", "text/plain", "bad tenant!")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
	rec = discoveryRequest(t, h, http.MethodGet, discoveryTargetsScrapePath, "", "", "bad tenant!")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
}

func TestDiscoveryScrapeTenancy(t *testing.T) {
	h := Handler()
	srv := expositionServer(t, http.StatusOK, "# TYPE g gauge\ng 9\n")
	reloadOne(t, h, "alpha", "t", srv.URL, `{}`, true)

	// beta has no targets: empty results, generation 0.
	gen, results := scrapeResults(t, scrapeRequest(t, h, "beta", `{"target_ids":[]}`))
	if gen != 0 || len(results) != 0 {
		t.Fatalf("beta scrape = gen %v results %v", gen, results)
	}
	// beta cannot address alpha's target.
	expectErrorCode(t, scrapeRequest(t, h, "beta", `{"target_ids":["t"]}`), http.StatusNotFound, "discovery_target_not_found")

	// alpha scrapes and the metric lands only in alpha.
	gen, results = scrapeResults(t, scrapeRequest(t, h, "alpha", `{"target_ids":["t"]}`))
	if gen != 1 || len(results) != 1 || results[0].(map[string]any)["status"] != "ok" {
		t.Fatalf("alpha scrape = gen %v results %v", gen, results)
	}
	if v, _ := scrapedValue(t, h, "alpha", "g", ""); v != 9 {
		t.Fatalf("alpha g = %v", v)
	}
	rec := discoveryRequest(t, h, http.MethodGet, "/api/v1/metrics?name=g", "", "", "beta")
	if series := decodeBody(t, rec)["series"].([]any); len(series) != 0 {
		t.Fatalf("beta sees alpha metrics: %v", series)
	}
}

func TestDiscoveryScrapeSnapshotPinned(t *testing.T) {
	h := Handler()
	srv := expositionServer(t, http.StatusOK, "# TYPE g gauge\ng 1\n")
	reloadOne(t, h, "", "t", srv.URL, `{}`, true)

	// A reload between scrapes does not rewrite history: the generation
	// returned is the one pinned when each scrape started.
	gen, _ := scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":["t"]}`))
	if gen != 1 {
		t.Fatalf("gen = %v", gen)
	}
	reloadOne(t, h, "", "t", srv.URL, `{"job":"x"}`, true)
	gen, results := scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":["t"]}`))
	if gen != 2 || len(results) != 1 {
		t.Fatalf("gen=%v results=%v", gen, results)
	}
	// The new static labels apply to the new scrape (a new label set is a
	// new series, so select it explicitly).
	_, item := scrapedValue(t, h, "", "g", "&label.job=x")
	if item["labels"].(map[string]any)["job"] != "x" {
		t.Fatalf("labels = %v", item["labels"])
	}
}

func TestDiscoveryScrapeRedirectNotFollowed(t *testing.T) {
	h := Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/real" {
			_, _ = fmt.Fprint(w, "# TYPE g gauge\ng 1\n")
			return
		}
		http.Redirect(w, r, "/real", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	reloadOne(t, h, "", "t", srv.URL, `{}`, true)

	_, results := scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":["t"]}`))
	r := results[0].(map[string]any)
	if r["status"] != "error" || r["code"] != "target_http_error" {
		t.Fatalf("result = %v", r)
	}
}

func TestDiscoveryScrapeTypeFromLatestDeclaration(t *testing.T) {
	h := Handler()
	srv := expositionServer(t, http.StatusOK, strings.Join([]string{
		"# TYPE m gauge",
		"# TYPE m counter",
		"m 5",
		"# TYPE n counter",
		"# TYPE n gauge",
		"n 2",
	}, "\n")+"\n")
	reloadOne(t, h, "", "t", srv.URL, `{}`, true)

	_, results := scrapeResults(t, scrapeRequest(t, h, "", `{"target_ids":["t"]}`))
	if r := results[0].(map[string]any); r["status"] != "ok" || r["accepted"] != float64(2) {
		t.Fatalf("result = %v", r)
	}
	_, itemM := scrapedValue(t, h, "", "m", "")
	if itemM["type"] != "counter" {
		t.Fatalf("m type = %v", itemM["type"])
	}
	_, itemN := scrapedValue(t, h, "", "n", "")
	if itemN["type"] != "gauge" {
		t.Fatalf("n type = %v", itemN["type"])
	}
}
