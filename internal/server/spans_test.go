package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- helpers ---------------------------------------------------------------

const (
	testTraceID = "0123456789abcdef0123456789abcdef"
	testSpanID  = "0123456789abcdef"
)

func makeSpan(traceID, spanID, parent, service, name, start, end, status string) string {
	parentJSON := "null"
	if parent != "" {
		parentJSON = fmt.Sprintf("%q", parent)
	}
	return fmt.Sprintf(`{"trace_id":%q,"span_id":%q,"parent_span_id":%s,"service":%q,"name":%q,`+
		`"start_time":%q,"end_time":%q,"status":%q,"attributes":{"http_method":"GET"}}`,
		traceID, spanID, parentJSON, service, name, start, end, status)
}

func postSpans(t *testing.T, h http.Handler, body, contentType string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodPost, spansPath, body, contentType, tenants...)
}

func postSpanBatch(t *testing.T, h http.Handler, spans []string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return postSpans(t, h, `{"spans":[`+strings.Join(spans, ",")+`]}`, "application/json", tenants...)
}

func getTrace(t *testing.T, h http.Handler, target string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodGet, tracesPrefix+target, "", "", tenants...)
}

type traceDetailBody struct {
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

func decodeTraceDetail(t *testing.T, rec *httptest.ResponseRecorder) traceDetailBody {
	t.Helper()
	var body traceDetailBody
	if err := decodeJSON(rec, &body); err != nil {
		t.Fatalf("decode trace detail: %v", err)
	}
	return body
}

// ---- POST /api/v1/spans ----------------------------------------------------

func TestSpansPostAccepted(t *testing.T) {
	h := Handler()
	rec := postSpanBatch(t, h, []string{
		makeSpan(testTraceID, testSpanID, "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok"),
		makeSpan(testTraceID, "aaaaaaaaaaaaaaaa", testSpanID, "db", "query", "2026-01-02T03:04:05.1+02:00", "2026-01-02T03:04:05.2+02:00", "unset"),
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

func TestSpansPostReplayAndConflict(t *testing.T) {
	h := Handler()
	span := makeSpan(testTraceID, testSpanID, "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok")
	if rec := postSpanBatch(t, h, []string{span}); rec.Code != http.StatusAccepted {
		t.Fatalf("first post status = %d, want 202", rec.Code)
	}

	// Identical content is a replay; state and counts reflect no new write.
	rec := postSpanBatch(t, h, []string{span})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d, want 202", rec.Code)
	}
	var payload map[string]int
	if err := decodeJSON(rec, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["accepted"] != 0 || payload["replayed"] != 1 {
		t.Fatalf("payload = %v, want accepted=0 replayed=1", payload)
	}

	// Same identity with different content conflicts and changes nothing.
	changed := makeSpan(testTraceID, testSpanID, "", "web", "renamed", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok")
	rec = postSpanBatch(t, h, []string{changed})
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409", rec.Code)
	}
	if code := errorCode(t, rec); code != "span_conflict" {
		t.Fatalf("error code = %q, want span_conflict", code)
	}
	detail := decodeTraceDetail(t, getTrace(t, h, testTraceID))
	if len(detail.Spans) != 1 || detail.Spans[0].Name != "root" {
		t.Fatalf("state mutated by conflict: %+v", detail.Spans)
	}
}

func TestSpansPostConflictWithinBatch(t *testing.T) {
	h := Handler()
	a := makeSpan(testTraceID, testSpanID, "", "web", "one", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok")
	b := makeSpan(testTraceID, testSpanID, "", "web", "two", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok")
	rec := postSpanBatch(t, h, []string{a, b})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if code := errorCode(t, rec); code != "span_conflict" {
		t.Fatalf("error code = %q, want span_conflict", code)
	}
	if rec := getTrace(t, h, testTraceID); rec.Code != http.StatusNotFound {
		t.Fatalf("state mutated by conflicting batch: status = %d", rec.Code)
	}
}

func TestSpansPostInvalid(t *testing.T) {
	h := Handler()
	valid := makeSpan(testTraceID, testSpanID, "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok")

	cases := map[string]string{
		"empty batch":          `{"spans":[]}`,
		"missing spans key":    `{}`,
		"unknown envelope key": `{"spans":[` + valid + `],"extra":1}`,
		"unknown span key":     `{"spans":[{"trace_id":"` + testTraceID + `","span_id":"` + testSpanID + `","parent_span_id":null,"service":"web","name":"root","start_time":"2026-01-02T03:04:05Z","end_time":"2026-01-02T03:04:06Z","status":"ok","attributes":{},"extra":1}]}`,
		"missing field":        `{"spans":[{"trace_id":"` + testTraceID + `","span_id":"` + testSpanID + `","service":"web","name":"root","start_time":"2026-01-02T03:04:05Z","end_time":"2026-01-02T03:04:06Z","status":"ok","attributes":{}}]}`,
		"short trace id":       `{"spans":[` + makeSpan("abc", testSpanID, "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok") + `]}`,
		"upper trace id":       `{"spans":[` + makeSpan("0123456789ABCDEF0123456789ABCDEF", testSpanID, "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok") + `]}`,
		"short span id":        `{"spans":[` + makeSpan(testTraceID, "abc", "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok") + `]}`,
		"parent is self":       `{"spans":[` + makeSpan(testTraceID, testSpanID, testSpanID, "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok") + `]}`,
		"bad parent format":    `{"spans":[` + makeSpan(testTraceID, testSpanID, "xyz", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok") + `]}`,
		"empty service":        `{"spans":[` + makeSpan(testTraceID, testSpanID, "", "", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok") + `]}`,
		"empty name":           `{"spans":[` + makeSpan(testTraceID, testSpanID, "", "web", "", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok") + `]}`,
		"no timezone":          `{"spans":[` + makeSpan(testTraceID, testSpanID, "", "web", "root", "2026-01-02 03:04:05", "2026-01-02T03:04:06Z", "ok") + `]}`,
		"end before start":     `{"spans":[` + makeSpan(testTraceID, testSpanID, "", "web", "root", "2026-01-02T03:04:06Z", "2026-01-02T03:04:05Z", "ok") + `]}`,
		"bad status":           `{"spans":[` + makeSpan(testTraceID, testSpanID, "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "broken") + `]}`,
		"null attributes":      `{"spans":[{"trace_id":"` + testTraceID + `","span_id":"` + testSpanID + `","parent_span_id":null,"service":"web","name":"root","start_time":"2026-01-02T03:04:05Z","end_time":"2026-01-02T03:04:06Z","status":"ok","attributes":null}]}`,
		"null attribute value": `{"spans":[{"trace_id":"` + testTraceID + `","span_id":"` + testSpanID + `","parent_span_id":null,"service":"web","name":"root","start_time":"2026-01-02T03:04:05Z","end_time":"2026-01-02T03:04:06Z","status":"ok","attributes":{"k":null}}]}`,
		"bad attribute key":    `{"spans":[{"trace_id":"` + testTraceID + `","span_id":"` + testSpanID + `","parent_span_id":null,"service":"web","name":"root","start_time":"2026-01-02T03:04:05Z","end_time":"2026-01-02T03:04:06Z","status":"ok","attributes":{"1bad":"v"}}]}`,
		"trailing data":        `{"spans":[` + valid + `]} {}`,
	}
	for name, body := range cases {
		rec := postSpans(t, h, body, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
			continue
		}
		if code := errorCode(t, rec); code != "invalid_spans" {
			t.Errorf("%s: error code = %q, want invalid_spans", name, code)
		}
	}
	if rec := getTrace(t, h, testTraceID); rec.Code != http.StatusNotFound {
		t.Fatalf("invalid batches mutated state: status = %d", rec.Code)
	}
}

func TestSpansPostMediaTypeAndMethod(t *testing.T) {
	h := Handler()
	rec := postSpans(t, h, `{"spans":[]}`, "text/plain")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
	if code := errorCode(t, rec); code != "unsupported_media_type" {
		t.Fatalf("error code = %q, want unsupported_media_type", code)
	}

	rec = doRequest(t, h, http.MethodGet, spansPath, "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", allow)
	}
}

// ---- GET /api/v1/traces/{trace_id} -----------------------------------------

func TestTraceGetDetail(t *testing.T) {
	h := Handler()
	// Child submitted before its parent; parent may arrive later.
	postSpanBatch(t, h, []string{
		makeSpan(testTraceID, "bbbbbbbbbbbbbbbb", "aaaaaaaaaaaaaaaa", "db", "query", "2026-01-02T03:04:06Z", "2026-01-02T03:04:07Z", "ok"),
		makeSpan(testTraceID, "aaaaaaaaaaaaaaaa", "", "web", "root", "2026-01-02T05:04:05+02:00", "2026-01-02T05:04:08+02:00", "error"),
	})
	// An orphan span whose parent never arrived still shows up.
	postSpanBatch(t, h, []string{
		makeSpan(testTraceID, "cccccccccccccccc", "ffffffffffffffff", "cache", "get", "2026-01-02T03:04:05.5Z", "2026-01-02T03:04:05.6Z", "unset"),
	})
	postLogEntries(t, h, []string{
		`{"id":"log-2","timestamp":"2026-01-02T03:04:06Z","level":"info","message":"second","labels":{"app":"demo"},"trace_id":"` + testTraceID + `"}`,
		`{"id":"log-1","timestamp":"2026-01-02T03:04:05Z","level":"error","message":"first","labels":{"app":"demo"},"trace_id":"` + testTraceID + `"}`,
		`{"id":"log-3","timestamp":"2026-01-02T03:04:07Z","level":"info","message":"other trace","labels":{"app":"demo"},"trace_id":"ffffffffffffffffffffffffffffffff"}`,
	})

	rec := getTrace(t, h, testTraceID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	detail := decodeTraceDetail(t, rec)
	if detail.TraceID != testTraceID {
		t.Fatalf("trace_id = %q", detail.TraceID)
	}
	if len(detail.Spans) != 3 {
		t.Fatalf("span count = %d, want 3", len(detail.Spans))
	}
	// Sorted by start instant ascending, span id ascending on ties; UTC output.
	gotOrder := []string{detail.Spans[0].SpanID, detail.Spans[1].SpanID, detail.Spans[2].SpanID}
	wantOrder := []string{"aaaaaaaaaaaaaaaa", "cccccccccccccccc", "bbbbbbbbbbbbbbbb"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("span order = %v, want %v", gotOrder, wantOrder)
		}
	}
	root := detail.Spans[0]
	if root.StartTime != "2026-01-02T03:04:05Z" || root.EndTime != "2026-01-02T03:04:08Z" {
		t.Fatalf("root times not UTC: %q %q", root.StartTime, root.EndTime)
	}
	if root.ParentSpanID != nil {
		t.Fatalf("root parent = %v, want null", *root.ParentSpanID)
	}
	if root.Service != "web" || root.Name != "root" || root.Status != "error" {
		t.Fatalf("root fields = %+v", root)
	}
	if root.Attributes["http_method"] != "GET" {
		t.Fatalf("root attributes = %v", root.Attributes)
	}
	if p := detail.Spans[2].ParentSpanID; p == nil || *p != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("child parent = %v", p)
	}

	// Logs: only matching trace id, timestamp ascending.
	if len(detail.Logs) != 2 || detail.Logs[0].ID != "log-1" || detail.Logs[1].ID != "log-2" {
		t.Fatalf("logs = %+v", detail.Logs)
	}
}

func TestTraceGetNotFound(t *testing.T) {
	h := Handler()
	// Logs alone never create a trace.
	postLogEntries(t, h, []string{
		`{"id":"log-1","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":{},"trace_id":"` + testTraceID + `"}`,
	})
	rec := getTrace(t, h, testTraceID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec); code != "trace_not_found" {
		t.Fatalf("error code = %q, want trace_not_found", code)
	}
}

func TestTraceGetInvalidRequests(t *testing.T) {
	h := Handler()
	postSpanBatch(t, h, []string{
		makeSpan(testTraceID, testSpanID, "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok"),
	})

	rec := getTrace(t, h, "not-a-trace-id")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id status = %d, want 400", rec.Code)
	}
	if code := errorCode(t, rec); code != "invalid_trace_id" {
		t.Fatalf("error code = %q, want invalid_trace_id", code)
	}

	rec = getTrace(t, h, testTraceID+"?verbose=true")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("query status = %d, want 400", rec.Code)
	}
	if code := errorCode(t, rec); code != "invalid_trace_query" {
		t.Fatalf("error code = %q, want invalid_trace_query", code)
	}

	rec = doRequest(t, h, http.MethodDelete, tracesPrefix+testTraceID, "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", allow)
	}
}

func TestSpansTenantIsolation(t *testing.T) {
	h := Handler()
	postSpanBatch(t, h, []string{
		makeSpan(testTraceID, testSpanID, "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok"),
	}, "tenant-a")

	if rec := getTrace(t, h, testTraceID, "tenant-b"); rec.Code != http.StatusNotFound {
		t.Fatalf("tenant-b status = %d, want 404", rec.Code)
	}
	if rec := getTrace(t, h, testTraceID); rec.Code != http.StatusNotFound {
		t.Fatalf("default tenant status = %d, want 404", rec.Code)
	}
	if rec := getTrace(t, h, testTraceID, "tenant-a"); rec.Code != http.StatusOK {
		t.Fatalf("tenant-a status = %d, want 200", rec.Code)
	}
}
