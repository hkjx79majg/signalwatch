package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- helpers ---------------------------------------------------------------

const sampleTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

func postSpans(t *testing.T, h http.Handler, body string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodPost, spansPath, body, "application/json", tenants...)
}

func postSpanBatch(t *testing.T, h http.Handler, spans []string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return postSpans(t, h, `{"spans":[`+strings.Join(spans, ",")+`]}`, tenants...)
}

func makeSpan(traceID, spanID, parent, service, name, start, end, status string, attrs string) string {
	parentField := "null"
	if parent != "" {
		parentField = fmt.Sprintf("%q", parent)
	}
	return fmt.Sprintf(
		`{"trace_id":%q,"span_id":%q,"parent_span_id":%s,"service":%q,"name":%q,`+
			`"start_time":%q,"end_time":%q,"status":%q,"attributes":%s}`,
		traceID, spanID, parentField, service, name, start, end, status, attrs)
}

func rootSpan(attrs string) string {
	return makeSpan(sampleTraceID, "0000000000000001", "", "web", "root",
		"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", attrs)
}

func getTrace(t *testing.T, h http.Handler, target string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodGet, target, "", "", tenants...)
}

type traceDetail struct {
	TraceID string `json:"trace_id"`
	Spans   []struct {
		TraceID      string            `json:"trace_id"`
		SpanID       string            `json:"span_id"`
		ParentSpanID *string           `json:"parent_span_id"`
		Service      string            `json:"service"`
		Name         string            `json:"name"`
		StartTime    string            `json:"start_time"`
		EndTime      string            `json:"end_time"`
		Status       string            `json:"status"`
		Attributes   map[string]string `json:"attributes"`
	} `json:"spans"`
	Logs []struct {
		ID        string `json:"id"`
		Timestamp string `json:"timestamp"`
	} `json:"logs"`
}

func decodeTrace(t *testing.T, rec *httptest.ResponseRecorder) traceDetail {
	t.Helper()
	var d traceDetail
	if err := decodeJSON(rec, &d); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	return d
}

// ---- POST /api/v1/spans ----------------------------------------------------

