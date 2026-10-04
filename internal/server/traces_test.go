package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- helpers ---------------------------------------------------------------

func getTraces(t *testing.T, h http.Handler, query string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodGet, tracesCollectionPath+query, "", "", tenants...)
}

type traceList struct {
	Traces []struct {
		TraceID   string   `json:"trace_id"`
		StartTime string   `json:"start_time"`
		EndTime   string   `json:"end_time"`
		SpanCount int      `json:"span_count"`
		Services  []string `json:"services"`
		Status    string   `json:"status"`
	} `json:"traces"`
	NextCursor *string `json:"next_cursor"`
}

func decodeTraceList(t *testing.T, rec *httptest.ResponseRecorder) traceList {
	t.Helper()
	var l traceList
	if err := decodeJSON(rec, &l); err != nil {
		t.Fatalf("decode trace list: %v", err)
	}
	return l
}

// seedTrace posts one span per id: traceID gets span 00000000000000NN.
func seedTrace(t *testing.T, h http.Handler, traceID, spanID, service, name, start, end, status string, tenants ...string) {
	t.Helper()
	rec := postSpanBatch(t, h, []string{
		makeSpan(traceID, spanID, "", service, name, start, end, status, `{}`),
	}, tenants...)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("seed %s: status = %d", traceID, rec.Code)
	}
}

const window = "?start=2026-01-02T00:00:00Z&end=2026-01-03T00:00:00Z"

// ---- validation ------------------------------------------------------------

func TestTracesGetInvalidQuery(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"missing both":       "",
		"missing end":        "?start=2026-01-02T00:00:00Z",
		"missing start":      "?end=2026-01-03T00:00:00Z",
		"repeated start":     "?start=2026-01-02T00:00:00Z&start=2026-01-02T00:00:00Z&end=2026-01-03T00:00:00Z",
		"repeated limit":     window + "&limit=5&limit=5",
		"unknown param":      window + "&foo=bar",
		"naive start":        "?start=2026-01-02T00:00:00&end=2026-01-03T00:00:00Z",
		"bad end":            "?start=2026-01-02T00:00:00Z&end=tomorrow",
		"start after end":    "?start=2026-01-04T00:00:00Z&end=2026-01-03T00:00:00Z",
		"start equal end":    "?start=2026-01-03T00:00:00Z&end=2026-01-03T00:00:00Z",
		"same instant zones": "?start=2026-01-02T08:00:00%2B08:00&end=2026-01-02T00:00:00Z",
		"empty service":      window + "&service=",
		"empty name":         window + "&name=",
		"bad status":         window + "&status=failed",
		"status case":        window + "&status=OK",
		"limit zero":         window + "&limit=0",
		"limit too big":      window + "&limit=201",
		"limit not number":   window + "&limit=many",
		"limit float":        window + "&limit=1.5",
		"bad escape":         "?start=%zz&end=2026-01-03T00:00:00Z",
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			rec := getTraces(t, h, query)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_trace_query" {
				t.Fatalf("status = %d code = %q, want 400 invalid_trace_query", rec.Code, errorCode(t, rec))
			}
		})
	}
}

func TestTracesGetEmpty(t *testing.T) {
	h := Handler()
	rec := getTraces(t, h, window)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	l := decodeTraceList(t, rec)
	if l.Traces == nil || len(l.Traces) != 0 {
		t.Fatalf("traces = %v, want empty array", l.Traces)
	}
	if l.NextCursor != nil {
		t.Fatalf("next_cursor = %v, want null", *l.NextCursor)
	}
}

func TestTracesCollectionMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := doRequest(t, h, method, tracesCollectionPath, `{}`, "application/json")
		if rec.Code != http.StatusMethodNotAllowed || errorCode(t, rec) != "method_not_allowed" {
			t.Fatalf("%s: status = %d code = %q, want 405 method_not_allowed", method, rec.Code, errorCode(t, rec))
		}
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Fatalf("%s: Allow = %q, want GET", method, got)
		}
	}
}

