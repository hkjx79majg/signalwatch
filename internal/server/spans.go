package server

import (
	"encoding/json"
	"io"
	"maps"
	"mime"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	spansPath    = "/api/v1/spans"
	tracesPrefix = "/api/v1/traces/"
)

var spanStatuses = map[string]bool{
	"unset": true,
	"ok":    true,
	"error": true,
}

// span is one committed trace span. Spans are immutable once stored; a later
// submission of the same (trace_id, span_id) is either a replay of identical
// content or a conflict.
type span struct {
	traceID      string
	spanID       string
	parentSpanID string // "" when the submitted parent_span_id was null
	service      string
	name         string
	start        time.Time
	end          time.Time
	status       string
	attributes   map[string]string
}

// sameContent reports whether two spans with the same identity carry the same
// content, making the later one a replay rather than a conflict. Timestamps
// compare as instants, so equal moments in different offsets still replay.
func (sp *span) sameContent(other *span) bool {
	return sp.traceID == other.traceID &&
		sp.spanID == other.spanID &&
		sp.parentSpanID == other.parentSpanID &&
		sp.service == other.service &&
		sp.name == other.name &&
		sp.start.Equal(other.start) &&
		sp.end.Equal(other.end) &&
		sp.status == other.status &&
		maps.Equal(sp.attributes, other.attributes)
}

// spanKey builds the per-tenant identity of a span. Both ids are fixed-length
// lowercase hex, so plain concatenation is unambiguous.
func spanKey(traceID, spanID string) string {
	return traceID + spanID
}

// checkAndApplySpans validates the batch against committed state (and against
// itself) and then commits it in array order. A false result signals a content
// conflict on a repeated (trace_id, span_id); in that case no state is
// mutated. Replays — repeats of an identity with identical content — are
// counted but do not mutate state.
func (s *metricStore) checkAndApplySpans(batch []*span) (accepted, replayed int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// First pass: conflict detection against committed spans and earlier
	// spans in the same batch.
	seen := make(map[string]*span, len(batch))
	for _, sp := range batch {
		key := spanKey(sp.traceID, sp.spanID)
		if prev, dup := seen[key]; dup {
			if !prev.sameContent(sp) {
				return 0, 0, false
			}
			continue
		}
		if existing, committed := s.spans[key]; committed && !existing.sameContent(sp) {
			return 0, 0, false
		}
		seen[key] = sp
	}

	// Second pass: commit in array order.
	for _, sp := range batch {
		key := spanKey(sp.traceID, sp.spanID)
		if _, exists := s.spans[key]; exists {
			replayed++
			continue
		}
		s.spans[key] = sp
		accepted++
	}
	return accepted, replayed, true
}

// traceDetail returns the spans of one trace ordered by start instant then
// span id, plus the currently retained log entries carrying the same trace id
// ordered by timestamp then id. The boolean is false when the trace has no
// stored spans; logs alone never create a trace.
func (s *metricStore) traceDetail(traceID string) ([]*span, []*logEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	spans := make([]*span, 0)
	for _, sp := range s.spans {
		if sp.traceID == traceID {
			spans = append(spans, sp)
		}
	}
	if len(spans) == 0 {
		return nil, nil, false
	}
	sort.Slice(spans, func(i, j int) bool {
		a, b := spans[i], spans[j]
		if !a.start.Equal(b.start) {
			return a.start.Before(b.start)
		}
		return a.spanID < b.spanID
	})

	entries := make([]*logEntry, 0)
	for _, e := range s.logs {
		if e.traceID == traceID {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.timestamp.Equal(b.timestamp) {
			return a.timestamp.Before(b.timestamp)
		}
		return a.id < b.id
	})
	return spans, entries, true
}

// ---- JSON wire shapes ------------------------------------------------------

type spansEnvelope struct {
	Spans *[]json.RawMessage `json:"spans"`
}

