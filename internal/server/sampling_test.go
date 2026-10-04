package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- policy endpoint helpers ----------------------------------------------

const samplingPath = samplingPolicyPath

// tenantsOrNone passes the tenant header only when a name is given, so an empty
// name selects the header-less default tenant.
func tenantsOrNone(tenant string) []string {
	if tenant == "" {
		return nil
	}
	return []string{tenant}
}

func putPolicy(t *testing.T, h http.Handler, body, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodPut, samplingPath, body, "application/json", tenantsOrNone(tenant)...)
}

func getPolicy(t *testing.T, h http.Handler, targetSuffix, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	target := samplingPath + targetSuffix
	return doRequest(t, h, http.MethodGet, target, "", "", tenantsOrNone(tenant)...)
}

func decodePolicyBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]float64 {
	t.Helper()
	var p struct {
		LogRate   float64 `json:"log_rate"`
		TraceRate float64 `json:"trace_rate"`
	}
	if err := decodeJSON(rec, &p); err != nil {
		t.Fatalf("decode policy: %v body=%q", err, rec.Body.String())
	}
	return map[string]float64{"log_rate": p.LogRate, "trace_rate": p.TraceRate}
}

// ---- GET/PUT policy --------------------------------------------------------

func TestSamplingPolicyDefault(t *testing.T) {
	h := Handler()
	for _, tenant := range []string{"", "fresh-tenant"} {
		rec := getPolicy(t, h, "", tenant)
		if rec.Code != http.StatusOK {
			t.Fatalf("tenant %q: status = %d body=%q", tenant, rec.Code, rec.Body.String())
		}
		p := decodePolicyBody(t, rec)
		if p["log_rate"] != 1 || p["trace_rate"] != 1 {
			t.Fatalf("tenant %q: policy = %v, want 1/1", tenant, p)
		}
		if rec.Body.String() != "{\"log_rate\":1,\"trace_rate\":1}\n" {
			t.Fatalf("default body = %q", rec.Body.String())
		}
	}
}

func TestSamplingPolicyPutAndGet(t *testing.T) {
	h := Handler()
	rec := putPolicy(t, h, `{"log_rate":0,"trace_rate":0.25}`, "team-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("put: status = %d body=%q", rec.Code, rec.Body.String())
	}
	if p := decodePolicyBody(t, rec); p["log_rate"] != 0 || p["trace_rate"] != 0.25 {
		t.Fatalf("put echo = %v", p)
	}
	if p := decodePolicyBody(t, getPolicy(t, h, "", "team-a")); p["log_rate"] != 0 || p["trace_rate"] != 0.25 {
		t.Fatalf("get after put = %v", p)
	}

	// Endpoint values 0 and 1 are both accepted.
	rec = putPolicy(t, h, `{"log_rate":1,"trace_rate":0}`, "team-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("boundary put: status = %d", rec.Code)
	}
}

func TestSamplingPolicyInvalidBodies(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"not json":       `{`,
		"trailing data":  `{"log_rate":1,"trace_rate":1} x`,
		"not object":     `[1,1]`,
		"null":           `null`,
		"missing log":    `{"trace_rate":1}`,
		"missing trace":  `{"log_rate":1}`,
		"missing both":   `{}`,
		"extra field":    `{"log_rate":1,"trace_rate":1,"other":1}`,
		"log wrong type": `{"log_rate":"1","trace_rate":1}`,
		"trace null":     `{"log_rate":1,"trace_rate":null}`,
		"log negative":   `{"log_rate":-0.0000001,"trace_rate":1}`,
		"trace above 1":  `{"log_rate":1,"trace_rate":1.0000001}`,
		"log huge":       `{"log_rate":1e3,"trace_rate":1}`,
		"log bool":       `{"log_rate":true,"trace_rate":1}`,
	}
	for name, body := range cases {
		rec := putPolicy(t, h, body, "team-a")
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_sampling_policy" {
			t.Errorf("%s: status = %d code = %q, want 400 invalid_sampling_policy", name, rec.Code, errorCode(t, rec))
		}
	}

	// A rejected replacement leaves the prior policy untouched.
	if rec := putPolicy(t, h, `{"log_rate":0.2,"trace_rate":0.3}`, "team-b"); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d", rec.Code)
	}
	if rec := putPolicy(t, h, `{"log_rate":2,"trace_rate":1}`, "team-b"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad replacement: %d", rec.Code)
	}
	if p := decodePolicyBody(t, getPolicy(t, h, "", "team-b")); p["log_rate"] != 0.2 || p["trace_rate"] != 0.3 {
		t.Fatalf("policy changed after failed put: %v", p)
	}
}

