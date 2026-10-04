package server

import (
	"encoding/json"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	spansPath            = "/api/v1/spans"
	tracesCollectionPath = "/api/v1/traces"
	tracesPrefix         = tracesCollectionPath + "/"
)

var spanStatuses = map[string]bool{
	"unset": true,
	"ok":    true,
	"error": true,
}

// span is one committed trace span. Spans are immutable once stored.
type span struct {
	traceID      string
	spanID       string
	parentSpanID string // "" when parent_span_id is null
	service      string
	name         string
	startTime    time.Time
	endTime      time.Time
	status       string
	attributes   map[string]string
	seq          int64 // commit sequence, assigned at insert
}

// sameContent reports whether two spans with the same (trace_id, span_id)
// carry the same content, making the later one a replay rather than a
// conflict. Time instants compare regardless of the submitted zone.
func (sp *span) sameContent(other *span) bool {
	return sp.traceID == other.traceID &&
		sp.spanID == other.spanID &&
		sp.parentSpanID == other.parentSpanID &&
		sp.service == other.service &&
		sp.name == other.name &&
		sp.startTime.Equal(other.startTime) &&
		sp.endTime.Equal(other.endTime) &&
		sp.status == other.status &&
		maps.Equal(sp.attributes, other.attributes)
}

// spanKey builds the tenant-unique identity of a span from trace and span id.
// Both ids are lowercase hex of fixed length, so a NUL separator is
// unambiguous.
func spanKey(traceID, spanID string) string {
	return traceID + "\x00" + spanID
}

// checkAndApplySpans validates the batch against committed state (and against
// itself) and then commits it in array order. A false ok signals a content
// conflict on a repeated span; in that case no state is mutated. Replays —
// repeats of a (trace_id, span_id) with identical content — are counted but
// do not mutate state. Conflict and replay detection always runs before
// sampling: only spans not yet retained are subject to the tenant's sampling
// policy, decided on the trace id at trace_rate so a trace's spans and traced
// logs are kept or dropped together. Dropped spans leave no trace.
// sampling reports whether the policy in effect had any rate below 1, so the
// response can carry sampled_out.
func (s *metricStore) checkAndApplySpans(batch []*span) (accepted, replayed, sampledOutN int, sampling, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// First pass: conflict detection against committed spans and earlier
	// spans in the same batch.
	seen := make(map[string]*span, len(batch))
	for _, sp := range batch {
		key := spanKey(sp.traceID, sp.spanID)
		if prev, dup := seen[key]; dup {
			if !prev.sameContent(sp) {
				return 0, 0, 0, false, false
			}
			continue
		}
		if existing, committed := s.spans[key]; committed && !existing.sameContent(sp) {
			return 0, 0, 0, false, false
		}
		seen[key] = sp
	}

	// Second pass: commit in array order.
	policy := s.sampling
	sampling = policy.active()
	for _, sp := range batch {
		key := spanKey(sp.traceID, sp.spanID)
		if _, exists := s.spans[key]; exists {
			replayed++
			continue
		}
		if sampledOut(sp.traceID, policy.TraceRate) {
			sampledOutN++
			continue
		}
		s.spanSeq++
		sp.seq = s.spanSeq
		s.spans[key] = sp
		accepted++
	}
	return accepted, replayed, sampledOutN, sampling, true
}

// traceSnapshot is one consistent read of a trace's spans together with the
// retained logs carrying the same trace id.
type traceSnapshot struct {
	spans []*span
	logs  []*logEntry
}

