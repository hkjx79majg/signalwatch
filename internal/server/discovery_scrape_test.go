package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func scrapeRequest(t *testing.T, h http.Handler, body, contentType, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	if contentType == "" {
		contentType = "application/json"
	}
	return discoveryRequest(t, h, http.MethodPost, discoveryTargetsScrapePath, body, contentType, tenant)
}

// metricsTarget serves a fixed status/body. The returned URL uses 127.0.0.1,
// which the reload validator accepts.
func metricsTarget(t *testing.T, status int, body string) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("scrape method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, srv.URL
}

// blockingTarget blocks its handler until release is closed, allowing a
// scrape to be held open while the configuration is reloaded. entered is
// closed once the handler has received the request.
func blockingTarget(t *testing.T, body string) (*httptest.Server, string, chan struct{}, chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, srv.URL, release, entered
}

func unreachableURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + ln.Addr().String()
	_ = ln.Close()
	return addr
}

func reloadScrapeConfig(t *testing.T, h http.Handler, tenant string, targets ...map[string]any) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`{"targets":[`)
	for i, tg := range targets {
		if i > 0 {
			sb.WriteByte(',')
		}
		labels := tg["labels"].(map[string]string)
		labelParts := make([]string, 0, len(labels))
		for k, v := range labels {
			labelParts = append(labelParts, strconv.Quote(k)+":"+strconv.Quote(v))
		}
		sb.WriteString(`{"id":` + strconv.Quote(tg["id"].(string)) +
			`,"url":` + strconv.Quote(tg["url"].(string)) +
			`,"labels":{` + strings.Join(labelParts, ",") +
			`},"enabled":` + strconv.FormatBool(tg["enabled"].(bool)) + `}`)
	}
	sb.WriteString(`]}`)
	if rec := reloadTargets(t, h, tenant, sb.String()); rec.Code != http.StatusOK {
		t.Fatalf("reload = %d %q", rec.Code, rec.Body.String())
	}
}

func queryMetricValue(t *testing.T, h http.Handler, tenant, name string, selector map[string]string) (float64, bool) {
	t.Helper()
	target := metricsPath + "?name=" + name
	for k, v := range selector {
		target += "&label." + k + "=" + v
	}
	rec := discoveryRequest(t, h, http.MethodGet, target, "", "", tenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("metric query = %d %q", rec.Code, rec.Body.String())
	}
	series := decodeBody(t, rec)["series"].([]any)
	if len(series) == 0 {
		return 0, false
	}
	if len(series) != 1 {
		t.Fatalf("metric query matched %d series: %v", len(series), series)
	}
	return series[0].(map[string]any)["value"].(float64), true
}

func TestDiscoveryScrapeSuccess(t *testing.T) {
	h := Handler()
	_, urlA := metricsTarget(t, http.StatusOK, `# TYPE reqs counter
reqs 5
# TYPE temp gauge
temp{room="kitchen"} 21.5
`)
	_, urlB := metricsTarget(t, http.StatusOK, `# HELP reqs ignored
# TYPE reqs counter
reqs{code="200"} 7
`)
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "b", "url": urlB, "labels": map[string]string{"job": "b"}, "enabled": true},
		map[string]any{"id": "a", "url": urlA, "labels": map[string]string{"job": "web"}, "enabled": true},
	)

	// Explicitly out-of-order ids: results come back sorted by id.
	rec := scrapeRequest(t, h, `{"target_ids":["b","a"]}`, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape = %d %q", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["generation"] != float64(1) {
		t.Fatalf("generation = %v", body["generation"])
	}
	results := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v", results)
	}
	first := results[0].(map[string]any)
	second := results[1].(map[string]any)
	if first["id"] != "a" || second["id"] != "b" {
		t.Fatalf("results not sorted: %v", results)
	}
	for _, r := range results {
		item := r.(map[string]any)
		if item["status"] != "ok" {
			t.Fatalf("status = %v", item["status"])
		}
		if _, hasCode := item["code"]; hasCode {
			t.Fatalf("ok result carries code: %v", item)
		}
	}
	if first["accepted"] != float64(2) || second["accepted"] != float64(1) {
		t.Fatalf("accepted counts = %v %v", first["accepted"], second["accepted"])
	}

	if v, ok := queryMetricValue(t, h, "", "reqs", map[string]string{"job": "web", "target_id": "a"}); !ok || v != 5 {
		t.Fatalf("reqs a = %v ok=%v", v, ok)
	}
	if v, ok := queryMetricValue(t, h, "", "temp", map[string]string{"room": "kitchen", "target_id": "a"}); !ok || v != 21.5 {
		t.Fatalf("temp = %v ok=%v", v, ok)
	}
	if v, ok := queryMetricValue(t, h, "", "reqs", map[string]string{"job": "b", "code": "200", "target_id": "b"}); !ok || v != 7 {
		t.Fatalf("reqs b = %v ok=%v", v, ok)
	}
}