func TestSamplingPolicyUnsupportedMediaType(t *testing.T) {
	h := Handler()
	for _, ct := range []string{"text/plain", "application/json , garbage"} {
		rec := doRequest(t, h, http.MethodPut, samplingPath, `{"log_rate":0,"trace_rate":0}`, ct)
		if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
			t.Errorf("Content-Type %q: status = %d code = %q, want 415", ct, rec.Code, errorCode(t, rec))
		}
	}
	// A missing Content-Type is also a 415.
	rec := doRequest(t, h, http.MethodPut, samplingPath, `{"log_rate":0,"trace_rate":0}`, "")
	if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
		t.Fatalf("missing Content-Type: status = %d code = %q, want 415", rec.Code, errorCode(t, rec))
	}
}

func TestSamplingPolicyGetRejectsQuery(t *testing.T) {
	h := Handler()
	for _, suffix := range []string{"?x=1", "?x", "?log_rate=1"} {
		rec := getPolicy(t, h, suffix, "")
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_sampling_query" {
			t.Errorf("%s: status = %d code = %q, want 400 invalid_sampling_query", suffix, rec.Code, errorCode(t, rec))
		}
	}
}

func TestSamplingPolicyMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		rec := doRequest(t, h, method, samplingPath, "{}", "application/json")
		if rec.Code != http.StatusMethodNotAllowed || errorCode(t, rec) != "method_not_allowed" {
			t.Fatalf("%s: status = %d code = %q", method, rec.Code, errorCode(t, rec))
		}
		if rec.Header().Get("Allow") != "GET, PUT" {
			t.Fatalf("%s: Allow = %q, want GET, PUT", method, rec.Header().Get("Allow"))
		}
	}
}

func TestSamplingPolicyTenantIsolation(t *testing.T) {
	h := Handler()
	if rec := putPolicy(t, h, `{"log_rate":0,"trace_rate":0}`, "team-a"); rec.Code != http.StatusOK {
		t.Fatalf("seed team-a: %d", rec.Code)
	}
	// Other tenants keep the default.
	for _, tenant := range []string{"team-b", ""} {
		if p := decodePolicyBody(t, getPolicy(t, h, "", tenant)); p["log_rate"] != 1 || p["trace_rate"] != 1 {
			t.Fatalf("tenant %q leaked policy: %v", tenant, p)
		}
	}
	// An independent update in team-b never touches team-a.
	if rec := putPolicy(t, h, `{"log_rate":0.5,"trace_rate":0.5}`, "team-b"); rec.Code != http.StatusOK {
		t.Fatalf("seed team-b: %d", rec.Code)
	}
	if p := decodePolicyBody(t, getPolicy(t, h, "", "team-a")); p["log_rate"] != 0 || p["trace_rate"] != 0 {
		t.Fatalf("team-a changed: %v", p)
	}
}

func TestSamplingPolicyInvalidTenantPriority(t *testing.T) {
	h := Handler()
	// invalid_tenant beats query, media type and body validation.
	if rec := getPolicy(t, h, "?x=1", "bad tenant!"); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_tenant" {
		t.Fatalf("get query priority: %d %q", rec.Code, errorCode(t, rec))
	}
	if rec := doRequest(t, h, http.MethodPut, samplingPath, "{}", "text/plain", "bad tenant!"); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_tenant" {
		t.Fatalf("put media priority: %d %q", rec.Code, errorCode(t, rec))
	}
	if rec := doRequest(t, h, http.MethodPut, samplingPath, `{not json`, "application/json", "bad tenant!"); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_tenant" {
		t.Fatalf("put body priority: %d %q", rec.Code, errorCode(t, rec))
	}
}

