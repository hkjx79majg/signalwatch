package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTraceLimit = 100
	maxTraceLimit     = 200
)

// traceFilter is the immutable condition set of a trace search. The time
// window is always closed: [start, end) on the trace's earliest span start.
type traceFilter struct {
	start   time.Time
	end     time.Time
	service string // "" when absent
	name    string // "" when absent
	status  string // "" when absent
}

// spanMatches reports whether one span satisfies every span-level condition
// at once: several conditions must be met by the same span, not by
// different spans of the trace.
func (f *traceFilter) spanMatches(sp *span) bool {
	if f.service != "" && sp.service != f.service {
		return false
	}
	if f.name != "" && sp.name != f.name {
		return false
	}
	if f.status != "" && sp.status != f.status {
		return false
	}
	return true
}

func (f *traceFilter) hasSpanConditions() bool {
	return f.service != "" || f.name != "" || f.status != ""
}

// traceSummary is the aggregated view of one trace's stored spans.
type traceSummary struct {
	traceID   string
	startTime time.Time // earliest span start
	endTime   time.Time // latest span end
	spanCount int
	services  []string // deduplicated, lexicographic
	status    string   // error if any error, else ok if any ok, else unset
}

// queryTraces returns the page of trace summaries matching the filter within
// the snapshot (spans with seq <= snapshotSeq), strictly after the position
// (afterStart, afterTrace) in result order, plus whether more traces remain.
// Result order is start_time descending, trace_id ascending within one
// instant. Spans are immutable once stored, so the snapshot fixes every
// summary for the whole pagination run: later writes carry higher sequences
// and never mix in, and the position anchor prevents duplicates.
func (s *metricStore) queryTraces(f *traceFilter, snapshotSeq int64, afterStart time.Time, afterTrace string, hasAfter bool, limit int) ([]*traceSummary, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	byTrace := make(map[string][]*span)
	for _, sp := range s.spans {
		if sp.seq > snapshotSeq {
			continue // written after the snapshot anchor; never mixes in
		}
		byTrace[sp.traceID] = append(byTrace[sp.traceID], sp)
	}

	matched := make([]*traceSummary, 0)
	for traceID, spans := range byTrace {
		summary := summarizeTrace(traceID, spans)
		if summary.startTime.Before(f.start) || !summary.startTime.Before(f.end) {
			continue
		}
		if f.hasSpanConditions() {
			ok := false
			for _, sp := range spans {
				if f.spanMatches(sp) {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		if hasAfter && !summaryComesAfter(summary, afterStart, afterTrace) {
			continue
		}
		matched = append(matched, summary)
	}
	sort.Slice(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if !a.startTime.Equal(b.startTime) {
			return a.startTime.After(b.startTime)
		}
		return a.traceID < b.traceID
	})

	more := len(matched) > limit
	if more {
		matched = matched[:limit]
	}
	return matched, more
}

// summarizeTrace aggregates one trace's spans into its summary. The status
// folds to the most severe present: error beats ok beats unset.
func summarizeTrace(traceID string, spans []*span) *traceSummary {
	summary := &traceSummary{traceID: traceID, spanCount: len(spans), status: "unset"}
	seen := make(map[string]bool, len(spans))
	for i, sp := range spans {
		if i == 0 || sp.startTime.Before(summary.startTime) {
			summary.startTime = sp.startTime
		}
		if i == 0 || sp.endTime.After(summary.endTime) {
			summary.endTime = sp.endTime
		}
		if !seen[sp.service] {
			seen[sp.service] = true
			summary.services = append(summary.services, sp.service)
		}
		switch sp.status {
		case "error":
			summary.status = "error"
		case "ok":
			if summary.status != "error" {
				summary.status = "ok"
			}
		}
	}
	sort.Strings(summary.services)
	return summary
}

// summaryComesAfter reports whether s sorts strictly after the position
// (start, traceID) in result order (start_time descending, trace_id
// ascending).
func summaryComesAfter(s *traceSummary, start time.Time, traceID string) bool {
	if s.startTime.Equal(start) {
		return s.traceID > traceID
	}
	return s.startTime.Before(start)
}

// ---- JSON wire shapes ------------------------------------------------------

type traceSummaryJSON struct {
	TraceID   string   `json:"trace_id"`
	StartTime string   `json:"start_time"`
	EndTime   string   `json:"end_time"`
	SpanCount int      `json:"span_count"`
	Services  []string `json:"services"`
	Status    string   `json:"status"`
}

func (s *traceSummary) wireJSON() traceSummaryJSON {
	return traceSummaryJSON{
		TraceID:   s.traceID,
		StartTime: s.startTime.UTC().Format(time.RFC3339Nano),
		EndTime:   s.endTime.UTC().Format(time.RFC3339Nano),
		SpanCount: s.spanCount,
		Services:  append([]string(nil), s.services...),
		Status:    s.status,
	}
}

// ---- cursor ----------------------------------------------------------------

// traceCursor pins the first request's conditions, page size and snapshot
// upper bound, plus the position the next page continues from.
type traceCursor struct {
	V         int    `json:"v"`
	Tenant    string `json:"tenant"`
	Start     string `json:"start"`
	End       string `json:"end"`
	Service   string `json:"service,omitempty"`
	Name      string `json:"name,omitempty"`
	Status    string `json:"status,omitempty"`
	Limit     int    `json:"limit"`
	Snapshot  int64  `json:"snapshot"`
	LastStart string `json:"last_start,omitempty"`
	LastTrace string `json:"last_trace,omitempty"`
}

func encodeTraceCursor(c *traceCursor) string {
	payload, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, cursorKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// decodeTraceCursor parses and validates a cursor token. The boolean is
// false for any malformed, tampered or otherwise unusable token.
func decodeTraceCursor(token string) (*traceCursor, bool) {
	payloadPart, macPart, ok := strings.Cut(token, ".")
	if !ok {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return nil, false
	}
	wantMAC, err := base64.RawURLEncoding.DecodeString(macPart)
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, cursorKey)
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), wantMAC) {
		return nil, false
	}
	var c traceCursor
	if !strictDecode(payload, &c) {
		return nil, false
	}
	if c.V != 1 || c.Tenant == "" || c.Limit < 1 || c.Limit > maxTraceLimit || c.Snapshot < 0 {
		return nil, false
	}
	start, err := time.Parse(time.RFC3339Nano, c.Start)
	if err != nil {
		return nil, false
	}
	end, err := time.Parse(time.RFC3339Nano, c.End)
	if err != nil || !start.Before(end) {
		return nil, false
	}
	if c.Status != "" && !spanStatuses[c.Status] {
		return nil, false
	}
	if c.LastStart != "" {
		if _, err := time.Parse(time.RFC3339Nano, c.LastStart); err != nil {
			return nil, false
		}
	}
	if c.LastTrace != "" && !validTraceID(c.LastTrace) {
		return nil, false
	}
	return &c, true
}

