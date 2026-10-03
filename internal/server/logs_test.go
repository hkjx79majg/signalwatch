package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ---- helpers ---------------------------------------------------------------

func makeLogEntry(id, ts, level, message string) string {
	return fmt.Sprintf(`{"id":%q,"timestamp":%q,"level":%q,"message":%q,"labels":{"app":"demo"}}`,
		id, ts, level, message)
}

func postLogs(t *testing.T, h http.Handler, body string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodPost, logsPath, body, "application/json", tenants...)
}

func postLogEntries(t *testing.T, h http.Handler, entries []string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return postLogs(t, h, `{"entries":[`+strings.Join(entries, ",")+`]}`, tenants...)
}

func getLogs(t *testing.T, h http.Handler, query string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	target := logsPath
	if query != "" {
		target += "?" + query
	}
	return doRequest(t, h, http.MethodGet, target, "", "", tenants...)
}

type logsPage struct {
	Entries []struct {
		ID        string            `json:"id"`
		Timestamp string            `json:"timestamp"`
		Level     string            `json:"level"`
		Message   string            `json:"message"`
		Labels    map[string]string `json:"labels"`
		TraceID   string            `json:"trace_id"`
	} `json:"entries"`
	NextCursor *string `json:"next_cursor"`
}

func decodeLogPage(t *testing.T, rec *httptest.ResponseRecorder) logsPage {
	t.Helper()
	var page logsPage
	if err := decodeJSON(rec, &page); err != nil {
		t.Fatalf("decode logs page: %v", err)
	}
	return page
}

func pageIDs(page logsPage) []string {
	ids := make([]string, 0, len(page.Entries))
	for _, e := range page.Entries {
		ids = append(ids, e.ID)
	}
	return ids
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := decodeJSON(rec, &payload); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return payload.Error.Code
}

// ---- POST /api/v1/logs -----------------------------------------------------

func TestLogsPostAccepted(t *testing.T) {
	h := Handler()
	rec := postLogEntries(t, h, []string{
		makeLogEntry("a", "2026-01-02T03:04:05Z", "info", "hello"),
		makeLogEntry("b", "2026-01-02T03:04:06.5+02:00", "error", "boom"),
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

func TestLogsPostUnsupportedMediaType(t *testing.T) {
	h := Handler()
	rec := doRequest(t, h, http.MethodPost, logsPath, `{"entries":[]}`, "text/plain")
	if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
		t.Fatalf("status = %d code = %q", rec.Code, errorCode(t, rec))
	}
	// A malformed media type is still a media type error.
	rec = doRequest(t, h, http.MethodPost, logsPath, `{"entries":[]}`, "application/json , garbage")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
}

func TestLogsPostInvalidBodies(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"not json":            `{`,
		"trailing data":       `{"entries":[]} garbage`,
		"empty entries":       `{"entries":[]}`,
		"missing entries":     `{}`,
		"null entries":        `{"entries":null}`,
		"unknown envelope":    `{"entries":[{"id":"a","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":{}}],"extra":1}`,
		"unknown entry field": `{"entries":[{"id":"a","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":{},"bogus":1}]}`,
		"missing id":          `{"entries":[{"timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":{}}]}`,
		"empty id":            `{"entries":[{"id":"","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":{}}]}`,
		"long id":             `{"entries":[{"id":"` + strings.Repeat("x", 129) + `","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":{}}]}`,
		"non printable id":    `{"entries":[{"id":"a	b","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":{}}]}`,
		"missing timestamp":   `{"entries":[{"id":"a","level":"info","message":"m","labels":{}}]}`,
		"timestamp no zone":   `{"entries":[{"id":"a","timestamp":"2026-01-02T03:04:05","level":"info","message":"m","labels":{}}]}`,
		"timestamp bad":       `{"entries":[{"id":"a","timestamp":"2026-13-02T03:04:05Z","level":"info","message":"m","labels":{}}]}`,
		"bad level":           `{"entries":[{"id":"a","timestamp":"2026-01-02T03:04:05Z","level":"INFO","message":"m","labels":{}}]}`,
		"missing message":     `{"entries":[{"id":"a","timestamp":"2026-01-02T03:04:05Z","level":"info","labels":{}}]}`,
		"missing labels":      `{"entries":[{"id":"a","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m"}]}`,
		"null labels":         `{"entries":[{"id":"a","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":null}]}`,
		"bad label key":       `{"entries":[{"id":"a","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":{"1bad":"x"}}]}`,
		"non string label":    `{"entries":[{"id":"a","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":{"app":3}}]}`,
		"trace_id short":      `{"entries":[{"id":"a","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":{},"trace_id":"abc"}]}`,
		"trace_id uppercase":  `{"entries":[{"id":"a","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":{},"trace_id":"` + strings.Repeat("A", 32) + `"}]}`,
	}
	for name, body := range cases {
		rec := postLogs(t, h, body)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_logs" {
			t.Errorf("%s: status = %d code = %q, want 400 invalid_logs", name, rec.Code, errorCode(t, rec))
		}
	}

	// Batches over 500 entries are rejected.
	entries := make([]string, 501)
	for i := range entries {
		entries[i] = makeLogEntry(strconv.Itoa(i), "2026-01-02T03:04:05Z", "info", "m")
	}
	rec := postLogEntries(t, h, entries)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_logs" {
		t.Fatalf("oversized batch: status = %d code = %q", rec.Code, errorCode(t, rec))
	}

	// Exactly 500 entries are accepted.
	rec = postLogEntries(t, h, entries[:500])
	if rec.Code != http.StatusAccepted {
		t.Fatalf("500-entry batch: status = %d, want 202", rec.Code)
	}
}