// ---- ingestion response shape ---------------------------------------------

func ingestCounts(t *testing.T, rec *httptest.ResponseRecorder) map[string]int {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	var m map[string]int
	if err := decodeJSON(rec, &m); err != nil {
		t.Fatalf("decode ingest body: %v", err)
	}
	return m
}

func TestSamplingDefaultResponseShapeUnchanged(t *testing.T) {
	h := Handler()
	rec := postLogEntries(t, h, []string{
		makeLogEntry("a", "2026-01-02T03:04:05Z", "info", "m"),
		makeLogEntry("b", "2026-01-02T03:04:05Z", "info", "m"),
	})
	if rec.Body.String() != "{\"accepted\":2,\"replayed\":0}\n" {
		t.Fatalf("logs baseline body = %q", rec.Body.String())
	}
	rec = postSpanBatch(t, h, []string{rootSpan(`{}`)})
	if rec.Body.String() != "{\"accepted\":1,\"replayed\":0}\n" {
		t.Fatalf("spans baseline body = %q", rec.Body.String())
	}
}

// ---- deterministic log sampling --------------------------------------------

// classifyLogIDs returns candidate log ids partitioned by sampleIn under the
// given rate, so expectations are derived from the exact shipped predicate.
func classifyLogIDs(rate float64, n int) (kept, dropped []string) {
	for i := 0; len(kept) < n || len(dropped) < n; i++ {
		id := fmt.Sprintf("log-id-%04d", i)
		if sampleIn(id, rate) {
			if len(kept) < n {
				kept = append(kept, id)
			}
		} else if len(dropped) < n {
			dropped = append(dropped, id)
		}
	}
	return kept, dropped
}

func TestLogSamplingByIDWithoutTrace(t *testing.T) {
	h := Handler()
	if rec := putPolicy(t, h, `{"log_rate":0.5,"trace_rate":1}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("policy: %d", rec.Code)
	}
	kept, dropped := classifyLogIDs(0.5, 3)
	ids := append(append([]string{}, kept...), dropped...)
	entries := make([]string, len(ids))
	for i, id := range ids {
		entries[i] = makeLogEntry(id, "2026-01-02T03:04:05Z", "info", "sample-me")
	}

	rec := postLogEntries(t, h, entries)
	m := ingestCounts(t, rec)
	if m["accepted"] != len(kept) || m["sampled_out"] != len(dropped) || m["replayed"] != 0 {
		t.Fatalf("counts = %v, want accepted=%d sampled_out=%d", m, len(kept), len(dropped))
	}
	if m["accepted"]+m["replayed"]+m["sampled_out"] != len(entries) {
		t.Fatalf("counts do not sum to batch length: %v", m)
	}

	// Only kept ids are searchable; dropped ones left no trace.
	page := decodeLogPage(t, getLogs(t, h, "q=sample-me&limit=200"))
	got := map[string]bool{}
	for _, e := range page.Entries {
		got[e.ID] = true
	}
	for _, id := range kept {
		if !got[id] {
			t.Errorf("kept id %s missing from search", id)
		}
	}
	for _, id := range dropped {
		if got[id] {
			t.Errorf("dropped id %s visible in search", id)
		}
	}
}

func TestLogSamplingDeterministicAcrossBatchesAndReplays(t *testing.T) {
	h := Handler()
	if rec := putPolicy(t, h, `{"log_rate":0.5,"trace_rate":1}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("policy: %d", rec.Code)
	}
	kept, dropped := classifyLogIDs(0.5, 4)
	all := append(append([]string{}, kept...), dropped...)
	mk := func(ids []string) []string {
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = makeLogEntry(id, "2026-01-02T03:04:05Z", "info", "det")
		}
		return out
	}

	// Submit the same set split into two batches: the retained set is the
	// same regardless of how the batch is split.
	if rec := postLogEntries(t, h, mk(all[:len(all)/2])); rec.Code != http.StatusAccepted {
		t.Fatalf("batch 1: %d", rec.Code)
	}
	if rec := postLogEntries(t, h, mk(all[len(all)/2:])); rec.Code != http.StatusAccepted {
		t.Fatalf("batch 2: %d", rec.Code)
	}

	// Resubmit everything in one batch: kept ids replay, dropped ids are
	// sampled out again, nothing new is accepted.
	rec := postLogEntries(t, h, mk(all))
	m := ingestCounts(t, rec)
	if m["accepted"] != 0 || m["replayed"] != len(kept) || m["sampled_out"] != len(dropped) {
		t.Fatalf("resubmit counts = %v, want 0/%d/%d", m, len(kept), len(dropped))
	}
}