func TestDiscoveryScrapeAllEnabledAndEmpty(t *testing.T) {
	h := Handler()
	_, urlOn := metricsTarget(t, http.StatusOK, "# TYPE m gauge\nm 1\n")
	_, urlOff := metricsTarget(t, http.StatusOK, "# TYPE m gauge\nm 2\n")
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "on", "url": urlOn, "labels": map[string]string{}, "enabled": true},
		map[string]any{"id": "off", "url": urlOff, "labels": map[string]string{}, "enabled": false},
	)

	rec := scrapeRequest(t, h, `{"target_ids":[]}`, "", "")
	body := decodeBody(t, rec)
	results := body["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["id"] != "on" {
		t.Fatalf("empty selection results = %v", results)
	}
	if _, ok := queryMetricValue(t, h, "", "m", map[string]string{"target_id": "off"}); ok {
		t.Fatal("disabled target was scraped")
	}

	// No enabled targets at all: empty results.
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "off", "url": urlOff, "labels": map[string]string{}, "enabled": false},
	)
	rec = scrapeRequest(t, h, `{"target_ids":[]}`, "", "")
	if r := decodeBody(t, rec)["results"].([]any); len(r) != 0 {
		t.Fatalf("want empty results, got %v", r)
	}
}

func TestDiscoveryScrapeCounterSemantics(t *testing.T) {
	h := Handler()
	// The handler returns a value the test mutates between scrapes.
	current := 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("# TYPE hits counter\nhits " + strconv.Itoa(current) + "\n"))
	}))
	defer srv.Close()
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "t", "url": srv.URL, "labels": map[string]string{}, "enabled": true},
	)
	sel := map[string]string{"target_id": "t"}

	scrapeRequest(t, h, `{"target_ids":["t"]}`, "", "")
	if v, _ := queryMetricValue(t, h, "", "hits", sel); v != 10 {
		t.Fatalf("first write = %v, want full 10", v)
	}

	current = 14
	scrapeRequest(t, h, `{"target_ids":["t"]}`, "", "")
	if v, _ := queryMetricValue(t, h, "", "hits", sel); v != 14 {
		t.Fatalf("delta write = %v, want 14", v)
	}

	current = 3 // counter reset: write the new full value
	scrapeRequest(t, h, `{"target_ids":["t"]}`, "", "")
	if v, _ := queryMetricValue(t, h, "", "hits", sel); v != 17 {
		t.Fatalf("reset write = %v, want 17", v)
	}

	// Gauge always overwrites.
	_, gaugeURL := metricsTarget(t, http.StatusOK, "# TYPE g gauge\ng 4\n")
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "g", "url": gaugeURL, "labels": map[string]string{}, "enabled": true},
	)
	for i := 0; i < 2; i++ {
		scrapeRequest(t, h, `{"target_ids":["g"]}`, "", "")
	}
	if v, _ := queryMetricValue(t, h, "", "g", map[string]string{"target_id": "g"}); v != 4 {
		t.Fatalf("gauge value = %v, want 4", v)
	}
}