func TestLogsReplayAndConflict(t *testing.T) {
	h := Handler()
	entry := makeLogEntry("a", "2026-01-02T03:04:05Z", "info", "hello")
	if rec := postLogEntries(t, h, []string{entry}); rec.Code != http.StatusAccepted {
		t.Fatalf("first write: status = %d", rec.Code)
	}

	// Same id, same instant expressed in another offset, same content: replay.
	replay := makeLogEntry("a", "2026-01-02T05:04:05+02:00", "info", "hello")
	rec := postLogEntries(t, h, []string{replay})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("replay: status = %d, want 202", rec.Code)
	}
	var payload map[string]int
	_ = decodeJSON(rec, &payload)
	if payload["accepted"] != 0 || payload["replayed"] != 1 {
		t.Fatalf("replay payload = %v, want accepted=0 replayed=1", payload)
	}

	// Same id, different content: whole batch conflicts, nothing commits.
	conflict := makeLogEntry("a", "2026-01-02T03:04:05Z", "info", "changed")
	fresh := makeLogEntry("fresh", "2026-01-02T03:04:05Z", "info", "new")
	rec = postLogEntries(t, h, []string{fresh, conflict})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "log_conflict" {
		t.Fatalf("conflict: status = %d code = %q", rec.Code, errorCode(t, rec))
	}
	page := decodeLogPage(t, getLogs(t, h, "q=new"))
	if len(page.Entries) != 0 {
		t.Fatalf("conflicting batch partially committed: %v", pageIDs(page))
	}

	// A different instant with the same id is also a conflict.
	otherInstant := makeLogEntry("a", "2026-01-02T03:04:06Z", "info", "hello")
	if rec := postLogEntries(t, h, []string{otherInstant}); rec.Code != http.StatusConflict {
		t.Fatalf("different instant: status = %d, want 409", rec.Code)
	}
}

func TestLogsBatchDuplicateIDs(t *testing.T) {
	h := Handler()

	// Identical duplicates inside one batch: one accept, one replay.
	dup := makeLogEntry("a", "2026-01-02T03:04:05Z", "info", "m")
	rec := postLogEntries(t, h, []string{dup, dup})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	var payload map[string]int
	_ = decodeJSON(rec, &payload)
	if payload["accepted"] != 1 || payload["replayed"] != 1 {
		t.Fatalf("payload = %v, want accepted=1 replayed=1", payload)
	}

	// Diverging duplicates inside one batch: conflict, nothing commits.
	a := makeLogEntry("b", "2026-01-02T03:04:05Z", "info", "one")
	b := makeLogEntry("b", "2026-01-02T03:04:05Z", "warn", "two")
	rec = postLogEntries(t, h, []string{a, b})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "log_conflict" {
		t.Fatalf("status = %d code = %q", rec.Code, errorCode(t, rec))
	}
	page := decodeLogPage(t, getLogs(t, h, "q=one"))
	if len(page.Entries) != 0 {
		t.Fatalf("diverging duplicate committed: %v", pageIDs(page))
	}
}