func TestTraceLogsSampledByTraceID(t *testing.T) {
	h := Handler()
	// log_rate 0 would drop every trace-less log; trace logs must follow
	// trace_rate instead.
	if rec := putPolicy(t, h, `{"log_rate":0,"trace_rate":1}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("policy: %d", rec.Code)
	}
	traceID := strings.Repeat("a", 32)
	body := `{"entries":[` +
		`{"id":"traced","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"t","labels":{},"trace_id":"` + traceID + `"},` +
		`{"id":"plain","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"p","labels":{}}` +
		`]}`
	rec := postLogs(t, h, body)
	m := ingestCounts(t, rec)
	if m["accepted"] != 1 || m["sampled_out"] != 1 {
		t.Fatalf("counts = %v, want the traced log kept and the plain one dropped", m)
	}
	page := decodeLogPage(t, getLogs(t, h, "limit=10"))
	if len(page.Entries) != 1 || page.Entries[0].ID != "traced" {
		t.Fatalf("visible logs = %v, want only traced", pageIDs(page))
	}

	// With trace_rate 0 the traced log is dropped even though log_rate is 1.
	if rec := putPolicy(t, h, `{"log_rate":1,"trace_rate":0}`, "t2"); rec.Code != http.StatusOK {
		t.Fatalf("policy t2: %d", rec.Code)
	}
	rec = postLogs(t, h, body, "t2")
	m = ingestCounts(t, rec)
	if m["accepted"] != 1 || m["sampled_out"] != 1 {
		t.Fatalf("t2 counts = %v, want plain kept and traced dropped", m)
	}
	page = decodeLogPage(t, getLogs(t, h, "limit=10", "t2"))
	if len(page.Entries) != 1 || page.Entries[0].ID != "plain" {
		t.Fatalf("t2 visible logs = %v, want only plain", pageIDs(page))
	}
}

// ---- span/trace sampling ---------------------------------------------------

func classifyTraces(rate float64, n int) (kept, dropped []string) {
	for i := 0; len(kept) < n || len(dropped) < n; i++ {
		tid := fmt.Sprintf("%032x", i)
		if sampleIn(tid, rate) {
			if len(kept) < n {
				kept = append(kept, tid)
			}
		} else if len(dropped) < n {
			dropped = append(dropped, tid)
		}
	}
	return kept, dropped
}

func TestSpanSamplingByTraceID(t *testing.T) {
	h := Handler()
	if rec := putPolicy(t, h, `{"log_rate":1,"trace_rate":0.5}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("policy: %d", rec.Code)
	}
	keptTraces, droppedTraces := classifyTraces(0.5, 2)
	spans := []string{
		makeSpan(keptTraces[0], "0000000000000001", "", "web", "a", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
		makeSpan(droppedTraces[0], "0000000000000002", "", "web", "b", "2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
	}
	rec := postSpanBatch(t, h, spans)
	m := ingestCounts(t, rec)
	if m["accepted"] != 1 || m["sampled_out"] != 1 {
		t.Fatalf("span counts = %v, want 1 kept / 1 dropped", m)
	}

	// Every span of one trace shares the decision: post two spans of a kept
	// and two of a dropped trace in a single batch.
	spans = []string{
		makeSpan(keptTraces[1], "0000000000000001", "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:09Z", "ok", `{}`),
		makeSpan(keptTraces[1], "0000000000000002", "0000000000000001", "web", "child", "2026-01-02T03:04:06Z", "2026-01-02T03:04:07Z", "ok", `{}`),
		makeSpan(droppedTraces[1], "0000000000000003", "", "web", "root", "2026-01-02T03:04:05Z", "2026-01-02T03:04:09Z", "ok", `{}`),
		makeSpan(droppedTraces[1], "0000000000000004", "0000000000000003", "web", "child", "2026-01-02T03:04:06Z", "2026-01-02T03:04:07Z", "ok", `{}`),
	}
	rec = postSpanBatch(t, h, spans)
	m = ingestCounts(t, rec)
	if m["accepted"] != 2 || m["sampled_out"] != 2 {
		t.Fatalf("multi-span counts = %v, want 2 kept / 2 dropped (whole traces together)", m)
	}

	if rec := getTrace(t, h, tracesPrefix+keptTraces[1]); rec.Code != http.StatusOK {
		t.Fatalf("kept trace: status = %d, want 200", rec.Code)
	} else if d := decodeTrace(t, rec); len(d.Spans) != 2 {
		t.Fatalf("kept trace spans = %d, want 2", len(d.Spans))
	}
	if rec := getTrace(t, h, tracesPrefix+droppedTraces[1]); rec.Code != http.StatusNotFound {
		t.Fatalf("dropped trace: status = %d, want 404", rec.Code)
	}
}

func TestLogsAndSpansOfATraceStayTogether(t *testing.T) {
	h := Handler()
	if rec := putPolicy(t, h, `{"log_rate":0,"trace_rate":0.5}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("policy: %d", rec.Code)
	}
	keptTraces, droppedTraces := classifyTraces(0.5, 1)
	keptTrace, droppedTrace := keptTraces[0], droppedTraces[0]

	span := func(tid, suffix string) string {
		return makeSpan(tid, "00000000000000"+suffix, "", "web", "s",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`)
	}
	logEntry := func(id, tid string) string {
		return `{"id":"` + id + `","timestamp":"2026-01-02T03:04:05Z","level":"info","message":"m","labels":{},"trace_id":"` + tid + `"}`
	}

	// Kept trace: span and its log both retained, regardless of endpoint order.
	if rec := postSpanBatch(t, h, []string{span(keptTrace, "01")}); rec.Code != http.StatusAccepted {
		t.Fatalf("kept span: %d", rec.Code)
	}
	if rec := postLogs(t, h, `{"entries":[`+logEntry("lk", keptTrace)+`]}`); rec.Code != http.StatusAccepted {
		t.Fatalf("kept log: %d", rec.Code)
	}
	d := decodeTrace(t, getTrace(t, h, tracesPrefix+keptTrace))
	if len(d.Spans) != 1 || len(d.Logs) != 1 {
		t.Fatalf("kept trace detail = %d spans %d logs, want 1/1", len(d.Spans), len(d.Logs))
	}

	// Dropped trace: log first, span later — both dropped and no trace exists.
	if rec := postLogs(t, h, `{"entries":[`+logEntry("ld", droppedTrace)+`]}`); rec.Code != http.StatusAccepted {
		t.Fatalf("dropped log: %d", rec.Code)
	}
	if rec := postSpanBatch(t, h, []string{span(droppedTrace, "02")}); rec.Code != http.StatusAccepted {
		t.Fatalf("dropped span: %d", rec.Code)
	}
	if rec := getTrace(t, h, tracesPrefix+droppedTrace); rec.Code != http.StatusNotFound {
		t.Fatalf("dropped trace detail: status = %d, want 404", rec.Code)
	}
	if page := decodeLogPage(t, getLogs(t, h, "trace_id="+droppedTrace)); len(page.Entries) != 0 {
		t.Fatalf("dropped trace log visible: %v", pageIDs(page))
	}
}

// ---- precedence and policy update semantics -------------------------------

func TestRetainedIdentityReplaysOrConflictsAfterRateLowers(t *testing.T) {
	h := Handler()
	id := "retained-id"
	entry := makeLogEntry(id, "2026-01-02T03:04:05Z", "info", "first")
	if rec := postLogEntries(t, h, []string{entry}); rec.Code != http.StatusAccepted {
		t.Fatalf("seed at rate 1: %d", rec.Code)
	}

	// Lower the rate to zero: the retained identity still replays identical
	// content rather than being sampled out.
	if rec := putPolicy(t, h, `{"log_rate":0,"trace_rate":0}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("lower policy: %d", rec.Code)
	}
	fresh := makeLogEntry("brand-new", "2026-01-02T03:04:05Z", "info", "fresh")
	rec := postLogEntries(t, h, []string{fresh, entry})
	m := ingestCounts(t, rec)
	if m["accepted"] != 0 || m["replayed"] != 1 || m["sampled_out"] != 1 {
		t.Fatalf("counts = %v, want replay=1 sampled_out=1", m)
	}

	// Different content for the retained identity is still a conflict that
	// fails the whole batch, even at rate 0.
	conflict := makeLogEntry(id, "2026-01-02T03:04:05Z", "info", "changed")
	rec = postLogEntries(t, h, []string{fresh, conflict})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "log_conflict" {
		t.Fatalf("conflict: status = %d code = %q, want 409 log_conflict", rec.Code, errorCode(t, rec))
	}
	// The fresh id in the failed batch must not have been committed either.
	// Raise the rate so a retry lands: a committed-in-failure id would replay,
	// a never-stored id is accepted.
	if rec := putPolicy(t, h, `{"log_rate":1,"trace_rate":1}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("raise policy: %d", rec.Code)
	}
	rec = postLogEntries(t, h, []string{fresh})
	m = ingestCounts(t, rec)
	if m["accepted"] != 1 || m["replayed"] != 0 {
		t.Fatalf("fresh after failed batch = %v, want accepted=1 (failed batch committed nothing)", m)
	}
}

func TestSampledOutIdentityCanBeAcceptedAfterRateRaises(t *testing.T) {
	h := Handler()
	if rec := putPolicy(t, h, `{"log_rate":0,"trace_rate":0}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("zero policy: %d", rec.Code)
	}
	entry := makeLogEntry("later", "2026-01-02T03:04:05Z", "info", "m")
	rec := postLogEntries(t, h, []string{entry})
	if m := ingestCounts(t, rec); m["accepted"] != 0 || m["sampled_out"] != 1 {
		t.Fatalf("at rate 0: %v", m)
	}

	// Raising the rate lets the never-retained identity in under the new policy.
	if rec := putPolicy(t, h, `{"log_rate":1,"trace_rate":1}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("raise policy: %d", rec.Code)
	}
	rec = postLogEntries(t, h, []string{entry})
	m := ingestCounts(t, rec)
	if m["accepted"] != 1 || m["replayed"] != 0 {
		t.Fatalf("after raise: %v, want accepted=1 (not a replay, never retained)", m)
	}
}

func TestPolicyUpdateIsNotRetroactive(t *testing.T) {
	h := Handler()
	entry := makeLogEntry("keep-forever", "2026-01-02T03:04:05Z", "info", "keep-forever-msg")
	if rec := postLogEntries(t, h, []string{entry}); rec.Code != http.StatusAccepted {
		t.Fatalf("seed: %d", rec.Code)
	}
	if rec := putPolicy(t, h, `{"log_rate":0,"trace_rate":0}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("lower: %d", rec.Code)
	}
	// Existing data is not deleted by the policy update.
	page := decodeLogPage(t, getLogs(t, h, "q=keep-forever-msg"))
	if len(page.Entries) != 1 {
		t.Fatalf("retained data vanished after policy update: %v", pageIDs(page))
	}
}

// ---- invalid batches and conflicts precede sampling -----------------------

func TestInvalidAndConflictingBatchesBypassSampling(t *testing.T) {
	h := Handler()
	if rec := putPolicy(t, h, `{"log_rate":0,"trace_rate":0}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("policy: %d", rec.Code)
	}

	// A malformed batch is invalid_logs, not a 202 full of sampled_out.
	rec := postLogs(t, h, `{"entries":[{"id":"x"}]}`)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_logs" {
		t.Fatalf("invalid logs: %d %q", rec.Code, errorCode(t, rec))
	}

	// Diverging in-batch duplicates conflict before sampling, even though at
	// rate 0 neither would be retained on its own.
	a := makeLogEntry("dup", "2026-01-02T03:04:05Z", "info", "one")
	b := makeLogEntry("dup", "2026-01-02T03:04:05Z", "warn", "two")
	rec = postLogEntries(t, h, []string{a, b})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "log_conflict" {
		t.Fatalf("logs in-batch conflict: %d %q", rec.Code, errorCode(t, rec))
	}

	// Same for spans: format errors stay invalid_spans at trace_rate 0.
	rec = postSpans(t, h, `{"spans":[{"trace_id":"short"}]}`)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_spans" {
		t.Fatalf("invalid spans: %d %q", rec.Code, errorCode(t, rec))
	}
}

// ---- dropped items consume no capacity or sequence ------------------------

func TestSampledOutLogsConsumeNoCapacityOrSnapshot(t *testing.T) {
	h := Handler()
	if rec := putPolicy(t, h, `{"log_rate":0,"trace_rate":0}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("policy: %d", rec.Code)
	}
	dropped := make([]string, 5)
	for i := range dropped {
		dropped[i] = makeLogEntry(fmt.Sprintf("d%d", i), "2026-01-02T03:04:05Z", "info", "dropped")
	}
	if rec := postLogEntries(t, h, dropped); rec.Code != http.StatusAccepted {
		t.Fatalf("dropped batch: %d", rec.Code)
	}
	if page := decodeLogPage(t, getLogs(t, h, "limit=200")); len(page.Entries) != 0 {
		t.Fatalf("dropped entries visible: %v", pageIDs(page))
	}

	// Kept entries afterwards begin a fresh commit sequence and are not
	// evicted or displaced by the dropped ids.
	if rec := putPolicy(t, h, `{"log_rate":1,"trace_rate":1}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("raise: %d", rec.Code)
	}
	kept := makeLogEntry("k0", "2026-01-02T03:04:05Z", "info", "kept")
	if rec := postLogEntries(t, h, []string{kept}); rec.Code != http.StatusAccepted {
		t.Fatalf("kept batch: %d", rec.Code)
	}
	page := decodeLogPage(t, getLogs(t, h, "q=kept"))
	if len(page.Entries) != 1 || page.Entries[0].ID != "k0" {
		t.Fatalf("kept entry missing: %v", pageIDs(page))
	}
	// A cursor pagination over the retained entry never surfaces dropped ids.
	if page.NextCursor != nil {
		next := decodeLogPage(t, getLogs(t, h, "cursor="+*page.NextCursor))
		if len(next.Entries) != 0 {
			t.Fatalf("later page surfaced dropped ids: %v", pageIDs(next))
		}
	}
}

// ---- sampled_out field presence follows the policy ------------------------

func TestSampledOutFieldTracksPolicyNotCount(t *testing.T) {
	h := Handler()
	// Active policy (log_rate < 1) but a guaranteed-kept identity under
	// trace_rate: the field must be present with value 0.
	if rec := putPolicy(t, h, `{"log_rate":0.5,"trace_rate":1}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("policy: %d", rec.Code)
	}
	kept, _ := classifyLogIDs(0.5, 1)
	rec := postLogEntries(t, h, []string{makeLogEntry(kept[0], "2026-01-02T03:04:05Z", "info", "m")})
	body := rec.Body.String()
	if !strings.Contains(body, `"sampled_out":0`) {
		t.Fatalf("active-policy logs body = %q, want sampled_out:0 present", body)
	}

	// Only log_rate being below 1 activates the field on the spans endpoint
	// too, even though trace_rate is 1 and no span is dropped.
	rec = postSpanBatch(t, h, []string{rootSpan(`{}`)})
	body = rec.Body.String()
	if rec.Code != http.StatusAccepted || !strings.Contains(body, `"sampled_out":0`) {
		t.Fatalf("active-policy spans body = %q, want sampled_out:0 present", body)
	}
}
