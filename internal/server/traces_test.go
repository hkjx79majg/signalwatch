package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- helpers ---------------------------------------------------------------

func searchTraces(t *testing.T, h http.Handler, query string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return getTrace(t, h, tracesCollectionPath+query, tenants...)
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
	var d traceList
	if err := decodeJSON(rec, &d); err != nil {
		t.Fatalf("decode trace list: %v", err)
	}
	return d
}

const (
	dayWindow = "?start=2026-01-02T00:00:00Z&end=2026-01-03T00:00:00Z"
)

// ---- aggregation and ordering ----------------------------------------------

func TestTraceSearchEmpty(t *testing.T) {
	h := Handler()
	rec := searchTraces(t, h, dayWindow)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	d := decodeTraceList(t, rec)
	if len(d.Traces) != 0 || d.NextCursor != nil {
		t.Fatalf("payload = %+v, want empty traces and null cursor", d)
	}
}

func TestTraceSearchSummaryAndOrder(t *testing.T) {
	h := Handler()
	// sampleTraceID: three spans across two services, mixed status, earliest
	// start submitted with a +01:00 zone.
	if rec := postSpanBatch(t, h, []string{
		makeSpan(sampleTraceID, "0000000000000001", "", "web", "root",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
		makeSpan(sampleTraceID, "0000000000000002", "0000000000000001", "api", "child",
			"2026-01-02T03:04:07Z", "2026-01-02T03:04:09.5Z", "error", `{}`),
		makeSpan(sampleTraceID, "0000000000000003", "0000000000000001", "web", "slow",
			"2026-01-02T04:04:04+01:00", "2026-01-02T03:04:10Z", "unset", `{}`),
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("seed mixed trace: status = %d", rec.Code)
	}
	// Older, ok-only trace.
	okTrace := "22222222222222222222222222222222"
	if rec := postSpanBatch(t, h, []string{
		makeSpan(okTrace, "0000000000000001", "", "db", "query",
			"2026-01-02T03:00:00Z", "2026-01-02T03:00:02Z", "ok", `{}`),
		makeSpan(okTrace, "0000000000000002", "0000000000000001", "db", "scan",
			"2026-01-02T03:00:01Z", "2026-01-02T03:00:01Z", "unset", `{}`),
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("seed ok trace: status = %d", rec.Code)
	}
	// Unset-only trace, starting at the same instant as the ok trace; it must
	// sort after it by trace id.
	unsetTrace := "33333333333333333333333333333333"
	if rec := postSpanBatch(t, h, []string{
		makeSpan(unsetTrace, "0000000000000001", "", "cache", "get",
			"2026-01-02T03:00:00Z", "2026-01-02T03:00:00Z", "unset", `{}`),
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("seed unset trace: status = %d", rec.Code)
	}

	rec := searchTraces(t, h, dayWindow)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	d := decodeTraceList(t, rec)
	if d.NextCursor != nil {
		t.Fatalf("next_cursor = %q, want null", *d.NextCursor)
	}
	if len(d.Traces) != 3 {
		t.Fatalf("traces = %d, want 3", len(d.Traces))
	}

	// Order: newest earliest start first; ties broken by trace id ascending.
	wantOrder := []string{sampleTraceID, okTrace, unsetTrace}
	for i, want := range wantOrder {
		if d.Traces[i].TraceID != want {
			t.Fatalf("position %d = %q, want %q", i, d.Traces[i].TraceID, want)
		}
	}

	top := d.Traces[0]
	if top.StartTime != "2026-01-02T03:04:04Z" || top.EndTime != "2026-01-02T03:04:10Z" {
		t.Fatalf("bounds = %q..%q, want 03:04:04Z..03:04:10Z", top.StartTime, top.EndTime)
	}
	if top.SpanCount != 3 {
		t.Fatalf("span_count = %d, want 3", top.SpanCount)
	}
	if len(top.Services) != 2 || top.Services[0] != "api" || top.Services[1] != "web" {
		t.Fatalf("services = %v, want [api web]", top.Services)
	}
	if top.Status != "error" {
		t.Fatalf("status = %q, want error (error outranks ok)", top.Status)
	}
	if d.Traces[1].Status != "ok" {
		t.Fatalf("ok-trace status = %q, want ok", d.Traces[1].Status)
	}
	if d.Traces[2].Status != "unset" {
		t.Fatalf("unset-trace status = %q, want unset", d.Traces[2].Status)
	}
}

func TestTraceSearchWindowHalfOpen(t *testing.T) {
	h := Handler()
	if rec := postSpanBatch(t, h, []string{
		makeSpan(sampleTraceID, "0000000000000001", "", "web", "root",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("seed: status = %d", rec.Code)
	}

	// Earliest start equal to start: included.
	rec := searchTraces(t, h, "?start=2026-01-02T03:04:05Z&end=2026-01-02T04:00:00Z")
	if d := decodeTraceList(t, rec); len(d.Traces) != 1 {
		t.Fatalf("at lower bound: traces = %d, want 1", len(d.Traces))
	}
	// Earliest start equal to end: excluded (half-open).
	rec = searchTraces(t, h, "?start=2026-01-02T02:00:00Z&end=2026-01-02T03:04:05Z")
	if d := decodeTraceList(t, rec); len(d.Traces) != 0 {
		t.Fatalf("at upper bound: traces = %d, want 0", len(d.Traces))
	}
	// Spans ending inside the window do not count: the trace time is the
	// earliest span start, which lies before the window.
	rec = searchTraces(t, h, "?start=2026-01-02T03:04:05.5Z&end=2026-01-02T03:04:07Z")
	if d := decodeTraceList(t, rec); len(d.Traces) != 0 {
		t.Fatalf("start before window: traces = %d, want 0", len(d.Traces))
	}
}

func TestTraceSearchSpanFeaturesSameSpan(t *testing.T) {
	h := Handler()
	if rec := postSpanBatch(t, h, []string{
		makeSpan(sampleTraceID, "0000000000000001", "", "web", "root",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
		makeSpan(sampleTraceID, "0000000000000002", "0000000000000001", "api", "child",
			"2026-01-02T03:04:07Z", "2026-01-02T03:04:08Z", "error", `{}`),
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("seed: status = %d", rec.Code)
	}

	cases := map[string]int{
		"service=web":                         1,
		"service=api":                         1,
		"service=WEB":                         0, // case-sensitive exact match
		"name=root":                           1,
		"name=Root":                           0,
		"status=error":                        1,
		"status=unset":                        0,
		"service=web&name=root":               1,
		"service=web&status=ok":               1,
		"service=web&status=error":            0, // no single span is both
		"service=api&name=root":               0, // features live on different spans
		"service=web&name=child":              0,
		"name=root&status=error":              0,
		"service=api&name=child&status=error": 1,
	}
	for query, want := range cases {
		t.Run(query, func(t *testing.T) {
			rec := searchTraces(t, h, dayWindow+"&"+query)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if d := decodeTraceList(t, rec); len(d.Traces) != want {
				t.Fatalf("traces = %d, want %d", len(d.Traces), want)
			}
		})
	}

	// Feature filters never change the aggregate status/services of a match.
	rec := searchTraces(t, h, dayWindow+"&service=api&status=error")
	d := decodeTraceList(t, rec)
	if len(d.Traces) != 1 {
		t.Fatalf("traces = %d, want 1", len(d.Traces))
	}
	if got := d.Traces[0]; got.SpanCount != 2 || got.Status != "error" ||
		len(got.Services) != 2 || got.Services[0] != "api" || got.Services[1] != "web" {
		t.Fatalf("summary filtered by one span but aggregated over all: %+v", got)
	}
}

// ---- invalid first requests ------------------------------------------------

func TestTraceSearchInvalidQuery(t *testing.T) {
	h := Handler()
	postSpanBatch(t, h, []string{
		makeSpan(sampleTraceID, "0000000000000001", "", "web", "root",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
	})

	cases := []string{
		"",
		"?start=2026-01-02T00:00:00Z",
		"?end=2026-01-03T00:00:00Z",
		"?start=2026-01-02T00:00:00Z",
		"?start=2026-01-02T00:00:00Z&end=2026-01-03T00:00:00Z&start=2026-01-02T01:00:00Z",
		"?start=not-a-time&end=2026-01-03T00:00:00Z",
		"?start=2026-01-02T00:00:00&end=2026-01-03T00:00:00Z", // naive, no zone
		"?start=2026-01-03T00:00:00Z&end=2026-01-02T00:00:00Z",
		"?start=2026-01-02T00:00:00Z&end=2026-01-02T00:00:00Z",
		"?start=&end=2026-01-03T00:00:00Z",
		dayWindow + "&service=",
		dayWindow + "&name=",
		dayWindow + "&status=failed",
		dayWindow + "&status=OK",
		dayWindow + "&limit=0",
		dayWindow + "&limit=201",
		dayWindow + "&limit=-1",
		dayWindow + "&limit=abc",
		dayWindow + "&limit=1&limit=2",
		dayWindow + "&service=web&service=api",
		dayWindow + "&unknown=1",
		"?x=%zz",
	}
	for _, query := range cases {
		t.Run(query, func(t *testing.T) {
			rec := searchTraces(t, h, query)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_trace_query" {
				t.Fatalf("%s: status = %d code = %q, want 400 invalid_trace_query",
					query, rec.Code, errorCode(t, rec))
			}
		})
	}

	// limit defaults to 100 and accepts the range endpoints.
	for _, limit := range []string{"1", "200"} {
		rec := searchTraces(t, h, dayWindow+"&limit="+limit)
		if rec.Code != http.StatusOK {
			t.Fatalf("limit=%s: status = %d, want 200", limit, rec.Code)
		}
	}
}

// ---- pagination ------------------------------------------------------------

func TestTraceSearchPagination(t *testing.T) {
	h := Handler()
	traceA := "11111111111111111111111111111111" // newest
	traceB := "22222222222222222222222222222222" // tied start, lower id
	traceC := "33333333333333333333333333333333" // tied start, higher id
	for _, spec := range []struct {
		traceID, start string
	}{
		{traceC, "2026-01-02T03:00:00Z"},
		{traceA, "2026-01-02T03:05:00Z"},
		{traceB, "2026-01-02T03:00:00Z"},
	} {
		if rec := postSpanBatch(t, h, []string{
			makeSpan(spec.traceID, "0000000000000001", "", "web", "root",
				spec.start, "2026-01-02T03:10:00Z", "ok", `{}`),
		}); rec.Code != http.StatusAccepted {
			t.Fatalf("seed %s: status = %d", spec.traceID, rec.Code)
		}
	}

	rec := searchTraces(t, h, dayWindow+"&limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("first page: status = %d", rec.Code)
	}
	page1 := decodeTraceList(t, rec)
	if len(page1.Traces) != 2 {
		t.Fatalf("page1 size = %d, want 2", len(page1.Traces))
	}
	if page1.Traces[0].TraceID != traceA || page1.Traces[1].TraceID != traceB {
		t.Fatalf("page1 = %q, %q, want A, B", page1.Traces[0].TraceID, page1.Traces[1].TraceID)
	}
	if page1.NextCursor == nil {
		t.Fatal("page1 next_cursor = null, want a token")
	}
	cursor := *page1.NextCursor

	// A trace written after the first request must not enter the snapshot.
	laterTrace := "44444444444444444444444444444444"
	if rec := postSpanBatch(t, h, []string{
		makeSpan(laterTrace, "0000000000000001", "", "web", "root",
			"2026-01-02T09:00:00Z", "2026-01-02T09:00:01Z", "ok", `{}`),
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("late write: status = %d", rec.Code)
	}

	rec = searchTraces(t, h, "?cursor="+cursor)
	if rec.Code != http.StatusOK {
		t.Fatalf("second page: status = %d", rec.Code)
	}
	page2 := decodeTraceList(t, rec)
	if len(page2.Traces) != 1 || page2.Traces[0].TraceID != traceC {
		t.Fatalf("page2 = %+v, want only C", page2.Traces)
	}
	if page2.NextCursor != nil {
		t.Fatalf("last page cursor = %q, want null", *page2.NextCursor)
	}

	// Replaying the earlier cursor returns the same page: the snapshot and
	// position are fixed, so the late trace never mixes in and no trace repeats.
	rec = searchTraces(t, h, "?cursor="+cursor)
	d := decodeTraceList(t, rec)
	if len(d.Traces) != 1 || d.Traces[0].TraceID != traceC {
		t.Fatalf("cursor replay = %+v, want stable C page", d.Traces)
	}
}

func TestTraceSearchInvalidCursor(t *testing.T) {
	h := Handler()
	postSpanBatch(t, h, []string{
		makeSpan(sampleTraceID, "0000000000000001", "", "web", "root",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
		makeSpan("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000001", "", "web", "root",
			"2026-01-02T03:01:05Z", "2026-01-02T03:01:06Z", "ok", `{}`),
	})
	good := searchTraces(t, h, dayWindow+"&limit=1")
	token := *decodeTraceList(t, good).NextCursor

	// Flip one payload character to another valid base64url character: the
	// token decodes but the HMAC no longer matches.
	tampered := token
	if c := tampered[0]; c == 'A' {
		tampered = "B" + tampered[1:]
	} else {
		tampered = "A" + tampered[1:]
	}

	cases := []string{
		"",
		"garbage",
		strings.SplitN(token, ".", 2)[0],
		token + "x",
		tampered,
	}
	for _, cursor := range cases {
		t.Run(cursor, func(t *testing.T) {
			rec := searchTraces(t, h, "?cursor="+cursor)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_trace_cursor" {
				t.Fatalf("status = %d code = %q, want 400 invalid_trace_cursor",
					rec.Code, errorCode(t, rec))
			}
		})
	}

	// Cursor mixed with any other parameter (including a second cursor) is a
	// cursor error, not a query error.
	for _, query := range []string{
		"?cursor=" + token + "&start=2026-01-02T00:00:00Z",
		"?cursor=" + token + "&limit=1",
		"?cursor=" + token + "&cursor=" + token,
	} {
		rec := searchTraces(t, h, query)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_trace_cursor" {
			t.Fatalf("%s: status = %d code = %q, want 400 invalid_trace_cursor",
				query, rec.Code, errorCode(t, rec))
		}
	}

	// A cursor minted for another tenant is rejected.
	rec := searchTraces(t, h, "?cursor="+token, "team-a")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_trace_cursor" {
		t.Fatalf("cross-tenant cursor: status = %d code = %q", rec.Code, errorCode(t, rec))
	}
}

// ---- method handling, sampling, tenant isolation ---------------------------

func TestTraceCollectionMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := doRequest(t, h, method, tracesCollectionPath, `{}`, "application/json")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Fatalf("%s: Allow = %q, want GET", method, got)
		}
		if errorCode(t, rec) != "method_not_allowed" {
			t.Fatalf("%s: code = %q, want method_not_allowed", method, errorCode(t, rec))
		}
	}
}

func TestTraceSearchExcludesDroppedSpansAndLogsOnly(t *testing.T) {
	h := Handler()
	// A log carrying a trace id never creates a trace by itself.
	body := `{"entries":[{"id":"l1","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"hi","labels":{},"trace_id":"` + sampleTraceID + `"}]}`
	if rec := postLogs(t, h, body); rec.Code != http.StatusAccepted {
		t.Fatalf("seed log: status = %d", rec.Code)
	}

	// With trace_rate 0 every new span is sampled out and leaves no trace.
	put := doRequest(t, h, http.MethodPut, "/api/v1/sampling-policy",
		`{"log_rate":1,"trace_rate":0}`, "application/json")
	if put.Code != http.StatusOK {
		t.Fatalf("disable trace sampling: status = %d", put.Code)
	}
	if rec := postSpanBatch(t, h, []string{
		makeSpan("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000001", "", "web", "root",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("sampled-out write: status = %d", rec.Code)
	}

	rec := searchTraces(t, h, dayWindow)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if d := decodeTraceList(t, rec); len(d.Traces) != 0 {
		t.Fatalf("traces = %d, want 0 (logs-only and sampled-out excluded)", len(d.Traces))
	}
}

func TestTraceSearchTenantIsolation(t *testing.T) {
	h := Handler()
	if rec := postSpanBatch(t, h, []string{
		makeSpan(sampleTraceID, "0000000000000001", "", "web", "root",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
	}, "team-a"); rec.Code != http.StatusAccepted {
		t.Fatalf("seed team-a: status = %d", rec.Code)
	}

	rec := searchTraces(t, h, dayWindow)
	if d := decodeTraceList(t, rec); len(d.Traces) != 0 {
		t.Fatalf("default tenant saw %d traces, want 0", len(d.Traces))
	}
	rec = searchTraces(t, h, dayWindow, "team-a")
	if d := decodeTraceList(t, rec); len(d.Traces) != 1 || d.Traces[0].TraceID != sampleTraceID {
		t.Fatalf("team-a = %+v, want its own trace", d.Traces)
	}
}
