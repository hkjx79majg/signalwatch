package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- helpers ---------------------------------------------------------------

func getSamplingPolicy(t *testing.T, h http.Handler, query string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	target := samplingPolicyPath
	if query != "" {
		target += "?" + query
	}
	return doRequest(t, h, http.MethodGet, target, "", "", tenants...)
}

func putSamplingPolicy(t *testing.T, h http.Handler, body, contentType string, tenants ...string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodPut, samplingPolicyPath, body, contentType, tenants...)
}

type samplingPolicyPayload struct {
	LogRate   float64 `json:"log_rate"`
	TraceRate float64 `json:"trace_rate"`
}

func decodeSamplingPolicyPayload(t *testing.T, rec *httptest.ResponseRecorder) samplingPolicyPayload {
	t.Helper()
	var p samplingPolicyPayload
	if err := decodeJSON(rec, &p); err != nil {
		t.Fatalf("decode sampling policy: %v", err)
	}
	return p
}

// ---- GET/PUT /api/v1/sampling-policy ---------------------------------------

func TestSamplingPolicyDefault(t *testing.T) {
	h := Handler()
	rec := getSamplingPolicy(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	p := decodeSamplingPolicyPayload(t, rec)
	if p.LogRate != 1 || p.TraceRate != 1 {
		t.Fatalf("policy = %+v, want both rates 1", p)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"log_rate":1`) || !strings.Contains(got, `"trace_rate":1`) {
		t.Fatalf("body = %s, want literal 1 rates", got)
	}
}

func TestSamplingPolicyPutAndGet(t *testing.T) {
	h := Handler()
	rec := putSamplingPolicy(t, h, `{"log_rate":0.25,"trace_rate":0}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	p := decodeSamplingPolicyPayload(t, rec)
	if p.LogRate != 0.25 || p.TraceRate != 0 {
		t.Fatalf("put response = %+v, want 0.25/0", p)
	}
	got := decodeSamplingPolicyPayload(t, getSamplingPolicy(t, h, ""))
	if got.LogRate != 0.25 || got.TraceRate != 0 {
		t.Fatalf("get after put = %+v, want 0.25/0", got)
	}

	// Atomic replace: a second PUT swaps the whole policy.
	rec = putSamplingPolicy(t, h, `{"log_rate":1,"trace_rate":1}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got = decodeSamplingPolicyPayload(t, getSamplingPolicy(t, h, ""))
	if got.LogRate != 1 || got.TraceRate != 1 {
		t.Fatalf("get after second put = %+v, want 1/1", got)
	}
}

func TestSamplingPolicyPutBoundaryRates(t *testing.T) {
	h := Handler()
	for _, body := range []string{
		`{"log_rate":0,"trace_rate":0}`,
		`{"log_rate":1,"trace_rate":1}`,
		`{"log_rate":0.0,"trace_rate":1.0}`,
		`{"log_rate":1e-3,"trace_rate":0.999}`,
	} {
		rec := putSamplingPolicy(t, h, body, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: status = %d, want 200", body, rec.Code)
		}
	}
}

func TestSamplingPolicyPutInvalid(t *testing.T) {
	h := Handler()
	for _, body := range []string{
		`{"log_rate":0.5}`,                 // missing trace_rate
		`{"trace_rate":0.5}`,               // missing log_rate
		`{}`,                               // both missing
		`{"log_rate":null,"trace_rate":1}`, // null field
		`{"log_rate":0.5,"trace_rate":0.5,"extra":1}`, // unknown field
		`{"log_rate":"0.5","trace_rate":1}`,           // wrong type
		`{"log_rate":true,"trace_rate":1}`,            // wrong type
		`{"log_rate":-0.1,"trace_rate":1}`,            // below range
		`{"log_rate":1,"trace_rate":1.1}`,             // above range
		`{"log_rate":1e999,"trace_rate":1}`,           // non-finite
		`{"log_rate":0.5,"trace_rate":0.5`,            // malformed JSON
		`[]`,                                          // not an object
		``,                                            // empty body
	} {
		rec := putSamplingPolicy(t, h, body, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %q: status = %d, want 400", body, rec.Code)
		}
		if code := errorCode(t, rec); code != "invalid_sampling_policy" {
			t.Fatalf("PUT %q: code = %q, want invalid_sampling_policy", body, code)
		}
	}
	// Failed PUTs never change the stored policy.
	p := decodeSamplingPolicyPayload(t, getSamplingPolicy(t, h, ""))
	if p.LogRate != 1 || p.TraceRate != 1 {
		t.Fatalf("policy after invalid PUTs = %+v, want default 1/1", p)
	}
}

func TestSamplingPolicyPutUnsupportedMediaType(t *testing.T) {
	h := Handler()
	for _, ct := range []string{"text/plain", "application/xml", ""} {
		rec := putSamplingPolicy(t, h, `{"log_rate":0.5,"trace_rate":0.5}`, ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("content-type %q: status = %d, want 415", ct, rec.Code)
		}
		if code := errorCode(t, rec); code != "unsupported_media_type" {
			t.Fatalf("content-type %q: code = %q, want unsupported_media_type", ct, code)
		}
	}
}

func TestSamplingPolicyGetWithQuery(t *testing.T) {
	h := Handler()
	rec := getSamplingPolicy(t, h, "log_rate=0.5")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := errorCode(t, rec); code != "invalid_sampling_query" {
		t.Fatalf("code = %q, want invalid_sampling_query", code)
	}
}

func TestSamplingPolicyMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		rec := doRequest(t, h, method, samplingPolicyPath, `{"log_rate":1,"trace_rate":1}`, "application/json")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, PUT" {
			t.Fatalf("%s: Allow = %q, want %q", method, allow, "GET, PUT")
		}
	}
}

func TestSamplingPolicyTenantIsolation(t *testing.T) {
	h := Handler()
	rec := putSamplingPolicy(t, h, `{"log_rate":0,"trace_rate":0}`, "application/json", "tenant-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The default tenant and any other tenant are unaffected.
	p := decodeSamplingPolicyPayload(t, getSamplingPolicy(t, h, ""))
	if p.LogRate != 1 || p.TraceRate != 1 {
		t.Fatalf("default tenant sees %+v, want default 1/1", p)
	}
	p = decodeSamplingPolicyPayload(t, getSamplingPolicy(t, h, "", "tenant-b"))
	if p.LogRate != 1 || p.TraceRate != 1 {
		t.Fatalf("tenant-b sees %+v, want default 1/1", p)
	}
	p = decodeSamplingPolicyPayload(t, getSamplingPolicy(t, h, "", "tenant-a"))
	if p.LogRate != 0 || p.TraceRate != 0 {
		t.Fatalf("tenant-a sees %+v, want 0/0", p)
	}
}

func TestSamplingPolicyInvalidTenantPrecedence(t *testing.T) {
	h := Handler()
	// A malformed tenant wins over media type, body and query validation.
	rec := doRequest(t, h, http.MethodPut, samplingPolicyPath+"?x=1", "not json", "text/plain", "bad tenant!")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := errorCode(t, rec); code != "invalid_tenant" {
		t.Fatalf("code = %q, want invalid_tenant", code)
	}
}

// ---- ingest sampling --------------------------------------------------------

func TestSamplingLogsZeroRate(t *testing.T) {
	h := Handler()
	putSamplingPolicy(t, h, `{"log_rate":0,"trace_rate":1}`, "application/json")

	rec := postLogEntries(t, h, []string{
		makeLogEntry("a", "2026-01-02T03:04:05Z", "info", "one"),
		makeLogEntry("b", "2026-01-02T03:04:06Z", "info", "two"),
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	var payload map[string]int
	if err := decodeJSON(rec, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["accepted"] != 0 || payload["replayed"] != 0 || payload["sampled_out"] != 2 {
		t.Fatalf("payload = %v, want accepted=0 replayed=0 sampled_out=2", payload)
	}

	// Dropped entries are invisible to retrieval.
	page := decodeLogPage(t, getLogs(t, h, ""))
	if len(page.Entries) != 0 {
		t.Fatalf("retrieved %d entries, want 0", len(page.Entries))
	}

	// Raising the rate lets the same identities in again, judged fresh.
	putSamplingPolicy(t, h, `{"log_rate":1,"trace_rate":1}`, "application/json")
	rec = postLogEntries(t, h, []string{
		makeLogEntry("a", "2026-01-02T03:04:05Z", "info", "one"),
	})
	var again map[string]int
	if err := decodeJSON(rec, &again); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if again["accepted"] != 1 {
		t.Fatalf("after raising rate payload = %v, want accepted=1", again)
	}
}

func TestSamplingLogsReplayAndConflictSurviveLowering(t *testing.T) {
	h := Handler()
	entry := makeLogEntry("keep", "2026-01-02T03:04:05Z", "info", "hello")
	postLogEntries(t, h, []string{entry})

	putSamplingPolicy(t, h, `{"log_rate":0,"trace_rate":0}`, "application/json")

	// An already-retained id with identical content still replays.
	rec := postLogEntries(t, h, []string{entry})
	var payload map[string]int
	if err := decodeJSON(rec, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["replayed"] != 1 || payload["accepted"] != 0 || payload["sampled_out"] != 0 {
		t.Fatalf("payload = %v, want replayed=1 only", payload)
	}

	// An already-retained id with different content still conflicts.
	rec = postLogEntries(t, h, []string{
		makeLogEntry("keep", "2026-01-02T03:04:05Z", "error", "changed"),
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if code := errorCode(t, rec); code != "log_conflict" {
		t.Fatalf("code = %q, want log_conflict", code)
	}
}

func TestSamplingSpansZeroRate(t *testing.T) {
	h := Handler()
	putSamplingPolicy(t, h, `{"log_rate":1,"trace_rate":0}`, "application/json")

	rec := postSpanBatch(t, h, []string{rootSpan(`{"route":"/a"}`)})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	var payload map[string]int
	if err := decodeJSON(rec, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["accepted"] != 0 || payload["replayed"] != 0 || payload["sampled_out"] != 1 {
		t.Fatalf("payload = %v, want accepted=0 replayed=0 sampled_out=1", payload)
	}

	// A dropped span never creates a trace.
	rec = getTrace(t, h, tracesPrefix+sampleTraceID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("trace status = %d, want 404", rec.Code)
	}
}

func TestSamplingDefaultRatesKeepExistingResponseShape(t *testing.T) {
	h := Handler()
	rec := postLogEntries(t, h, []string{
		makeLogEntry("a", "2026-01-02T03:04:05Z", "info", "one"),
	})
	var payload map[string]int
	if err := decodeJSON(rec, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := payload["sampled_out"]; present {
		t.Fatalf("payload = %v, must not contain sampled_out at default rates", payload)
	}
	if payload["accepted"] != 1 || payload["replayed"] != 0 {
		t.Fatalf("payload = %v, want accepted=1 replayed=0", payload)
	}

	rec = postSpanBatch(t, h, []string{rootSpan(`{"route":"/a"}`)})
	payload = map[string]int{}
	if err := decodeJSON(rec, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := payload["sampled_out"]; present {
		t.Fatalf("span payload = %v, must not contain sampled_out at default rates", payload)
	}
}

func TestSamplingCountsSumToBatchLength(t *testing.T) {
	h := Handler()
	putSamplingPolicy(t, h, `{"log_rate":0.5,"trace_rate":1}`, "application/json")

	entries := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		entries = append(entries, makeLogEntry(
			fmt.Sprintf("log-%02d", i), "2026-01-02T03:04:05Z", "info", "msg"))
	}
	rec := postLogEntries(t, h, entries)
	var payload map[string]int
	if err := decodeJSON(rec, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	total := payload["accepted"] + payload["replayed"] + payload["sampled_out"]
	if total != len(entries) {
		t.Fatalf("payload = %v sums to %d, want %d", payload, total, len(entries))
	}

	// The kept set is exactly what the deterministic decision predicts, and
	// retrieval agrees with it.
	wantKept := 0
	for i := 0; i < 20; i++ {
		if !sampledOut(fmt.Sprintf("log-%02d", i), 0.5) {
			wantKept++
		}
	}
	if payload["accepted"] != wantKept {
		t.Fatalf("accepted = %d, want deterministic %d", payload["accepted"], wantKept)
	}
	page := decodeLogPage(t, getLogs(t, h, ""))
	if len(page.Entries) != wantKept {
		t.Fatalf("retrieved %d entries, want %d", len(page.Entries), wantKept)
	}
}

func TestSamplingTraceConsistencyAcrossSignalsAndBatches(t *testing.T) {
	h := Handler()
	putSamplingPolicy(t, h, `{"log_rate":1,"trace_rate":0.5}`, "application/json")

	// Find one kept and one dropped trace id at rate 0.5.
	var keptTrace, droppedTrace string
	for i := 0; i < 200 && (keptTrace == "" || droppedTrace == ""); i++ {
		id := fmt.Sprintf("4bf92f3577b34da6a3ce929d0e0e%04d", i)
		if sampledOut(id, 0.5) {
			droppedTrace = id
		} else {
			keptTrace = id
		}
	}
	if keptTrace == "" || droppedTrace == "" {
		t.Fatal("could not find kept and dropped trace ids")
	}

	tracedLog := func(id, traceID string) string {
		return fmt.Sprintf(`{"id":%q,"timestamp":"2026-01-02T03:04:05Z","level":"info",`+
			`"message":"m","labels":{"app":"demo"},"trace_id":%q}`, id, traceID)
	}

	// Spans and traced logs of the same trace arrive in separate batches and
	// in either order; both signals must make the same keep/drop decision.
	spanRec := postSpanBatch(t, h, []string{
		makeSpan(keptTrace, "0000000000000001", "", "web", "root",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
		makeSpan(droppedTrace, "0000000000000002", "", "web", "root",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
	})
	var spanPayload map[string]int
	if err := decodeJSON(spanRec, &spanPayload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if spanPayload["accepted"] != 1 || spanPayload["sampled_out"] != 1 {
		t.Fatalf("span payload = %v, want accepted=1 sampled_out=1", spanPayload)
	}

	logRec := postLogEntries(t, h, []string{
		tracedLog("log-kept", keptTrace),
		tracedLog("log-dropped", droppedTrace),
	})
	var logPayload map[string]int
	if err := decodeJSON(logRec, &logPayload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if logPayload["accepted"] != 1 || logPayload["sampled_out"] != 1 {
		t.Fatalf("log payload = %v, want accepted=1 sampled_out=1", logPayload)
	}

	// The kept trace shows span and log together; the dropped trace is absent.
	detail := decodeTrace(t, getTrace(t, h, tracesPrefix+keptTrace))
	if len(detail.Spans) != 1 || len(detail.Logs) != 1 {
		t.Fatalf("kept trace has %d spans and %d logs, want 1/1", len(detail.Spans), len(detail.Logs))
	}
	if rec := getTrace(t, h, tracesPrefix+droppedTrace); rec.Code != http.StatusNotFound {
		t.Fatalf("dropped trace status = %d, want 404", rec.Code)
	}

	// Replaying the same submissions in a different batch split is stable.
	spanRec = postSpanBatch(t, h, []string{
		makeSpan(keptTrace, "0000000000000001", "", "web", "root",
			"2026-01-02T03:04:05Z", "2026-01-02T03:04:06Z", "ok", `{}`),
	})
	if err := decodeJSON(spanRec, &spanPayload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if spanPayload["replayed"] != 1 {
		t.Fatalf("replay payload = %v, want replayed=1", spanPayload)
	}
}

func TestSamplingUntracedLogsUseLogRateOnly(t *testing.T) {
	h := Handler()
	// trace_rate 0 must not affect logs without a trace id.
	putSamplingPolicy(t, h, `{"log_rate":1,"trace_rate":0}`, "application/json")
	rec := postLogEntries(t, h, []string{
		makeLogEntry("plain", "2026-01-02T03:04:05Z", "info", "no trace"),
	})
	var payload map[string]int
	if err := decodeJSON(rec, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["accepted"] != 1 {
		t.Fatalf("payload = %v, want accepted=1", payload)
	}
}