// ---- summaries -------------------------------------------------------------

func TestTracesGetSummaries(t *testing.T) {
	h := Handler()
	// Trace A: two spans, mixed services and statuses, offset zones.
	if rec := postSpanBatch(t, h, []string{
		makeSpan("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000001", "", "web", "root",
			"2026-01-02T03:04:05.25Z", "2026-01-02T03:04:09Z", "ok", `{}`),
		makeSpan("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000002", "0000000000000001", "api", "child",
			"2026-01-02T11:04:06+08:00", "2026-01-02T11:04:10+08:00", "error", `{}`),
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("seed A: status = %d", rec.Code)
	}
	// Trace B: single unset span, earlier.
	seedTrace(t, h, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "0000000000000001", "web", "root",
		"2026-01-02T01:00:00Z", "2026-01-02T01:00:01Z", "unset")

	rec := getTraces(t, h, window)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	l := decodeTraceList(t, rec)
	if len(l.Traces) != 2 {
		t.Fatalf("traces = %d, want 2", len(l.Traces))
	}
	// Newest start_time first.
	a := l.Traces[0]
	if a.TraceID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("first trace = %q, want aaaa...", a.TraceID)
	}
	if a.StartTime != "2026-01-02T03:04:05.25Z" || a.EndTime != "2026-01-02T03:04:10Z" {
		t.Fatalf("A window = %q..%q, want UTC-normalized min start / max end", a.StartTime, a.EndTime)
	}
	if a.SpanCount != 2 || a.Status != "error" {
		t.Fatalf("A count/status = %d/%q, want 2/error", a.SpanCount, a.Status)
	}
	if len(a.Services) != 2 || a.Services[0] != "api" || a.Services[1] != "web" {
		t.Fatalf("A services = %v, want [api web]", a.Services)
	}
	b := l.Traces[1]
	if b.TraceID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || b.Status != "unset" ||
		b.SpanCount != 1 || len(b.Services) != 1 || b.Services[0] != "web" {
		t.Fatalf("B summary = %+v", b)
	}
	if l.NextCursor != nil {
		t.Fatalf("next_cursor = %v, want null", *l.NextCursor)
	}
}

func TestTracesGetWindowBounds(t *testing.T) {
	h := Handler()
	// Trace time is the earliest span start: one span starts before the
	// window, but a later span starts inside — the trace is still excluded.
	if rec := postSpanBatch(t, h, []string{
		makeSpan("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000001", "", "web", "early",
			"2026-01-01T23:00:00Z", "2026-01-01T23:30:00Z", "ok", `{}`),
		makeSpan("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000002", "", "web", "inside",
			"2026-01-02T12:00:00Z", "2026-01-02T12:30:00Z", "ok", `{}`),
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("seed early: status = %d", rec.Code)
	}
	// Exactly at start: included. Exactly at end: excluded.
	seedTrace(t, h, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "0000000000000001", "web", "at-start",
		"2026-01-02T00:00:00Z", "2026-01-02T00:00:01Z", "ok")
	seedTrace(t, h, "cccccccccccccccccccccccccccccccc", "0000000000000001", "web", "at-end",
		"2026-01-03T00:00:00Z", "2026-01-03T00:00:01Z", "ok")

	l := decodeTraceList(t, getTraces(t, h, window))
	if len(l.Traces) != 1 || l.Traces[0].TraceID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("traces = %+v, want only the at-start trace", l.Traces)
	}
}

