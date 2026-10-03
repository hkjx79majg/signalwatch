package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func postLogs(t *testing.T, h http.Handler, tenant, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	if contentType == "" {
		contentType = "application/json"
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, logsPath, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if tenant != "" {
		req.Header.Set(tenantHeader, tenant)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func getLogs(t *testing.T, h http.Handler, tenant, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if tenant != "" {
		req.Header.Set(tenantHeader, tenant)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func validLogEntry(id, ts, level, msg string) string {
	return fmt.Sprintf(`{"id":%q,"timestamp":%q,"level":%q,"message":%q,"labels":{"k":"v"}}`, id, ts, level, msg)
}

func TestPostLogsAcceptedAndReplayed(t *testing.T) {
	h := Handler()
	body := `{"entries":[
		{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"hello","labels":{}},
		{"id":"b","timestamp":"2026-01-01T00:00:01+08:00","level":"error","message":"boom","labels":{"svc":"api"},"trace_id":"0123456789abcdef0123456789abcdef"}
	]}`
	rec := postLogs(t, h, "", body, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)
	if payload["accepted"] != float64(2) || payload["replayed"] != float64(0) {
		t.Fatalf("payload = %v", payload)
	}

	// Identical resubmission (different timezone encoding, same instant) replays.
	replay := `{"entries":[
		{"id":"a","timestamp":"2026-01-01T08:00:00+08:00","level":"info","message":"hello","labels":{}}
	]}`
	rec = postLogs(t, h, "", replay, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	payload = decodeBody(t, rec)
	if payload["accepted"] != float64(0) || payload["replayed"] != float64(1) {
		t.Fatalf("payload = %v", payload)
	}
}

func TestPostLogsBatchInternalDuplicates(t *testing.T) {
	h := Handler()
	base := `{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{}}`

	// Identical duplicate within the batch counts as replay.
	rec := postLogs(t, h, "", `{"entries":[`+base+`,`+base+`]}`, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)
	if payload["accepted"] != float64(1) || payload["replayed"] != float64(1) {
		t.Fatalf("payload = %v", payload)
	}

	// Same id twice with different content conflicts and changes nothing.
	other := `{"id":"a","timestamp":"2026-01-01T00:00:01Z","level":"info","message":"m","labels":{}}`
	rec = postLogs(t, h, "", `{"entries":[`+other+`,`+other+`]}`, "")
	expectErrorCode(t, rec, http.StatusConflict, "log_conflict")

	// Stored record is still the original one.
	rec = getLogs(t, h, "", logsPath+"?q=m")
	entries := decodeBody(t, rec)["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["id"] != "a" {
		t.Fatalf("entries = %v", entries)
	}
}

func TestPostLogsConflicts(t *testing.T) {
	h := Handler()
	first := postLogs(t, h, "", `{"entries":[`+validLogEntry("a", "2026-01-01T00:00:00Z", "info", "m")+`]}`, "")
	if first.Code != http.StatusAccepted {
		t.Fatalf("status = %d", first.Code)
	}

	cases := []string{
		// different timestamp
		validLogEntry("a", "2026-01-01T00:00:01Z", "info", "m"),
		// different level
		`{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"warn","message":"m","labels":{"k":"v"}}`,
		// different message
		validLogEntry("a", "2026-01-01T00:00:00Z", "info", "other"),
		// different labels
		`{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{"k":"w"}}`,
		// trace_id newly added
		`{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{"k":"v"},"trace_id":"0123456789abcdef0123456789abcdef"}`,
	}
	for i, e := range cases {
		rec := postLogs(t, h, "", `{"entries":[`+e+`]}`, "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("case %d: status = %d body=%q", i, rec.Code, rec.Body.String())
		}
	}
}

func TestPostLogsInvalidBodies(t *testing.T) {
	h := Handler()
	good := `{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{}}`
	cases := []string{
		`null`,
		`{}`,
		`{"entries":null}`,
		`{"entries":[]}`,
		`{"entries":[` + good + `],"extra":1}`,
		`{"entries":[{"id":"","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{}}]}`,
		`{"entries":[{"id":"` + strings.Repeat("x", 129) + `","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{}}]}`,
		`{"entries":[{"id":"a\tb","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{}}]}`,
		`{"entries":[{"id":"a","timestamp":"2026-01-01T00:00:00","level":"info","message":"m","labels":{}}]}`,
		`{"entries":[{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"trace","message":"m","labels":{}}]}`,
		`{"entries":[{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"INFO","message":"m","labels":{}}]}`,
		`{"entries":[{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":null}]}`,
		`{"entries":[{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{"1bad":"v"}}]}`,
		`{"entries":[{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{}},"weird":1}]}`,
		`{"entries":[{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{},"trace_id":"ABCDEF0123456789ABCDEF0123456789"}]}`,
		`{"entries":[{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{},"trace_id":"0123"}]}`,
		`{"entries":[{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"info","message":1,"labels":{}}]}`,
		`{"entries":["not-an-object"]}`,
		`{"entries":[` + good + `]}{"entries":[` + good + `]}`,
	}
	for i, body := range cases {
		rec := postLogs(t, h, "fresh_"+fmt.Sprint(i), body, "")
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_logs")
	}
}

func TestPostLogsBatchSizeCap(t *testing.T) {
	h := Handler()
	mk := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"entries":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			id := fmt.Sprintf("e%04d", i)
			b.WriteString(validLogEntry(id, "2026-01-01T00:00:00Z", "info", "m"))
		}
		b.WriteString(`]}`)
		return b.String()
	}
	if rec := postLogs(t, h, "", mk(500), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("500 entries: status = %d body=%q", rec.Code, rec.Body.String())
	}
	rec := postLogs(t, h, "other", mk(501), "")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_logs")
}

func TestPostLogsMediaTypeAndMethod(t *testing.T) {
	h := Handler()
	body := `{"entries":[` + validLogEntry("a", "2026-01-01T00:00:00Z", "info", "m") + `]}`
	rec := postLogs(t, h, "", body, "text/plain")
	expectErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, logsPath, nil)
	h.ServeHTTP(rec, req)
	expectErrorCode(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if got := rec.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("Allow = %q", got)
	}
}

func TestGetLogsOrderingAndUTCNormalization(t *testing.T) {
	h := Handler()
	body := `{"entries":[
		{"id":"c","timestamp":"2026-01-01T00:00:00.5Z","level":"info","message":"m","labels":{}},
		{"id":"a","timestamp":"2026-01-01T00:00:00.500000000Z","level":"info","message":"m","labels":{}},
		{"id":"b","timestamp":"2026-01-01T08:00:01+08:00","level":"info","message":"m","labels":{}}
	]}`
	if rec := postLogs(t, h, "", body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	rec := getLogs(t, h, "", logsPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	entries := decodeBody(t, rec)["entries"].([]any)
	gotIDs := []string{}
	for _, e := range entries {
		em := e.(map[string]any)
		gotIDs = append(gotIDs, em["id"].(string))
		if ts := em["timestamp"].(string); !strings.HasSuffix(ts, "Z") {
			t.Fatalf("timestamp not UTC: %q", ts)
		}
	}
	want := []string{"b", "a", "c"} // newest instant first; ties by id ascending
	if fmt.Sprint(gotIDs) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", gotIDs, want)
	}
	// a and c are the same instant: a must precede c.
	aTS := entries[1].(map[string]any)["timestamp"].(string)
	cTS := entries[2].(map[string]any)["timestamp"].(string)
	if aTS != cTS {
		t.Fatalf("tie timestamps differ: %q %q", aTS, cTS)
	}
}

func TestGetLogsFilters(t *testing.T) {
	h := Handler()
	body := `{"entries":[
		{"id":"d1","timestamp":"2026-01-02T00:00:00Z","level":"debug","message":"startup sequence","labels":{"svc":"api","zone":"z1"}},
		{"id":"i1","timestamp":"2026-01-03T00:00:00Z","level":"info","message":"Request handled","labels":{"svc":"api"},"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"id":"e1","timestamp":"2026-01-04T00:00:00Z","level":"error","message":"request FAILED","labels":{"svc":"web"},"trace_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	]}`
	if rec := postLogs(t, h, "tf", body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post: %d", rec.Code)
	}

	ids := func(target string) []string {
		rec := getLogs(t, h, "tf", logsPath+target)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body.String())
		}
		var out []string
		for _, e := range decodeBody(t, rec)["entries"].([]any) {
			out = append(out, e.(map[string]any)["id"].(string))
		}
		return out
	}

	if got := ids("?level=error"); fmt.Sprint(got) != "[e1]" {
		t.Fatalf("level filter = %v", got)
	}
	if got := ids("?trace_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); fmt.Sprint(got) != "[i1]" {
		t.Fatalf("trace filter = %v", got)
	}
	if got := ids("?q=request"); fmt.Sprint(got) != "[e1]" {
		t.Fatalf("q should be case-sensitive, got %v", got)
	}
	if got := ids("?q=sequence"); fmt.Sprint(got) != "[d1]" {
		t.Fatalf("q substring = %v", got)
	}
	if got := ids("?label.svc=api"); fmt.Sprint(got) != "[i1 d1]" {
		t.Fatalf("label filter = %v", got)
	}
	if got := ids("?label.svc=api&label.zone=z1"); fmt.Sprint(got) != "[d1]" {
		t.Fatalf("combined labels = %v", got)
	}
	if got := ids("?start=2026-01-03T00:00:00Z&end=2026-01-04T00:00:00Z"); fmt.Sprint(got) != "[i1]" {
		t.Fatalf("start/end half-open = %v", got)
	}
	if got := ids("?level=error&q=FAILED&trace_id=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb&label.svc=web"); fmt.Sprint(got) != "[e1]" {
		t.Fatalf("combined filters = %v", got)
	}
	if got := ids("?level=warn"); len(got) != 0 {
		t.Fatalf("empty result = %v", got)
	}
}

func TestGetLogsEmptyShape(t *testing.T) {
	h := Handler()
	rec := getLogs(t, h, "empt", logsPath)
	payload := decodeBody(t, rec)
	if entries, ok := payload["entries"].([]any); !ok || len(entries) != 0 {
		t.Fatalf("entries = %v", payload["entries"])
	}
	if payload["next_cursor"] != nil {
		t.Fatalf("next_cursor = %v, want null", payload["next_cursor"])
	}
}

func TestGetLogsInvalidQueries(t *testing.T) {
	h := Handler()
	bad := []string{
		"?limit=0",
		"?limit=201",
		"?limit=abc",
		"?limit=1.5",
		"?level=panic",
		"?trace_id=zz",
		"?start=not-a-time",
		"?end=2026-13-01T00:00:00Z",
		"?unknown=1",
		"?label.1bad=v",
		"?level=info&level=warn",
		"?label.svc=a&label.svc=b",
	}
	for _, q := range bad {
		rec := getLogs(t, h, "", logsPath+q)
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_log_query")
	}
}

func TestGetLogsPaginationPinsSnapshot(t *testing.T) {
	h := Handler()
	mk := func(n int, off int, tenant string) {
		var b strings.Builder
		b.WriteString(`{"entries":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			idx := off + i
			fmt.Fprintf(&b, `{"id":"p%03d","timestamp":"2026-01-01T00:00:%02dZ","level":"info","message":"m","labels":{}}`, idx, idx%60)
		}
		b.WriteString(`]}`)
		rec := postLogs(t, h, tenant, b.String(), "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
		}
	}
	mk(5, 0, "page")

	rec := getLogs(t, h, "page", logsPath+"?limit=2")
	payload := decodeBody(t, rec)
	first := payload["entries"].([]any)
	if len(first) != 2 {
		t.Fatalf("first page len = %d", len(first))
	}
	cursor := payload["next_cursor"].(string)
	wantOrder := []string{"p004", "p003", "p002", "p001", "p000"}

	// New writes arrive after the first page; they must not enter the snapshot.
	mk(3, 10, "page")

	var seen []string
	seen = append(seen, first[0].(map[string]any)["id"].(string), first[1].(map[string]any)["id"].(string))
	for cursor != "" {
		rec = getLogs(t, h, "page", logsPath+"?cursor="+cursor)
		if rec.Code != http.StatusOK {
			t.Fatalf("page: %d %s", rec.Code, rec.Body.String())
		}
		payload = decodeBody(t, rec)
		for _, e := range payload["entries"].([]any) {
			seen = append(seen, e.(map[string]any)["id"].(string))
		}
		if payload["next_cursor"] == nil {
			break
		}
		cursor = payload["next_cursor"].(string)
	}
	if fmt.Sprint(seen) != fmt.Sprint(wantOrder) {
		t.Fatalf("paged order = %v, want %v", seen, wantOrder)
	}

	// A fresh request sees all 8 rows.
	rec = getLogs(t, h, "page", logsPath+"?limit=200")
	if got := len(decodeBody(t, rec)["entries"].([]any)); got != 8 {
		t.Fatalf("fresh query rows = %d, want 8", got)
	}

	// Cursor cannot be combined with other parameters: the cursor itself is
	// valid, so this is an invalid query rather than an invalid cursor.
	rec = getLogs(t, h, "page", logsPath+"?cursor="+cursor+"&level=info")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_log_query")
}

func TestGetLogsInvalidCursor(t *testing.T) {
	h := Handler()
	// Seed and obtain a valid cursor.
	postLogs(t, h, "ca", `{"entries":[
		`+validLogEntry("a", "2026-01-01T00:00:00Z", "info", "m")+`,
		`+validLogEntry("b", "2026-01-01T00:00:01Z", "info", "m")+`
	]}`, "")
	cursor := decodeBody(t, getLogs(t, h, "ca", logsPath+"?limit=1"))["next_cursor"]
	if cursor == nil {
		t.Fatal("expected a cursor")
	}
	token := cursor.(string)

	for _, bad := range []string{
		"garbage",
		strings.Replace(token, ".", "x", 1), // tampered signature separator
		token[:len(token)-2] + "aa",         // flipped signature chars
	} {
		rec := getLogs(t, h, "ca", logsPath+"?cursor="+bad)
		expectErrorCode(t, rec, http.StatusBadRequest, "invalid_cursor")
	}
	// Cursor from another tenant is rejected.
	rec := getLogs(t, h, "cb", logsPath+"?cursor="+token)
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_cursor")
}

func TestLogsTenantIsolation(t *testing.T) {
	h := Handler()
	body := `{"entries":[` + validLogEntry("a", "2026-01-01T00:00:00Z", "info", "tenant-one") + `]}`
	if rec := postLogs(t, h, "t1", body, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post: %d", rec.Code)
	}
	if rec := getLogs(t, h, "t2", logsPath); len(decodeBody(t, rec)["entries"].([]any)) != 0 {
		t.Fatal("tenant t2 sees t1 logs")
	}
	// Same id may exist independently in another tenant with different content.
	other := `{"entries":[` + validLogEntry("a", "2026-02-01T00:00:00Z", "error", "tenant-two") + `]}`
	if rec := postLogs(t, h, "t2", other, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("post t2: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, getLogs(t, h, "t1", logsPath))["entries"].([]any); len(got) != 1 {
		t.Fatalf("t1 rows = %v", got)
	}
}

func TestLogsInvalidTenantPriority(t *testing.T) {
	h := Handler()
	rec := postLogs(t, h, "Bad Tenant", `{"entries":[]}`, "text/plain")
	expectErrorCode(t, rec, http.StatusBadRequest, "invalid_tenant")
}

func TestLogsRetention(t *testing.T) {
	store := newMetricStore()
	batch := make([]*logEntry, 0, logsPerBatchMax)
	ts, err := time.Parse(time.RFC3339Nano, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	commit := func() {
		accepted, _, ok := store.appendLogs(batch)
		if !ok || accepted != len(batch) {
			t.Fatalf("commit accepted=%d ok=%v", accepted, ok)
		}
		batch = batch[:0]
	}
	for i := 0; i < logsPerTenant; i++ {
		batch = append(batch, &logEntry{
			id: fmt.Sprintf("id%05d", i), ts: ts, level: "info", message: "m", labels: map[string]string{},
		})
		if len(batch) == logsPerBatchMax {
			commit()
		}
	}
	commit()
	if len(store.logs) != logsPerTenant || store.logRing.len() != logsPerTenant {
		t.Fatalf("size = %d/%d", len(store.logs), store.logRing.len())
	}

	// One new id evicts the oldest; the evicted id can be written again.
	next := []*logEntry{{id: "fresh001", ts: ts, level: "info", message: "m", labels: map[string]string{}}}
	if _, _, ok := store.appendLogs(next); !ok {
		t.Fatal("append fresh failed")
	}
	if _, exists := store.logs["id00000"]; exists {
		t.Fatal("oldest id was not evicted")
	}
	if _, exists := store.logs["id00001"]; !exists {
		t.Fatal("second-oldest should remain")
	}
	// Evicted id is writable again even with different content.
	rewrite := []*logEntry{{id: "id00000", ts: ts, level: "error", message: "changed", labels: map[string]string{}}}
	accepted, _, ok := store.appendLogs(rewrite)
	if !ok || accepted != 1 {
		t.Fatalf("rewrite evicted id: accepted=%d ok=%v", accepted, ok)
	}
	if store.logs["id00000"].level != "error" {
		t.Fatal("evicted id rewrite not stored")
	}
}

func TestLogsReplayDoesNotRefreshOrder(t *testing.T) {
	store := newMetricStore()
	ts, err := time.Parse(time.RFC3339Nano, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id string) *logEntry {
		return &logEntry{id: id, ts: ts, level: "info", message: "m", labels: map[string]string{}}
	}
	store.appendLogs([]*logEntry{mk("a"), mk("b"), mk("c")})
	// Replay "a"; it must stay the oldest slot.
	if _, replayed, ok := store.appendLogs([]*logEntry{mk("a")}); !ok || replayed != 1 {
		t.Fatal("replay failed")
	}
	// Add enough fresh ids to evict exactly one entry: it must be "a".
	for i := 0; i < logsPerTenant-3; i++ {
		store.appendLogs([]*logEntry{mk(fmt.Sprintf("n%05d", i))})
	}
	if _, exists := store.logs["a"]; !exists {
		t.Fatal("a evicted too early")
	}
	store.appendLogs([]*logEntry{mk("last01")})
	if _, exists := store.logs["a"]; exists {
		t.Fatal("replay refreshed retention order: a survived")
	}
	if _, exists := store.logs["b"]; !exists {
		t.Fatal("b should still be retained")
	}
}

func TestLogsConcurrentBatchesAreAtomic(t *testing.T) {
	h := Handler()
	var wg sync.WaitGroup
	// Two tenants, repeated concurrent batches against the same ids with
	// conflicting content: every individual response must be either 202 or
	// 409, state must never be partially visible, and tenants never mix.
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"entries":[{"id":"k","timestamp":"2026-01-01T00:00:%02dZ","level":"info","message":"m","labels":{}}]}`, i)
			postLogs(t, h, "cc1", body, "")
		}(i)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"entries":[{"id":"k","timestamp":"2026-02-01T00:00:%02dZ","level":"error","message":"m","labels":{}}]}`, i)
			postLogs(t, h, "cc2", body, "")
		}(i)
	}
	wg.Wait()
	r1 := decodeBody(t, getLogs(t, h, "cc1", logsPath))["entries"].([]any)
	r2 := decodeBody(t, getLogs(t, h, "cc2", logsPath))["entries"].([]any)
	if len(r1) != 1 || len(r2) != 1 {
		t.Fatalf("partial commits visible: %d %d", len(r1), len(r2))
	}
	if r1[0].(map[string]any)["level"] != "info" || r2[0].(map[string]any)["level"] != "error" {
		t.Fatalf("tenant data leaked: %v %v", r1, r2)
	}
}

func TestPostLogsTraceIDEcho(t *testing.T) {
	h := Handler()
	with := `{"entries":[{"id":"a","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{},"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`
	without := `{"entries":[{"id":"b","timestamp":"2026-01-01T00:00:00Z","level":"info","message":"m","labels":{}}]}`
	postLogs(t, h, "", with, "")
	postLogs(t, h, "", without, "")
	rec := getLogs(t, h, "", logsPath+"?limit=10")
	entries := decodeBody(t, rec)["entries"].([]any)
	byID := map[string]map[string]any{}
	for _, e := range entries {
		em := e.(map[string]any)
		byID[em["id"].(string)] = em
	}
	if byID["a"]["trace_id"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("trace_id not echoed: %v", byID["a"])
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(raw["entries"], &list); err != nil {
		t.Fatal(err)
	}
	for _, e := range list {
		if string(e["id"]) == `"b"` {
			if _, present := e["trace_id"]; present {
				t.Fatal("trace_id should be omitted when absent on write")
			}
		}
	}
}