func TestDiscoveryScrapeBaselinesKeyedByGeneration(t *testing.T) {
	h := Handler()
	slow, slowURL, release, entered := blockingTarget(t, "# TYPE hits counter\nhits 10\n")
	_ = slow
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "t", "url": slowURL, "labels": map[string]string{}, "enabled": true},
	)

	// Hold a scrape against generation 1 open, then reload to generation 2
	// with a different URL.
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, discoveryTargetsScrapePath, strings.NewReader(`{"target_ids":["t"]}`))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		done <- rec
	}()
	<-entered

	_, url2 := metricsTarget(t, http.StatusOK, "# TYPE hits counter\nhits 12\n")
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "t", "url": url2, "labels": map[string]string{"zone": "z"}, "enabled": true},
	)
	close(release)

	rec := <-done
	if rec.Code != http.StatusOK {
		t.Fatalf("pinned scrape = %d %q", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["generation"] != float64(1) {
		t.Fatalf("pinned generation = %v, want 1", body["generation"])
	}
	// The pinned scrape used generation 1's static labels (no zone).
	if v, ok := queryMetricValue(t, h, "", "hits", map[string]string{"target_id": "t"}); !ok || v != 10 {
		t.Fatalf("pinned scrape value = %v ok=%v, want 10", v, ok)
	}

	// A new scrape is pinned to generation 2: fresh baseline, so the larger
	// reading 12 is written in full (not the delta 2 against generation 1).
	rec = scrapeRequest(t, h, `{"target_ids":["t"]}`, "", "")
	if g := decodeBody(t, rec)["generation"]; g != float64(2) {
		t.Fatalf("second scrape generation = %v, want 2", g)
	}
	if v, _ := queryMetricValue(t, h, "", "hits", map[string]string{"zone": "z", "target_id": "t"}); v != 12 {
		t.Fatalf("generation-2 full write = %v, want 12", v)
	}
}

func TestDiscoveryScrapeRequestValidation(t *testing.T) {
	h := Handler()
	_, url := metricsTarget(t, http.StatusOK, "# TYPE m gauge\nm 1\n")
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "on", "url": url, "labels": map[string]string{}, "enabled": true},
		map[string]any{"id": "off", "url": url, "labels": map[string]string{}, "enabled": false},
	)

	bad := []string{
		`not json`,
		`[]`,
		`{}`,
		`{"target_ids":null}`,
		`{"target_ids":["on"],"extra":1}`,
		`{"target_ids":["1bad"]}`,
		`{"target_ids":["on","on"]}`,
		`{"target_ids":[1]}`,
		`{"target_ids":["on"]} trailing`,
	}
	for _, b := range bad {
		if rec := scrapeRequest(t, h, b, "", ""); rec.Code != http.StatusBadRequest ||
			decodeBody(t, rec)["error"].(map[string]any)["code"] != "invalid_discovery_scrape" {
			t.Fatalf("body %q -> %d %q", b, rec.Code, rec.Body.String())
		}
	}

	// Any missing id (even alongside a disabled one) is a 404.
	expectErrorCode(t, scrapeRequest(t, h, `{"target_ids":["aaa_missing","off"]}`, "", ""),
		http.StatusNotFound, "discovery_target_not_found")
	expectErrorCode(t, scrapeRequest(t, h, `{"target_ids":["off","zzz_missing"]}`, "", ""),
		http.StatusNotFound, "discovery_target_not_found")
	// All ids exist but one is disabled: 409.
	expectErrorCode(t, scrapeRequest(t, h, `{"target_ids":["off","on"]}`, "", ""),
		http.StatusConflict, "discovery_target_disabled")
}

func TestDiscoveryScrapeTargetFailures(t *testing.T) {
	h := Handler()
	_, urlOK := metricsTarget(t, http.StatusOK, "# TYPE m gauge\nm 3\n")
	_, url404 := metricsTarget(t, http.StatusNotFound, "nope")
	_, url302 := metricsTarget(t, http.StatusFound, "")
	urlDead := unreachableURL(t)

	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "a", "url": urlOK, "labels": map[string]string{}, "enabled": true},
		map[string]any{"id": "b", "url": url404, "labels": map[string]string{}, "enabled": true},
		map[string]any{"id": "c", "url": urlDead, "labels": map[string]string{}, "enabled": true},
		map[string]any{"id": "d", "url": url302, "labels": map[string]string{}, "enabled": true},
	)
	rec := scrapeRequest(t, h, `{"target_ids":[]}`, "", "")
	results := decodeBody(t, rec)["results"].([]any)
	codes := map[string]map[string]any{}
	for _, r := range results {
		item := r.(map[string]any)
		codes[item["id"].(string)] = item
	}
	if codes["a"]["status"] != "ok" || codes["a"]["accepted"] != float64(1) {
		t.Fatalf("a = %v", codes["a"])
	}
	for _, tc := range []struct {
		id, code string
	}{
		{"b", "target_http_error"},
		{"c", "target_unreachable"},
		{"d", "target_http_error"},
	} {
		item := codes[tc.id]
		if item == nil || item["status"] != "error" || item["accepted"] != float64(0) || item["code"] != tc.code {
			t.Fatalf("%s = %v, want code %s", tc.id, item, tc.code)
		}
	}
	// Failed targets did not roll back the successful one.
	if v, ok := queryMetricValue(t, h, "", "m", map[string]string{"target_id": "a"}); !ok || v != 3 {
		t.Fatalf("successful target rolled back: %v %v", v, ok)
	}
}