// ---- HTTP handlers ---------------------------------------------------------

func handleTracesGet(w http.ResponseWriter, r *http.Request) {
	store := tenantStore(r)
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeAPIError(w, "invalid_trace_query", http.StatusBadRequest)
		return
	}

	if cursors, present := values["cursor"]; present {
		// A follow-up request carries exactly one cursor and nothing else;
		// anything else — extra parameters, a repeated cursor, a malformed,
		// tampered or foreign token — is a cursor error.
		if len(values) != 1 || len(cursors) != 1 {
			writeAPIError(w, "invalid_trace_cursor", http.StatusBadRequest)
			return
		}
		cursor, ok := decodeTraceCursor(cursors[0])
		if !ok || cursor.Tenant != tenantName(r) {
			writeAPIError(w, "invalid_trace_cursor", http.StatusBadRequest)
			return
		}
		serveTracePage(w, store, cursor)
		return
	}

	filter, limit, ok := parseTraceQuery(values)
	if !ok {
		writeAPIError(w, "invalid_trace_query", http.StatusBadRequest)
		return
	}

	store.mu.RLock()
	snapshot := store.spanSeq
	store.mu.RUnlock()

	cursor := &traceCursor{
		V:        1,
		Tenant:   tenantName(r),
		Start:    filter.start.UTC().Format(time.RFC3339Nano),
		End:      filter.end.UTC().Format(time.RFC3339Nano),
		Service:  filter.service,
		Name:     filter.name,
		Status:   filter.status,
		Limit:    limit,
		Snapshot: snapshot,
	}
	serveTracePage(w, store, cursor)
}

// serveTracePage runs the paginated search described by cursor and writes
// the response, refreshing the cursor's position for the next page.
func serveTracePage(w http.ResponseWriter, store *metricStore, cursor *traceCursor) {
	start, _ := time.Parse(time.RFC3339Nano, cursor.Start)
	end, _ := time.Parse(time.RFC3339Nano, cursor.End)
	filter := &traceFilter{
		start:   start,
		end:     end,
		service: cursor.Service,
		name:    cursor.Name,
		status:  cursor.Status,
	}

	var afterStart time.Time
	hasAfter := cursor.LastStart != ""
	if hasAfter {
		afterStart, _ = time.Parse(time.RFC3339Nano, cursor.LastStart)
	}

	page, more := store.queryTraces(filter, cursor.Snapshot, afterStart, cursor.LastTrace, hasAfter, cursor.Limit)

	traces := make([]traceSummaryJSON, 0, len(page))
	for _, summary := range page {
		traces = append(traces, summary.wireJSON())
	}

	var nextCursor *string
	if more {
		last := page[len(page)-1]
		cursor.LastStart = last.startTime.UTC().Format(time.RFC3339Nano)
		cursor.LastTrace = last.traceID
		token := encodeTraceCursor(cursor)
		nextCursor = &token
	}

	writeJSON(w, http.StatusOK, map[string]any{"traces": traces, "next_cursor": nextCursor})
}

// parseTraceQuery validates the first-request query parameters and builds
// the filter. start and end are each required exactly once; service, name,
// status and limit are each optional at most once; every other parameter,
// any repeat and any illegal value is rejected.
func parseTraceQuery(values url.Values) (*traceFilter, int, bool) {
	filter := &traceFilter{}
	limit := defaultTraceLimit
	var hasStart, hasEnd bool

	for key, vals := range values {
		if len(vals) != 1 {
			return nil, 0, false
		}
		value := vals[0]
		switch key {
		case "start":
			ts, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return nil, 0, false
			}
			filter.start, hasStart = ts, true
		case "end":
			ts, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return nil, 0, false
			}
			filter.end, hasEnd = ts, true
		case "service":
			if value == "" {
				return nil, 0, false
			}
			filter.service = value
		case "name":
			if value == "" {
				return nil, 0, false
			}
			filter.name = value
		case "status":
			if !spanStatuses[value] {
				return nil, 0, false
			}
			filter.status = value
		case "limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > maxTraceLimit {
				return nil, 0, false
			}
			limit = n
		default:
			return nil, 0, false
		}
	}
	if !hasStart || !hasEnd || !filter.start.Before(filter.end) {
		return nil, 0, false
	}
	return filter, limit, true
}