func TestTracesGetSpanFiltersSameSpan(t *testing.T) {
	h := Handler()
	// Trace A: service and name conditions met by different spans — no match.
	if rec := postSpanBatch(t, h, []string{
		makeSpan("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000001", "", "web", "root",
			"2026-01-02T03:00:00Z", "2026-01-02T03:00:01Z", "ok", `{}`),
		makeSpan("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000002", "", "api", "child",
			"2026-01-02T03:00:02Z", "2026-01-02T03:00:03Z", "ok", `{}`),
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("seed A: status = %d", rec.Code)
	}
	// Trace B: one span satisfies service+name+status together.
	seedTrace(t, h, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "0000000000000001", "web", "root",
		"2026-01-02T04:00:00Z", "2026-01-02T04:00:01Z", "error")

	q := window + "&service=web&name=root&status=error"
	l := decodeTraceList(t, getTraces(t, h, q))
	if len(l.Traces) != 1 || l.Traces[0].TraceID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("traces = %+v, want only B", l.Traces)
	}

	// Case-sensitive exact matching.
	for _, q := range []string{
		window + "&service=Web",
		window + "&name=Root",
		window + "&service=we",
	} {
		l := decodeTraceList(t, getTraces(t, h, q))
		if len(l.Traces) != 0 {
			t.Fatalf("%s: traces = %+v, want none", q, l.Traces)
		}
	}
}

func TestTracesGetSkipsSampledAndLogsOnly(t *testing.T) {
	h := Handler()
	// Drop every trace, then write a span: nothing may appear in search.
	rec := doRequest(t, h, http.MethodPut, samplingPolicyPath,
		`{"log_rate":1,"trace_rate":0}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("set policy: status = %d", rec.Code)
	}
	seedTrace(t, h, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000001", "web", "dropped",
		"2026-01-02T03:00:00Z", "2026-01-02T03:00:01Z", "ok")
	// Keep everything again; a traced log alone never creates a searchable
	// trace either.
	rec = doRequest(t, h, http.MethodPut, samplingPolicyPath,
		`{"log_rate":1,"trace_rate":1}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("restore policy: status = %d", rec.Code)
	}
	body := `{"entries":[{"id":"l1","timestamp":"2026-01-02T03:00:00Z","level":"info","message":"hi","labels":{"app":"demo"},"trace_id":"dddddddddddddddddddddddddddddddd"}]}`
	if rec := postLogs(t, h, body); rec.Code != http.StatusAccepted {
		t.Fatalf("seed log: status = %d", rec.Code)
	}
	l := decodeTraceList(t, getTraces(t, h, window))
	if len(l.Traces) != 0 {
		t.Fatalf("traces = %+v, want none", l.Traces)
	}
}

// ---- pagination ------------------------------------------------------------

func TestTracesGetPagination(t *testing.T) {
	h := Handler()
	// Five traces, newest first a5..a1; a3 and a4 share one start instant to
	// exercise the trace_id tiebreak.
	ids := []string{
		"000000000000000000000000000000a1",
		"000000000000000000000000000000a2",
		"000000000000000000000000000000a3",
		"000000000000000000000000000000a4",
		"000000000000000000000000000000a5",
	}
	starts := []string{
		"2026-01-02T01:00:00Z",
		"2026-01-02T02:00:00Z",
		"2026-01-02T03:00:00Z",
		"2026-01-02T03:00:00Z",
		"2026-01-02T05:00:00Z",
	}
	for i, id := range ids {
		seedTrace(t, h, id, "0000000000000001", "web", "root", starts[i], "2026-01-02T06:00:00Z", "ok")
	}
	wantOrder := []string{ids[4], ids[2], ids[3], ids[1], ids[0]} // a5, a3, a4, a2, a1

	var got []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 5 {
			t.Fatalf("pagination did not terminate")
		}
		var rec *httptest.ResponseRecorder
		if cursor == "" {
			rec = getTraces(t, h, window+"&limit=2")
		} else {
			rec = getTraces(t, h, "?cursor="+cursor)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d", page, rec.Code)
		}
		l := decodeTraceList(t, rec)
		for _, tr := range l.Traces {
			got = append(got, tr.TraceID)
		}
		if l.NextCursor == nil {
			break
		}
		// Mid-run writes must not leak into the snapshot.
		if page == 0 {
			seedTrace(t, h, "000000000000000000000000000000b1", "0000000000000001", "web", "late",
				"2026-01-02T07:00:00Z", "2026-01-02T07:00:01Z", "ok")
		}
		cursor = *l.NextCursor
	}
	if strings.Join(got, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("pages = %v, want %v", got, wantOrder)
	}
}