func TestLogsEviction(t *testing.T) {
	h := Handler()

	// Fill the tenant past the 10000-id retention bound in batches of 500.
	post := func(start, count int) {
		t.Helper()
		entries := make([]string, count)
		for i := range entries {
			n := start + i
			entries[i] = makeLogEntry(fmt.Sprintf("id-%05d", n), "2026-01-02T03:04:05Z", "info", fmt.Sprintf("msg-%d", n))
		}
		if rec := postLogEntries(t, h, entries); rec.Code != http.StatusAccepted {
			t.Fatalf("batch at %d: status = %d", start, rec.Code)
		}
	}
	for start := 0; start < 10000; start += 500 {
		post(start, 500)
	}

	// Replaying the oldest id must not refresh its eviction position.
	oldest := makeLogEntry("id-00000", "2026-01-02T03:04:05Z", "info", "msg-0")
	rec := postLogEntries(t, h, []string{oldest})
	var payload map[string]int
	_ = decodeJSON(rec, &payload)
	if rec.Code != http.StatusAccepted || payload["replayed"] != 1 {
		t.Fatalf("replay of oldest: status = %d payload = %v", rec.Code, payload)
	}

	// One more id pushes the oldest out despite the replay.
	post(10000, 1)

	page := decodeLogPage(t, getLogs(t, h, "q=msg-0"))
	if len(page.Entries) != 0 {
		t.Fatalf("oldest id not evicted: %v", pageIDs(page))
	}
	page = decodeLogPage(t, getLogs(t, h, "q=msg-10000"))
	if len(page.Entries) != 1 {
		t.Fatalf("newest id missing: %v", pageIDs(page))
	}

	// An evicted id can be written again as a fresh entry.
	rec = postLogEntries(t, h, []string{oldest})
	_ = decodeJSON(rec, &payload)
	if rec.Code != http.StatusAccepted || payload["accepted"] != 1 {
		t.Fatalf("rewrite of evicted id: status = %d payload = %v", rec.Code, payload)
	}
}

// ---- GET /api/v1/logs ------------------------------------------------------

func seedLogEntries(t *testing.T, h http.Handler) {
	t.Helper()
	entries := []string{
		`{"id":"e1","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"Alpha started","labels":{"app":"web","env":"prod"},"trace_id":"` + strings.Repeat("1", 32) + `"}`,
		`{"id":"e2","timestamp":"2026-01-02T03:04:06Z","level":"error","message":"alpha failed","labels":{"app":"web","env":"dev"}}`,
		`{"id":"e3","timestamp":"2026-01-02T03:04:07Z","level":"warn","message":"Beta slow","labels":{"app":"worker"},"trace_id":"` + strings.Repeat("2", 32) + `"}`,
		`{"id":"e4","timestamp":"2026-01-02T03:04:07Z","level":"debug","message":"alpha trace","labels":{"app":"web"}}`,
	}
	if rec := postLogEntries(t, h, entries); rec.Code != http.StatusAccepted {
		t.Fatalf("seed: status = %d", rec.Code)
	}
}