func TestDiscoveryScrapeInvalidExposition(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"no type":              "m 1\n",
		"unknown type":         "# TYPE m histogram\nm 1\n",
		"untyped":              "# TYPE m untyped\nm 1\n",
		"duplicate label":      `# TYPE m gauge` + "\n" + `m{k="a",k="b"} 1` + "\n",
		"bad metric name":      "# TYPE 1m gauge\n1m 1\n",
		"bad label key":        "# TYPE m gauge\nm{1k=\"v\"} 1\n",
		"non-finite":           "# TYPE m gauge\nm NaN\n",
		"positive infinity":    "# TYPE m gauge\nm +Inf\n",
		"malformed labels":     "# TYPE m gauge\nm{k=\"a\" 1\n",
		"trailing timestamp":   "# TYPE m gauge\nm 1 1700000000000\n",
		"unterminated value":   "# TYPE m gauge\nm{k=\"a\n",
		"bad escape":           "# TYPE m gauge\n" + `m{k="a\q"} 1` + "\n",
		"equals without value": "# TYPE m gauge\nm=\n",
	}
	for name, exposition := range cases {
		t.Run(name, func(t *testing.T) {
			_, url := metricsTarget(t, http.StatusOK, exposition)
			reloadScrapeConfig(t, h, "",
				map[string]any{"id": "t", "url": url, "labels": map[string]string{}, "enabled": true},
			)
			expectErrorCode := func() {
				rec := scrapeRequest(t, h, `{"target_ids":["t"]}`, "", "")
				results := decodeBody(t, rec)["results"].([]any)
				item := results[0].(map[string]any)
				if item["code"] != "invalid_exposition" || item["accepted"] != float64(0) {
					t.Fatalf("result = %v", item)
				}
			}
			expectErrorCode()
			if _, ok := queryMetricValue(t, h, "", "m", map[string]string{"target_id": "t"}); ok {
				t.Fatal("invalid exposition wrote metrics")
			}
		})
	}

	// Valid edge forms that must be accepted. Each uses its own target id so
	// series from one case never type-clash with another.
	valid := []struct {
		name, id, exposition string
	}{
		{"brace then value", "v1", "# TYPE m gauge\nm{}1\n"},
		{"negative gauge", "v2", "# TYPE m gauge\nm -2.5\n"},
		{"escaped newline", "v3", "# TYPE m gauge\n" + `m{k="a\nb"} 1` + "\n"},
		{"cr line endings", "v4", "# TYPE m counter\r\nm 1\r\n"},
		{"blank and comments", "v5", "\n# just a comment\n# TYPE m gauge\nm\t1\t\n"},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			_, url := metricsTarget(t, http.StatusOK, tc.exposition)
			reloadScrapeConfig(t, h, "",
				map[string]any{"id": tc.id, "url": url, "labels": map[string]string{}, "enabled": true},
			)
			rec := scrapeRequest(t, h, `{"target_ids":["`+tc.id+`"]}`, "", "")
			results := decodeBody(t, rec)["results"].([]any)
			if r := results[0].(map[string]any); r["status"] != "ok" {
				t.Fatalf("valid exposition rejected: %v", r)
			}
		})
	}
}

func TestDiscoveryScrapeLabelConflicts(t *testing.T) {
	h := Handler()

	// Same label name, different values between static and sample.
	_, url1 := metricsTarget(t, http.StatusOK, `# TYPE m counter
m{job="exposed"} 5
`)
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "s1", "url": url1, "labels": map[string]string{"job": "static"}, "enabled": true},
	)
	rec := scrapeRequest(t, h, `{"target_ids":["s1"]}`, "", "")
	if code := decodeBody(t, rec)["results"].([]any)[0].(map[string]any)["code"]; code != "label_conflict" {
		t.Fatalf("static/sample clash code = %v", code)
	}

	// Reserved label used by the sample.
	_, url2 := metricsTarget(t, http.StatusOK, `# TYPE m gauge
m{target_id="spoof"} 5
`)
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "s2", "url": url2, "labels": map[string]string{}, "enabled": true},
	)
	rec = scrapeRequest(t, h, `{"target_ids":["s2"]}`, "", "")
	if code := decodeBody(t, rec)["results"].([]any)[0].(map[string]any)["code"]; code != "label_conflict" {
		t.Fatalf("sample reserved label code = %v", code)
	}

	// Same key/value in both sources is fine and merged once.
	_, url3 := metricsTarget(t, http.StatusOK, `# TYPE m gauge
m{job="same"} 5
`)
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "s3", "url": url3, "labels": map[string]string{"job": "same"}, "enabled": true},
	)
	rec = scrapeRequest(t, h, `{"target_ids":["s3"]}`, "", "")
	if r := decodeBody(t, rec)["results"].([]any)[0].(map[string]any); r["status"] != "ok" {
		t.Fatalf("equal-label merge failed: %v", r)
	}
}