// getTrace returns the spans of traceID and the currently retained logs with
// that trace id. The found flag is false only when no span is stored; logs
// alone never create a trace.
func (s *metricStore) getTrace(traceID string) (traceSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var snap traceSnapshot
	for _, sp := range s.spans {
		if sp.traceID == traceID {
			snap.spans = append(snap.spans, sp)
		}
	}
	if len(snap.spans) == 0 {
		return traceSnapshot{}, false
	}
	for _, e := range s.logs {
		if e.traceID == traceID {
			snap.logs = append(snap.logs, e)
		}
	}
	sort.Slice(snap.spans, func(i, j int) bool {
		a, b := snap.spans[i], snap.spans[j]
		if !a.startTime.Equal(b.startTime) {
			return a.startTime.Before(b.startTime)
		}
		return a.spanID < b.spanID
	})
	sort.Slice(snap.logs, func(i, j int) bool {
		a, b := snap.logs[i], snap.logs[j]
		if !a.timestamp.Equal(b.timestamp) {
			return a.timestamp.Before(b.timestamp)
		}
		return a.id < b.id
	})
	return snap, true
}

// ---- JSON wire shapes ------------------------------------------------------

type spansEnvelope struct {
	Spans *[]json.RawMessage `json:"spans"`
}

type rawSpan struct {
	TraceID      *string         `json:"trace_id"`
	SpanID       *string         `json:"span_id"`
	ParentSpanID json.RawMessage `json:"parent_span_id"`
	Service      *string         `json:"service"`
	Name         *string         `json:"name"`
	StartTime    *string         `json:"start_time"`
	EndTime      *string         `json:"end_time"`
	Status       *string         `json:"status"`
	Attributes   json.RawMessage `json:"attributes"`
}

type spanJSON struct {
	TraceID      string            `json:"trace_id"`
	SpanID       string            `json:"span_id"`
	ParentSpanID *string           `json:"parent_span_id"`
	Service      string            `json:"service"`
	Name         string            `json:"name"`
	StartTime    string            `json:"start_time"`
	EndTime      string            `json:"end_time"`
	Status       string            `json:"status"`
	Attributes   map[string]string `json:"attributes"`
}

func (sp *span) wireJSON() spanJSON {
	out := spanJSON{
		TraceID:    sp.traceID,
		SpanID:     sp.spanID,
		Service:    sp.service,
		Name:       sp.name,
		StartTime:  sp.startTime.UTC().Format(time.RFC3339Nano),
		EndTime:    sp.endTime.UTC().Format(time.RFC3339Nano),
		Status:     sp.status,
		Attributes: copyLabels(sp.attributes),
	}
	if sp.parentSpanID != "" {
		parent := sp.parentSpanID
		out.ParentSpanID = &parent
	}
	return out
}