func TestSpansPostAccepted(t *testing.T) {
	h := Handler()
	rec := postSpanBatch(t, h, []string{
		makeSpan(sampleTraceID, "0000000000000001", "", "web", "root",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{"route":"/a"}`),
		makeSpan(sampleTraceID, "0000000000000002", "0000000000000001", "api", "child",
			"2026-01-02T11:04:05.5+08:00", "2026-01-02T11:04:05.5+08:00", "unset", `{}`),
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	var payload map[string]int
	if err := decodeJSON(rec, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["accepted"] != 2 || payload["replayed"] != 0 {
		t.Fatalf("payload = %v, want accepted=2 replayed=0", payload)
	}
}

func TestSpansPostLateParent(t *testing.T) {
	h := Handler()
	child := makeSpan(sampleTraceID, "0000000000000002", "0000000000000001", "api", "child",
		"2026-01-02T03:04:06Z", "2026-01-02T03:04:07Z", "ok", `{}`)
	if rec := postSpanBatch(t, h, []string{child}); rec.Code != http.StatusAccepted {
		t.Fatalf("child before parent: status = %d, want 202", rec.Code)
	}
	parent := makeSpan(sampleTraceID, "0000000000000001", "", "web", "root",
		"2026-01-02T03:04:05Z", "2026-01-02T03:04:08Z", "ok", `{}`)
	if rec := postSpanBatch(t, h, []string{parent}); rec.Code != http.StatusAccepted {
		t.Fatalf("parent later: status = %d, want 202", rec.Code)
	}
}

func TestSpansPostReplay(t *testing.T) {
	h := Handler()
	sp := rootSpan(`{"route":"/a"}`)
	if rec := postSpanBatch(t, h, []string{sp}); rec.Code != http.StatusAccepted {
		t.Fatalf("first write: status = %d", rec.Code)
	}
	// Identical content, including the same instant expressed in another zone.
	replay := makeSpan(sampleTraceID, "0000000000000001", "", "web", "root",
		"2026-01-02T11:04:05+08:00", "2026-01-02T11:04:06+08:00", "ok", `{"route":"/a"}`)
	rec := postSpanBatch(t, h, []string{replay})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("replay: status = %d, want 202", rec.Code)
	}
	var payload map[string]int
	if err := decodeJSON(rec, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["accepted"] != 0 || payload["replayed"] != 1 {
		t.Fatalf("payload = %v, want accepted=0 replayed=1", payload)
	}

	// An identical duplicate inside one batch is a replay, not a conflict.
	rec = postSpanBatch(t, h, []string{replay, sp})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("in-batch replay: status = %d, want 202", rec.Code)
	}
	if err := decodeJSON(rec, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["accepted"] != 0 || payload["replayed"] != 2 {
		t.Fatalf("payload = %v, want accepted=0 replayed=2", payload)
	}
}

func TestSpansPostConflict(t *testing.T) {
	h := Handler()
	if rec := postSpanBatch(t, h, []string{rootSpan(`{"route":"/a"}`)}); rec.Code != http.StatusAccepted {
		t.Fatalf("seed: status = %d", rec.Code)
	}

	changed := makeSpan(sampleTraceID, "0000000000000001", "", "web", "root",
		"2026-01-02T03:04:05Z", "2026-01-02T03:04:07Z", "ok", `{"route":"/a"}`)
	rec := postSpanBatch(t, h, []string{changed})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "span_conflict" {
		t.Fatalf("conflict: status = %d code = %q", rec.Code, errorCode(t, rec))
	}

	// A conflict in the second span must not commit the first (new) span.
	other := makeSpan(sampleTraceID, "0000000000000009", "", "web", "other",
		"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`)
	rec = postSpanBatch(t, h, []string{other, changed})
	if rec.Code != http.StatusConflict {
		t.Fatalf("batch conflict: status = %d, want 409", rec.Code)
	}
	rec = getTrace(t, h, tracesPrefix+sampleTraceID)
	d := decodeTrace(t, rec)
	if len(d.Spans) != 1 {
		t.Fatalf("conflict mutated state: spans = %d, want 1", len(d.Spans))
	}
	if d.Spans[0].EndTime != "2026-01-02T03:04:06Z" {
		t.Fatalf("stored end_time = %q, want unchanged", d.Spans[0].EndTime)
	}

	// Two different payloads for one (trace_id, span_id) inside a batch also
	// conflict and leave state untouched.
	a := makeSpan("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000007", "", "web", "a",
		"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`)
	b := makeSpan("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000007", "", "web", "b",
		"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`)
	if rec := postSpanBatch(t, h, []string{a, b}); rec.Code != http.StatusConflict {
		t.Fatalf("in-batch conflict: status = %d, want 409", rec.Code)
	}
	if rec := getTrace(t, h, tracesPrefix+"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); rec.Code != http.StatusNotFound {
		t.Fatalf("in-batch conflict mutated state: status = %d, want 404", rec.Code)
	}
}

func TestSpansPostInvalidBodies(t *testing.T) {
	h := Handler()
	good := rootSpan(`{"route":"/a"}`)
	cases := map[string]string{
		"not json":          `{`,
		"trailing data":     `{"spans":[]} garbage`,
		"missing spans":     `{}`,
		"spans null":        `{"spans":null}`,
		"empty spans":       `{"spans":[]}`,
		"unknown top field": `{"spans":[` + good + `],"extra":1}`,
		"unknown span field": `{"spans":[{` +
			`"trace_id":"` + sampleTraceID + `","span_id":"0000000000000001",` +
			`"parent_span_id":null,"service":"web","name":"root",` +
			`"start_time":"2026-01-02T03:04:05Z","end_time":"2026-01-02T03:04:06Z",` +
			`"status":"ok","attributes":{},"bogus":1}]}`,
		"missing trace_id": `{"spans":[{` +
			`"span_id":"0000000000000001","parent_span_id":null,"service":"web","name":"root",` +
			`"start_time":"2026-01-02T03:04:05Z","end_time":"2026-01-02T03:04:06Z",` +
			`"status":"ok","attributes":{}}]}`,
		"missing parent": `{"spans":[{` +
			`"trace_id":"` + sampleTraceID + `","span_id":"0000000000000001",` +
			`"service":"web","name":"root",` +
			`"start_time":"2026-01-02T03:04:05Z","end_time":"2026-01-02T03:04:06Z",` +
			`"status":"ok","attributes":{}}]}`,
		"uppercase trace id": `{"spans":[` + makeSpan("4BF92F3577B34DA6A3CE929D0E0E4736", "0000000000000001", "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`) + `]}`,
		"trace id too short": `{"spans":[` + makeSpan("4bf92f3577b34da6a3ce929d0e0e473", "0000000000000001", "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`) + `]}`,
		"bad span id":        `{"spans":[` + makeSpan(sampleTraceID, "000000000000000g", "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`) + `]}`,
		"span id too long":   `{"spans":[` + makeSpan(sampleTraceID, "00000000000000001", "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`) + `]}`,
		"parent not string": `{"spans":[{` +
			`"trace_id":"` + sampleTraceID + `","span_id":"0000000000000001","parent_span_id":123,` +
			`"service":"web","name":"root","start_time":"2026-01-02T03:04:05Z",` +
			`"end_time":"2026-01-02T03:04:06Z","status":"ok","attributes":{}}]}`,
		"parent object": `{"spans":[{` +
			`"trace_id":"` + sampleTraceID + `","span_id":"0000000000000001","parent_span_id":{"id":"x"},` +
			`"service":"web","name":"root","start_time":"2026-01-02T03:04:05Z",` +
			`"end_time":"2026-01-02T03:04:06Z","status":"ok","attributes":{}}]}`,
		"parent self":       `{"spans":[` + makeSpan(sampleTraceID, "0000000000000001", "0000000000000001", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`) + `]}`,
		"bad parent format": `{"spans":[` + makeSpan(sampleTraceID, "0000000000000002", "abc", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`) + `]}`,
		"empty service":     `{"spans":[` + makeSpan(sampleTraceID, "0000000000000001", "", "", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`) + `]}`,
		"empty name":        `{"spans":[` + makeSpan(sampleTraceID, "0000000000000001", "", "web", "", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`) + `]}`,
		"service non-string": `{"spans":[{` +
			`"trace_id":"` + sampleTraceID + `","span_id":"0000000000000001","parent_span_id":null,` +
			`"service":7,"name":"root","start_time":"2026-01-02T03:04:05Z",` +
			`"end_time":"2026-01-02T03:04:06Z","status":"ok","attributes":{}}]}`,
		"bad time format":  `{"spans":[` + makeSpan(sampleTraceID, "0000000000000001", "", "web", "root", "2026-01-02 03:04:05", "2026-01-02T03:04:06Z", "ok", `{}`) + `]}`,
		"naive timestamp":  `{"spans":[` + makeSpan(sampleTraceID, "0000000000000001", "", "web", "root", "2026-01-02T03:04:05", "2026-01-02T03:04:06", "ok", `{}`) + `]}`,
		"end before start": `{"spans":[` + makeSpan(sampleTraceID, "0000000000000001", "", "web", "root", "2026-01-02T03:04:06Z", "2026-01-02T03:04:05Z", "ok", `{}`) + `]}`,
		"bad status":       `{"spans":[` + makeSpan(sampleTraceID, "0000000000000001", "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "failed", `{}`) + `]}`,
		"attributes null":  `{"spans":[` + makeSpan(sampleTraceID, "0000000000000001", "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `null`) + `]}`,
		"attribute value null": `{"spans":[{` +
			`"trace_id":"` + sampleTraceID + `","span_id":"0000000000000001","parent_span_id":null,` +
			`"service":"web","name":"root","start_time":"2026-01-02T03:04:05Z",` +
			`"end_time":"2026-01-02T03:04:06Z","status":"ok","attributes":{"route":null}}]}`,
		"bad attribute key": `{"spans":[` + makeSpan(sampleTraceID, "0000000000000001", "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{"bad-key":"v"}`) + `]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := postSpans(t, h, body)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_spans" {
				t.Fatalf("status = %d code = %q, want 400 invalid_spans", rec.Code, errorCode(t, rec))
			}
		})
	}

	// No invalid batch may have created a span.
	rec := getTrace(t, h, tracesPrefix+sampleTraceID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("invalid batches changed state: trace status = %d, want 404", rec.Code)
	}
}

func TestSpansPostUnsupportedMediaType(t *testing.T) {
	h := Handler()
	rec := doRequest(t, h, http.MethodPost, spansPath, `{"spans":[]}`, "text/plain")
	if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
		t.Fatalf("status = %d code = %q", rec.Code, errorCode(t, rec))
	}
}

func TestSpansMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := doRequest(t, h, method, spansPath, "", "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodPost {
			t.Fatalf("%s: Allow = %q, want POST", method, got)
		}
	}
}

// ---- GET /api/v1/traces/{trace_id} ----------------------------------------

func TestTraceGetNotFound(t *testing.T) {
	h := Handler()
	// A log carrying the trace id does not bring the trace into existence.
	body := `{"entries":[{"id":"l1","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"hi","labels":{"app":"demo"},"trace_id":"` + sampleTraceID + `"}]}`
	if rec := postLogs(t, h, body); rec.Code != http.StatusAccepted {
		t.Fatalf("seed log: status = %d", rec.Code)
	}
	rec := getTrace(t, h, tracesPrefix+sampleTraceID)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "trace_not_found" {
		t.Fatalf("status = %d code = %q, want 404 trace_not_found", rec.Code, errorCode(t, rec))
	}
}

func TestTraceGetEchoAndOrdering(t *testing.T) {
	h := Handler()
	spans := []string{
		// Submitted out of order; also spans a missing parent.
		makeSpan(sampleTraceID, "0000000000000003", "0000000000000002", "api", "grand",
			"2026-01-02T03:04:07Z", "2026-01-02T03:04:08Z", "error", `{"x":"2"}`),
		makeSpan(sampleTraceID, "0000000000000002", "0000000000000001", "api", "child",
			"2026-01-02T11:04:06+08:00", "2026-01-02T11:04:07+08:00", "unset", `{"x":"1"}`),
		makeSpan(sampleTraceID, "0000000000000001", "", "web", "root",
			"2026-01-02T03:04:05.25Z", "2026-01-02T03:04:09Z", "ok", `{"route":"/a"}`),
		// Two spans starting at the same instant order by span id.
		makeSpan(sampleTraceID, "000000000000000b", "", "web", "tie-b",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
		makeSpan(sampleTraceID, "000000000000000a", "", "web", "tie-a",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
	}
	if rec := postSpanBatch(t, h, spans); rec.Code != http.StatusAccepted {
		t.Fatalf("seed spans: status = %d", rec.Code)
	}
	// Logs for this trace (plus one foreign trace that must not appear).
	body := `{"entries":[` +
		`{"id":"l1","timestamp":"2026-01-02T03:04:05Z","level":"debug","message":"early","labels":{"app":"demo"},"trace_id":"` + sampleTraceID + `"},` +
		`{"id":"l2","timestamp":"2026-01-02T03:04:06Z","level":"info","message":"same-time","labels":{"app":"demo"},"trace_id":"` + sampleTraceID + `"},` +
		`{"id":"l3","timestamp":"2026-01-02T03:04:06Z","level":"info","message":"later","labels":{"app":"demo"},"trace_id":"` + sampleTraceID + `"},` +
		`{"id":"lx","timestamp":"2026-01-02T03:04:06Z","level":"info","message":"other trace","labels":{"app":"demo"},"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}` +
		`]}`
	if rec := postLogs(t, h, body); rec.Code != http.StatusAccepted {
		t.Fatalf("seed logs: status = %d", rec.Code)
	}

	rec := getTrace(t, h, tracesPrefix+sampleTraceID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	d := decodeTrace(t, rec)
	if d.TraceID != sampleTraceID {
		t.Fatalf("trace_id = %q", d.TraceID)
	}
	if len(d.Spans) != 5 {
		t.Fatalf("spans = %d, want 5", len(d.Spans))
	}
	wantOrder := []string{"000000000000000a", "000000000000000b", "0000000000000001", "0000000000000002", "0000000000000003"}
	for i, want := range wantOrder {
		if d.Spans[i].SpanID != want {
			t.Fatalf("span position %d = %q, want %q", i, d.Spans[i].SpanID, want)
		}
	}
	root := d.Spans[2]
	if root.ParentSpanID != nil || root.Service != "web" || root.Name != "root" ||
		root.StartTime != "2026-01-02T03:04:05.25Z" || root.Status != "ok" ||
		root.Attributes["route"] != "/a" {
		t.Fatalf("root echo wrong: %+v", root)
	}
	child := d.Spans[3]
	if child.ParentSpanID == nil || *child.ParentSpanID != "0000000000000001" {
		t.Fatalf("child parent = %v, want 0000000000000001", child.ParentSpanID)
	}
	if child.StartTime != "2026-01-02T03:04:06Z" {
		t.Fatalf("child start not normalized to UTC: %q", child.StartTime)
	}
	grand := d.Spans[4]
	if grand.ParentSpanID == nil || *grand.ParentSpanID != "0000000000000002" {
		t.Fatalf("missing-parent span not echoed: %+v", grand)
	}

	if len(d.Logs) != 3 {
		t.Fatalf("logs = %d, want 3", len(d.Logs))
	}
	wantLogs := []struct {
		id, ts string
	}{
		{"l1", "2026-01-02T03:04:05Z"},
		{"l2", "2026-01-02T03:04:06Z"},
		{"l3", "2026-01-02T03:04:06Z"},
	}
	for i, want := range wantLogs {
		if d.Logs[i].ID != want.id || d.Logs[i].Timestamp != want.ts {
			t.Fatalf("log position %d = %+v, want %s %s", i, d.Logs[i], want.id, want.ts)
		}
	}
}

func TestTraceGetInvalidPath(t *testing.T) {
	h := Handler()
	for _, target := range []string{
		tracesPrefix + "4BF92F3577B34DA6A3CE929D0E0E4736", // uppercase
		tracesPrefix + "abc",                              // too short
		tracesPrefix + sampleTraceID + "/child",           // nested path
	} {
		rec := getTrace(t, h, target)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_trace_id" {
			t.Fatalf("%s: status = %d code = %q, want 400 invalid_trace_id", target, rec.Code, errorCode(t, rec))
		}
	}
}

func TestTraceGetInvalidQuery(t *testing.T) {
	h := Handler()
	postSpanBatch(t, h, []string{rootSpan(`{}`)})
	for _, target := range []string{
		tracesPrefix + sampleTraceID + "?foo=bar",
		tracesPrefix + sampleTraceID + "?x=%zz",
	} {
		rec := getTrace(t, h, target)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_trace_query" {
			t.Fatalf("%s: status = %d code = %q, want 400 invalid_trace_query", target, rec.Code, errorCode(t, rec))
		}
	}
}

func TestTraceMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := doRequest(t, h, method, tracesPrefix+sampleTraceID, `{}`, "application/json")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Fatalf("%s: Allow = %q, want GET", method, got)
		}
	}
}

func TestTraceCollectionRequiresWindow(t *testing.T) {
	h := Handler()
	rec := getTrace(t, h, "/api/v1/traces")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_trace_query" {
		t.Fatalf("GET /api/v1/traces: status = %d code = %q, want 400 invalid_trace_query",
			rec.Code, errorCode(t, rec))
	}
}

func TestSpansTenantIsolation(t *testing.T) {
	h := Handler()
	if rec := postSpanBatch(t, h, []string{rootSpan(`{}`)}, "team-a"); rec.Code != http.StatusAccepted {
		t.Fatalf("seed team-a: status = %d", rec.Code)
	}
	body := `{"entries":[{"id":"l1","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"hi","labels":{"app":"demo"},"trace_id":"` + sampleTraceID + `"}]}`
	if rec := postLogs(t, h, body, "team-a"); rec.Code != http.StatusAccepted {
		t.Fatalf("seed log team-a: status = %d", rec.Code)
	}

	// team-b has no spans: logs-only must not create a trace there either.
	if rec := postLogs(t, h, body, "team-b"); rec.Code != http.StatusAccepted {
		t.Fatalf("seed log team-b: status = %d", rec.Code)
	}
	if rec := getTrace(t, h, tracesPrefix+sampleTraceID, "team-b"); rec.Code != http.StatusNotFound {
		t.Fatalf("team-b trace: status = %d, want 404", rec.Code)
	}

	// The same (trace_id, span_id) can exist independently in team-b.
	if rec := postSpanBatch(t, h, []string{
		makeSpan(sampleTraceID, "0000000000000001", "", "other", "different",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "error", `{}`),
	}, "team-b"); rec.Code != http.StatusAccepted {
		t.Fatalf("team-b same id: status = %d, want 202", rec.Code)
	}

	rec := getTrace(t, h, tracesPrefix+sampleTraceID, "team-a")
	d := decodeTrace(t, rec)
	if len(d.Spans) != 1 || d.Spans[0].Service != "web" || len(d.Logs) != 1 {
		t.Fatalf("team-a view = %+v, want its own span and log", d)
	}
}