func TestDiscoveryScrapeMetricConflictAtomic(t *testing.T) {
	h := Handler()

	// Pre-existing gauge with the exact identity target t's counter would use.
	postMetricsAsTenant(t, h, "", `{"samples":[{"name":"g","type":"gauge","labels":{"target_id":"t"},"value":9}]}`)

	_, url := metricsTarget(t, http.StatusOK, `# TYPE good counter
good 5
# TYPE g counter
g 1
`)
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "t", "url": url, "labels": map[string]string{}, "enabled": true},
	)
	rec := scrapeRequest(t, h, `{"target_ids":["t"]}`, "", "")
	if code := decodeBody(t, rec)["results"].([]any)[0].(map[string]any)["code"]; code != "metric_conflict" {
		t.Fatalf("code = %v", code)
	}
	// Nothing from the rejected batch landed, and the existing gauge kept
	// both its type-shape and value.
	if _, ok := queryMetricValue(t, h, "", "good", map[string]string{"target_id": "t"}); ok {
		t.Fatal("rejected batch wrote an earlier sample")
	}
	if v, _ := queryMetricValue(t, h, "", "g", map[string]string{"target_id": "t"}); v != 9 {
		t.Fatalf("existing gauge changed = %v, want 9", v)
	}

	// Baseline was not established: fixing the exposition writes full value.
	_, url2 := metricsTarget(t, http.StatusOK, "# TYPE good counter\ngood 5\n")
	reloadScrapeConfig(t, h, "",
		map[string]any{"id": "t", "url": url2, "labels": map[string]string{}, "enabled": true},
	)
	rec = scrapeRequest(t, h, `{"target_ids":["t"]}`, "", "")
	if r := decodeBody(t, rec)["results"].([]any)[0].(map[string]any); r["status"] != "ok" {
		t.Fatalf("rescrape = %v", r)
	}
	if v, _ := queryMetricValue(t, h, "", "good", map[string]string{"target_id": "t"}); v != 5 {
		t.Fatalf("good = %v, want full 5", v)
	}
}

func postMetricsAsTenant(t *testing.T, h http.Handler, tenant, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, metricsPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		req.Header.Set(tenantHeader, tenant)
	}
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("seed metrics = %d %q", rec.Code, rec.Body.String())
	}
}

func TestDiscoveryScrapeMethodsMediaTypeTenancy(t *testing.T) {
	h := Handler()

	rec := discoveryRequest(t, h, http.MethodGet, discoveryTargetsScrapePath, "", "", "")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("Allow = %q, want POST", allow)
	}
	rec = discoveryRequest(t, h, http.MethodDelete, discoveryTargetsScrapePath, "", "", "")
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")

	rec = scrapeRequest(t, h, `{"target_ids":[]}`, "text/plain", "")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")

	// Invalid tenant wins over media type and body checks.
	rec = discoveryRequest(t, h, http.MethodPost, discoveryTargetsScrapePath, "not json", "text/plain", "bad tenant!")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")

	// Tenant isolation: alpha's scrape is invisible to beta.
	_, url := metricsTarget(t, http.StatusOK, "# TYPE m gauge\nm 1\n")
	reloadScrapeConfig(t, h, "alpha",
		map[string]any{"id": "t", "url": url, "labels": map[string]string{}, "enabled": true},
	)
	if rec := scrapeRequest(t, h, `{"target_ids":["t"]}`, "", "alpha"); rec.Code != http.StatusOK {
		t.Fatalf("alpha scrape = %d %q", rec.Code, rec.Body.String())
	}
	if _, ok := queryMetricValue(t, h, "beta", "m", map[string]string{"target_id": "t"}); ok {
		t.Fatal("beta observed alpha's scraped metric")
	}
	if _, ok := queryMetricValue(t, h, "beta", "m", nil); ok {
		t.Fatal("beta observed alpha's scraped metric")
	}
}