func TestTracesGetCursorErrors(t *testing.T) {
	h := Handler()
	seedTrace(t, h, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000001", "web", "root",
		"2026-01-02T03:00:00Z", "2026-01-02T03:00:01Z", "ok")

	// Mint a real cursor via a paginated first request.
	seedTrace(t, h, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "0000000000000001", "web", "root",
		"2026-01-02T04:00:00Z", "2026-01-02T04:00:01Z", "ok")
	first := decodeTraceList(t, getTraces(t, h, window+"&limit=1"))
	if first.NextCursor == nil {
		t.Fatalf("first page: next_cursor is null, want a cursor")
	}
	good := *first.NextCursor

	cases := map[string]string{
		"garbage":          "?cursor=not-a-cursor",
		"tampered":         "?cursor=" + good[:len(good)-2] + "xx",
		"mixed with param": "?cursor=" + good + "&limit=1",
		"mixed with start": "?cursor=" + good + "&start=2026-01-02T00:00:00Z",
		"repeated cursor":  "?cursor=" + good + "&cursor=" + good,
		"empty cursor":     "?cursor=",
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			rec := getTraces(t, h, q)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_trace_cursor" {
				t.Fatalf("status = %d code = %q, want 400 invalid_trace_cursor", rec.Code, errorCode(t, rec))
			}
		})
	}

	// Cross-tenant use of a valid cursor is rejected.
	rec := getTraces(t, h, "?cursor="+good, "team-b")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_trace_cursor" {
		t.Fatalf("cross-tenant: status = %d code = %q, want 400 invalid_trace_cursor", rec.Code, errorCode(t, rec))
	}

	// The good cursor still works afterwards.
	rec = getTraces(t, h, "?cursor="+good)
	if rec.Code != http.StatusOK {
		t.Fatalf("good cursor: status = %d, want 200", rec.Code)
	}
	l := decodeTraceList(t, rec)
	if len(l.Traces) != 1 || l.NextCursor != nil {
		t.Fatalf("second page = %+v, want one trace and null cursor", l)
	}
}

func TestTracesGetTenantIsolation(t *testing.T) {
	h := Handler()
	seedTrace(t, h, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000001", "web", "root",
		"2026-01-02T03:00:00Z", "2026-01-02T03:00:01Z", "ok", "team-a")

	l := decodeTraceList(t, getTraces(t, h, window, "team-b"))
	if len(l.Traces) != 0 {
		t.Fatalf("team-b traces = %+v, want none", l.Traces)
	}
	l = decodeTraceList(t, getTraces(t, h, window, "team-a"))
	if len(l.Traces) != 1 {
		t.Fatalf("team-a traces = %+v, want one", l.Traces)
	}
}

func TestTracesGetLimitDefaultAndBounds(t *testing.T) {
	h := Handler()
	for i := 0; i < 3; i++ {
		seedTrace(t, h, fmt.Sprintf("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa%02d", i), "0000000000000001",
			"web", "root", fmt.Sprintf("2026-01-02T03:0%d:00Z", i), "2026-01-02T04:00:00Z", "ok")
	}
	// limit=1 paginates; limit=200 is accepted.
	l := decodeTraceList(t, getTraces(t, h, window+"&limit=1"))
	if len(l.Traces) != 1 || l.NextCursor == nil {
		t.Fatalf("limit=1: %+v, want one trace and a cursor", l)
	}
	l = decodeTraceList(t, getTraces(t, h, window+"&limit=200"))
	if len(l.Traces) != 3 || l.NextCursor != nil {
		t.Fatalf("limit=200: %+v, want three traces and null cursor", l)
	}
}