// rawSpan uses pointer fields so missing keys are distinguishable from
// explicit nulls. parent_span_id is raw because null is a valid submitted
// value; a present null decodes to a nil raw message, so key presence is
// verified separately on the raw key set.
type rawSpan struct {
	TraceID      *string             `json:"trace_id"`
	SpanID       *string             `json:"span_id"`
	ParentSpanID *json.RawMessage    `json:"parent_span_id"`
	Service      *string             `json:"service"`
	Name         *string             `json:"name"`
	StartTime    *string             `json:"start_time"`
	EndTime      *string             `json:"end_time"`
	Status       *string             `json:"status"`
	Attributes   *map[string]*string `json:"attributes"`
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
	var parent *string
	if sp.parentSpanID != "" {
		p := sp.parentSpanID
		parent = &p
	}
	return spanJSON{
		TraceID:      sp.traceID,
		SpanID:       sp.spanID,
		ParentSpanID: parent,
		Service:      sp.service,
		Name:         sp.name,
		StartTime:    sp.start.UTC().Format(time.RFC3339Nano),
		EndTime:      sp.end.UTC().Format(time.RFC3339Nano),
		Status:       sp.status,
		Attributes:   copyLabels(sp.attributes),
	}
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
		// A present null parent_span_id decodes identically to a missing key,
		// so presence is checked on the raw key set.
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			return nil, false
		}
		if _, present := keys["parent_span_id"]; !present {
			return nil, false
		}
		if rs.TraceID == nil || rs.SpanID == nil ||
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
		parentSpanID := ""
		if rs.ParentSpanID != nil { // nil raw message is the explicit null
			var parent *string
			if err := json.Unmarshal(*rs.ParentSpanID, &parent); err != nil || parent == nil {
				return nil, false
			}
			if !validSpanID(*parent) || *parent == spanID {
				return nil, false
			}
			parentSpanID = *parent
		}
		if *rs.Service == "" || *rs.Name == "" {
			return nil, false
		}
		start, err := time.Parse(time.RFC3339Nano, *rs.StartTime)
		if err != nil {
			return nil, false
		}
		end, err := time.Parse(time.RFC3339Nano, *rs.EndTime)
		if err != nil {
			return nil, false
		}
		if end.Before(start) {
			return nil, false
		}
		status := *rs.Status
		if !spanStatuses[status] {
			return nil, false
		}
		rawAttrs := *rs.Attributes
		if rawAttrs == nil {
			return nil, false // explicit null attributes
		}
		attributes := make(map[string]string, len(rawAttrs))
		for k, v := range rawAttrs {
			if !identPattern.MatchString(k) || v == nil {
				return nil, false
			}
			attributes[k] = *v
		}
		batch = append(batch, &span{
			traceID:      traceID,
			spanID:       spanID,
			parentSpanID: parentSpanID,
			service:      *rs.Service,
			name:         *rs.Name,
			start:        start,
			end:          end,
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

	accepted, replayed, applied := store.checkAndApplySpans(batch)
	if !applied {
		writeAPIError(w, "span_conflict", http.StatusConflict)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]int{"accepted": accepted, "replayed": replayed})
}

func handleTraceGet(w http.ResponseWriter, r *http.Request) {
	store := tenantStore(r)
	id := strings.TrimPrefix(r.URL.Path, tracesPrefix)
	// A structurally nested path is outside this resource subtree and simply
	// not found; an identifier-shaped but malformed trace id is invalid.
	if id == "" || strings.Contains(id, "/") {
		writeAPIError(w, "trace_not_found", http.StatusNotFound)
		return
	}
	if !validTraceID(id) {
		writeAPIError(w, "invalid_trace_id", http.StatusBadRequest)
		return
	}
	// The trace endpoint takes no query parameters.
	if r.URL.RawQuery != "" {
		writeAPIError(w, "invalid_trace_query", http.StatusBadRequest)
		return
	}

	spans, entries, ok := store.traceDetail(id)
	if !ok {
		writeAPIError(w, "trace_not_found", http.StatusNotFound)
		return
	}

	spanBody := make([]spanJSON, 0, len(spans))
	for _, sp := range spans {
		spanBody = append(spanBody, sp.wireJSON())
	}
	logBody := make([]logEntryJSON, 0, len(entries))
	for _, e := range entries {
		logBody = append(logBody, e.wireJSON())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"trace_id": id,
		"spans":    spanBody,
		"logs":     logBody,
	})
}