func TestLogsGetOrderingAndUTC(t *testing.T) {
	h := Handler()
	seedLogEntries(t, h)

	page := decodeLogPage(t, getLogs(t, h, ""))
	got := pageIDs(page)
	want := []string{"e3", "e4", "e2", "e1"} // timestamp desc, id asc within one instant
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if page.NextCursor != nil {
		t.Fatalf("next_cursor = %v, want null", *page.NextCursor)
	}
	for _, e := range page.Entries {
		if !strings.HasSuffix(e.Timestamp, "Z") {
			t.Fatalf("timestamp %q not normalized to UTC", e.Timestamp)
		}
	}

	// Offsets are normalized to UTC on output.
	rec := postLogEntries(t, h, []string{
		makeLogEntry("off", "2026-01-02T05:04:05.123456789+02:00", "info", "offset"),
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("offset write: status = %d", rec.Code)
	}
	page = decodeLogPage(t, getLogs(t, h, "q=offset"))
	if len(page.Entries) != 1 || page.Entries[0].Timestamp != "2026-01-02T03:04:05.123456789Z" {
		t.Fatalf("offset entry = %+v", page.Entries)
	}
}

func TestLogsGetFilters(t *testing.T) {
	h := Handler()
	seedLogEntries(t, h)

	cases := []struct {
		query string
		want  []string
	}{
		{"level=error", []string{"e2"}},
		{"trace_id=" + strings.Repeat("1", 32), []string{"e1"}},
		{"q=alpha", []string{"e4", "e2"}}, // case-sensitive: "Alpha" in e1 does not match
		{"q=Alpha", []string{"e1"}},       //
		{"label.app=web", []string{"e4", "e2", "e1"}},
		{"label.app=web&label.env=prod", []string{"e1"}},
		{"start=2026-01-02T03:04:06Z&end=2026-01-02T03:04:07Z", []string{"e2"}}, // start <= ts < end
		{"end=2026-01-02T03:04:06Z", []string{"e1"}},
		{"start=2026-01-02T03:04:07Z", []string{"e3", "e4"}},
		{"level=warn&label.app=worker&q=Beta", []string{"e3"}},
		{"q=absent", []string{}},
	}
	for _, tc := range cases {
		page := decodeLogPage(t, getLogs(t, h, tc.query))
		if fmt.Sprint(pageIDs(page)) != fmt.Sprint(tc.want) {
			t.Errorf("%s: ids = %v, want %v", tc.query, pageIDs(page), tc.want)
		}
	}

	// Empty results are an empty array with a null cursor.
	page := decodeLogPage(t, getLogs(t, h, "q=absent"))
	if page.Entries == nil || len(page.Entries) != 0 || page.NextCursor != nil {
		t.Fatalf("empty page = %+v", page)
	}
	if !strings.Contains(getLogs(t, h, "q=absent").Body.String(), `"entries":[]`) {
		t.Fatalf("empty result must encode entries as []")
	}
}

func TestLogsGetInvalidQuery(t *testing.T) {
	h := Handler()
	seedLogEntries(t, h)
	cases := []string{
		"limit=0",
		"limit=201",
		"limit=abc",
		"limit=1.5",
		"level=fatal",
		"trace_id=xyz",
		"start=not-a-time",
		"end=2026-01-02T03:04:05", // no timezone
		"label.1bad=x",
		"unknown=1",
		"level=info&level=warn", // conflicting repeats
	}
	for _, q := range cases {
		rec := getLogs(t, h, q)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_log_query" {
			t.Errorf("%s: status = %d code = %q, want 400 invalid_log_query", q, rec.Code, errorCode(t, rec))
		}
	}
}

func TestLogsPagination(t *testing.T) {
	h := Handler()
	entries := make([]string, 5)
	for i := 0; i < 5; i++ {
		entries[i] = makeLogEntry(fmt.Sprintf("p%d", i), fmt.Sprintf("2026-01-02T03:04:0%dZ", i), "info", "page")
	}
	if rec := postLogEntries(t, h, entries); rec.Code != http.StatusAccepted {
		t.Fatalf("seed: status = %d", rec.Code)
	}

	seen := map[string]int{}
	cursor := ""
	for pageNum := 0; ; pageNum++ {
		var rec *httptest.ResponseRecorder
		if cursor == "" {
			rec = getLogs(t, h, "limit=2")
		} else {
			rec = getLogs(t, h, "cursor="+cursor)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d", pageNum, rec.Code)
		}
		page := decodeLogPage(t, rec)
		for _, id := range pageIDs(page) {
			seen[id]++
		}
		if page.NextCursor == nil {
			break
		}
		cursor = (*page.NextCursor)
		if pageNum > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d distinct ids, want 5", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("id %s seen %d times", id, n)
		}
	}
}

func TestLogsCursorRules(t *testing.T) {
	h := Handler()
	entries := make([]string, 4)
	for i := 0; i < 4; i++ {
		entries[i] = makeLogEntry(fmt.Sprintf("c%d", i), fmt.Sprintf("2026-01-02T03:04:0%dZ", i), "info", "cur")
	}
	if rec := postLogEntries(t, h, entries); rec.Code != http.StatusAccepted {
		t.Fatalf("seed: status = %d", rec.Code)
	}

	page := decodeLogPage(t, getLogs(t, h, "limit=2"))
	if page.NextCursor == nil {
		t.Fatal("expected a cursor")
	}
	cursor := *page.NextCursor

	// A follow-up may only carry the cursor.
	rec := getLogs(t, h, "cursor="+cursor+"&limit=2")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_log_query" {
		t.Fatalf("cursor+limit: status = %d code = %q", rec.Code, errorCode(t, rec))
	}

	// Garbage cursors are rejected.
	for _, bad := range []string{"not-a-cursor", "AAAA.BBBB", cursor + "x", cursor[:len(cursor)-2] + "zz"} {
		rec := getLogs(t, h, "cursor="+bad)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_cursor" {
			t.Errorf("cursor %q: status = %d code = %q, want 400 invalid_cursor", bad, rec.Code, errorCode(t, rec))
		}
	}

	// A cursor from another tenant is rejected.
	rec = getLogs(t, h, "cursor="+cursor, "other-tenant")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_cursor" {
		t.Fatalf("cross-tenant cursor: status = %d code = %q", rec.Code, errorCode(t, rec))
	}

	// New writes after the first page never mix into later pages.
	if rec := postLogEntries(t, h, []string{
		makeLogEntry("late", "2026-01-02T03:04:09Z", "info", "cur"),
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("late write: status = %d", rec.Code)
	}
	page2 := decodeLogPage(t, getLogs(t, h, "cursor="+cursor))
	for _, id := range pageIDs(page2) {
		if id == "late" {
			t.Fatalf("post-snapshot write leaked into page: %v", pageIDs(page2))
		}
	}
	if len(pageIDs(page2)) != 2 {
		t.Fatalf("second page = %v, want 2 entries", pageIDs(page2))
	}
}

func TestLogsMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := doRequest(t, h, method, logsPath, "", "")
		if rec.Code != http.StatusMethodNotAllowed || errorCode(t, rec) != "method_not_allowed" {
			t.Fatalf("%s: status = %d code = %q", method, rec.Code, errorCode(t, rec))
		}
		if rec.Header().Get("Allow") != "GET, POST" {
			t.Fatalf("%s: Allow = %q", method, rec.Header().Get("Allow"))
		}
	}
}

func TestLogsTenantIsolation(t *testing.T) {
	h := Handler()
	entry := makeLogEntry("shared-id", "2026-01-02T03:04:05Z", "info", "tenant a")
	if rec := postLogEntries(t, h, []string{entry}, "tenant-a"); rec.Code != http.StatusAccepted {
		t.Fatalf("write: status = %d", rec.Code)
	}

	// Other tenants and the default tenant see nothing.
	for _, tenants := range [][]string{{"tenant-b"}, {}} {
		page := decodeLogPage(t, getLogs(t, h, "", tenants...))
		if len(page.Entries) != 0 {
			t.Fatalf("tenants %v: saw %v", tenants, pageIDs(page))
		}
	}

	// The same id in another tenant is an independent entry, not a conflict.
	rec := postLogEntries(t, h, []string{
		makeLogEntry("shared-id", "2026-01-02T03:04:05Z", "error", "tenant b"),
	}, "tenant-b")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cross-tenant same id: status = %d, want 202", rec.Code)
	}

	// invalid_tenant still wins over any log-level error.
	rec = doRequest(t, h, http.MethodPost, logsPath, "not json", "text/plain", "bad tenant!")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_tenant" {
		t.Fatalf("tenant priority: status = %d code = %q", rec.Code, errorCode(t, rec))
	}
}

func TestLogsConcurrentAccess(t *testing.T) {
	h := Handler()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%d", g%2)
			for i := 0; i < 50; i++ {
				id := fmt.Sprintf("g%d-i%d", g, i)
				entry := makeLogEntry(id, "2026-01-02T03:04:05Z", "info", fmt.Sprintf("m-%d-%d", g, i))
				rec := postLogEntries(t, h, []string{entry}, tenant)
				if rec.Code != http.StatusAccepted {
					t.Errorf("write %s: status = %d", id, rec.Code)
					return
				}
				if rec := getLogs(t, h, "limit=5", tenant); rec.Code != http.StatusOK {
					t.Errorf("read: status = %d", rec.Code)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	// Every write landed exactly once per tenant.
	for _, tenant := range []string{"tenant-0", "tenant-1"} {
		page := decodeLogPage(t, getLogs(t, h, "limit=200", tenant))
		if len(page.Entries) != 200 {
			t.Fatalf("%s: %d entries, want 200", tenant, len(page.Entries))
		}
	}
}

// ---- regression: existing endpoints untouched ------------------------------

func TestLogsDoNotAffectMetrics(t *testing.T) {
	h := Handler()
	if rec := postLogEntries(t, h, []string{
		makeLogEntry("a", "2026-01-02T03:04:05Z", "info", "m"),
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("log write: status = %d", rec.Code)
	}
	rec := tenantJSON(t, h, http.MethodGet, "/api/v1/metrics?name=requests_total", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics get: status = %d", rec.Code)
	}
	if seriesCount(rec) != 0 {
		t.Fatalf("log write leaked into metrics")
	}
}