// decodeSpanBatch performs every format and value check. It never returns a
// partially validated batch.
func decodeSpanBatch(body []byte) ([]*span, bool) {
	var env spansEnvelope
	if !strictDecode(body, &env) || env.Spans == nil || len(*env.Spans) == 0 {
		return nil, false
	}

	batch := make([]*span, 0, len(*env.Spans))
	for _, raw := range *env.Spans {
		var rs rawSpan
		if !strictDecode(raw, &rs) {
			return nil, false
		}
		if rs.TraceID == nil || rs.SpanID == nil || len(rs.ParentSpanID) == 0 ||
			rs.Service == nil || rs.Name == nil || rs.StartTime == nil ||
			rs.EndTime == nil || rs.Status == nil || rs.Attributes == nil {
			return nil, false
		}
		traceID := *rs.TraceID
		if !validTraceID(traceID) {
			return nil, false
		}
		spanID := *rs.SpanID
		if !validSpanID(spanID) {
			return nil, false
		}
		// parent_span_id is exactly null or a span-id string; a missing field
		// was rejected above. The parent span itself may arrive later.
		var parentPtr *string
		if err := json.Unmarshal(rs.ParentSpanID, &parentPtr); err != nil {
			return nil, false
		}
		parentSpanID := ""
		if parentPtr != nil {
			parentSpanID = *parentPtr
			if !validSpanID(parentSpanID) || parentSpanID == spanID {
				return nil, false
			}
		}
		service := *rs.Service
		name := *rs.Name
		if service == "" || name == "" {
			return nil, false
		}
		startTime, err := time.Parse(time.RFC3339Nano, *rs.StartTime)
		if err != nil {
			return nil, false
		}
		endTime, err := time.Parse(time.RFC3339Nano, *rs.EndTime)
		if err != nil {
			return nil, false
		}
		if endTime.Before(startTime) {
			return nil, false
		}
		status := *rs.Status
		if !spanStatuses[status] {
			return nil, false
		}
		// attributes must be a JSON object of string values. RawMessage keeps
		// explicit nulls visible: encoding/json would otherwise fold a null
		// value into the empty string, and a whole-document null into a nil
		// map; both are illegal here.
		var rawAttrs map[string]json.RawMessage
		if err := json.Unmarshal(rs.Attributes, &rawAttrs); err != nil || rawAttrs == nil {
			return nil, false
		}
		attributes := make(map[string]string, len(rawAttrs))
		for k, rawVal := range rawAttrs {
			if !identPattern.MatchString(k) {
				return nil, false
			}
			var value *string
			if err := json.Unmarshal(rawVal, &value); err != nil || value == nil {
				return nil, false
			}
			attributes[k] = *value
		}
		batch = append(batch, &span{
			traceID:      traceID,
			spanID:       spanID,
			parentSpanID: parentSpanID,
			service:      service,
			name:         name,
			startTime:    startTime,
			endTime:      endTime,
			status:       status,
			attributes:   attributes,
		})
	}
	return batch, true
}

// validSpanID reports whether id is exactly 16 lowercase hex characters.
func validSpanID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// ---- HTTP handlers ---------------------------------------------------------

func registerSpanHandlers(mux *http.ServeMux) {
	mux.HandleFunc(spansPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		handleSpansPost(w, r)
	})

	// The trace collection supports only the search GET; anything else is a
	// method error with the single allowed verb advertised.
	mux.HandleFunc(tracesCollectionPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		handleTracesGet(w, r)
	})

	mux.HandleFunc(tracesPrefix, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		handleTraceGet(w, r)
	})
}

func handleSpansPost(w http.ResponseWriter, r *http.Request) {
	store := tenantStore(r)
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, "invalid_spans", http.StatusBadRequest)
		return
	}

	batch, ok := decodeSpanBatch(body)
	if !ok {
		writeAPIError(w, "invalid_spans", http.StatusBadRequest)
		return
	}

	accepted, replayed, sampledOutN, sampling, applied := store.checkAndApplySpans(batch)
	if !applied {
		writeAPIError(w, "span_conflict", http.StatusConflict)
		return
	}

	resp := map[string]int{"accepted": accepted, "replayed": replayed}
	if sampling {
		resp["sampled_out"] = sampledOutN
	}
	writeJSON(w, http.StatusAccepted, resp)
}

func handleTraceGet(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimPrefix(r.URL.Path, tracesPrefix)
	if traceID == "" || strings.Contains(traceID, "/") || !validTraceID(traceID) {
		writeAPIError(w, "invalid_trace_id", http.StatusBadRequest)
		return
	}
	// The trace detail endpoint defines no query vocabulary, so any parameter
	// is an invalid query.
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values) != 0 {
		writeAPIError(w, "invalid_trace_query", http.StatusBadRequest)
		return
	}

	snap, found := tenantStore(r).getTrace(traceID)
	if !found {
		writeAPIError(w, "trace_not_found", http.StatusNotFound)
		return
	}

	spans := make([]spanJSON, 0, len(snap.spans))
	for _, sp := range snap.spans {
		spans = append(spans, sp.wireJSON())
	}
	logs := make([]logEntryJSON, 0, len(snap.logs))
	for _, e := range snap.logs {
		logs = append(logs, e.wireJSON())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"trace_id": traceID,
		"spans":    spans,
		"logs":     logs,
	})
}
