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
	"time"
)

const (
	defaultTraceLimit = 100
	maxTraceLimit     = 200
)

// traceFilter is the immutable, signed condition set of a trace search. Both
// time bounds are mandatory; service/name/status are empty when not requested.
type traceFilter struct {
	start   time.Time
	end     time.Time
	service string
	name    string
	status  string
}

// spanFeatureMatch reports whether sp alone satisfies every requested span
// feature. The multiple feature conditions must hold for the same span rather
// than being spread across spans of the trace.
func (f *traceFilter) spanFeatureMatch(sp *span) bool {
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

// traceAccumulator is the per-trace fold of retained spans.
type traceAccumulator struct {
	start        time.Time
	end          time.Time
	count        int
	services     map[string]struct{}
	hasOK        bool
	hasError     bool
	featureMatch bool
}

// traceSummary is one assembled trace collection entry.
type traceSummary struct {
	traceID   string
	startTime time.Time
	endTime   time.Time
	spanCount int
	services  []string
	status    string
}

// queryTraces returns the page of trace summaries matching the filter within
// the snapshot (spans with seq <= snapshotSeq), strictly after the position
// (afterStart, afterTraceID) in result order, plus whether more entries
// remain. Result order is earliest span start descending, trace id ascending
// within one instant.
func (s *metricStore) queryTraces(f *traceFilter, snapshotSeq int64, afterStart time.Time, afterTraceID string, hasAfter bool, limit int) ([]traceSummary, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	accs := make(map[string]*traceAccumulator)
	for _, sp := range s.spans {
		if sp.seq > snapshotSeq {
			continue // committed after the snapshot anchor; never mixes in
		}
		acc := accs[sp.traceID]
		if acc == nil {
			acc = &traceAccumulator{
				start:    sp.startTime,
				end:      sp.endTime,
				services: make(map[string]struct{}),
			}
			accs[sp.traceID] = acc
		}
		acc.count++
		if sp.startTime.Before(acc.start) {
			acc.start = sp.startTime
		}
		if sp.endTime.After(acc.end) {
			acc.end = sp.endTime
		}
		acc.services[sp.service] = struct{}{}
		switch sp.status {
		case "error":
			acc.hasError = true
		case "ok":
			acc.hasOK = true
		}
		if f.spanFeatureMatch(sp) {
			acc.featureMatch = true
		}
	}

	matched := make([]traceSummary, 0)
	for traceID, acc := range accs {
		// The trace time is the earliest stored span start; it must fall into
		// the half-open [start, end) window.
		if acc.start.Before(f.start) || !acc.start.Before(f.end) {
			continue
		}
		if !acc.featureMatch {
			continue
		}
		if hasAfter && !summaryComesAfter(acc.start, traceID, afterStart, afterTraceID) {
			continue
		}
		services := make([]string, 0, len(acc.services))
		for svc := range acc.services {
			services = append(services, svc)
		}
		sort.Strings(services)
		status := "unset"
		switch {
		case acc.hasError:
			status = "error"
		case acc.hasOK:
			status = "ok"
		}
		matched = append(matched, traceSummary{
			traceID:   traceID,
			startTime: acc.start,
			endTime:   acc.end,
			spanCount: acc.count,
			services:  services,
			status:    status,
		})
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

// summaryComesAfter reports whether an entry at (start, traceID) sorts strictly
// after the page position in start-descending, trace-id-ascending order.
func summaryComesAfter(start time.Time, traceID string, afterStart time.Time, afterTraceID string) bool {
	if start.Equal(afterStart) {
		return traceID > afterTraceID
	}
	return start.Before(afterStart)
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

// decodeTraceCursor parses and validates a trace cursor token. The boolean is
// false for any malformed, tampered or otherwise unusable token.
func decodeTraceCursor(token string) (*traceCursor, bool) {
	payload, _, ok := parseCursorToken(token)
	if !ok {
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
	if err != nil {
		return nil, false
	}
	if !start.Before(end) {
		return nil, false
	}
	if c.Status != "" && !spanStatuses[c.Status] {
		return nil, false
	}
	switch {
	case c.LastStart == "" && c.LastTrace == "":
		// First page position: neither field is set.
	case c.LastStart != "" && c.LastTrace != "":
		if _, err := time.Parse(time.RFC3339Nano, c.LastStart); err != nil {
			return nil, false
		}
		if !validTraceID(c.LastTrace) {
			return nil, false
		}
	default:
		return nil, false // position fields must appear together
	}
	return &c, true
}

// ---- HTTP handlers ---------------------------------------------------------

func handleTracesGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
		return
	}

	store := tenantStore(r)
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeAPIError(w, "invalid_trace_query", http.StatusBadRequest)
		return
	}

	if cursors, present := values["cursor"]; present {
		// A follow-up request carries exactly one cursor and nothing else.
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

// serveTracePage runs the paginated query described by cursor and writes the
// response, refreshing the cursor's position for the next page.
func serveTracePage(w http.ResponseWriter, store *metricStore, cursor *traceCursor) {
	// Both bounds were validated when the cursor was minted or decoded.
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
	for _, sum := range page {
		traces = append(traces, sum.wireJSON())
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

// parseTraceQuery validates the first-request query parameters and builds the
// filter. start and end are mandatory and unique; service, name, status and
// limit are optional but may appear at most once; every other name is unknown.
func parseTraceQuery(values url.Values) (*traceFilter, int, bool) {
	filter := &traceFilter{}
	limit := defaultTraceLimit
	hasStart, hasEnd := false, false

	for key, vals := range values {
		// Every accepted parameter is single-valued: duplicates are invalid.
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

// ---- JSON wire shapes ------------------------------------------------------

type traceSummaryJSON struct {
	TraceID   string   `json:"trace_id"`
	StartTime string   `json:"start_time"`
	EndTime   string   `json:"end_time"`
	SpanCount int      `json:"span_count"`
	Services  []string `json:"services"`
	Status    string   `json:"status"`
}

func (s traceSummary) wireJSON() traceSummaryJSON {
	return traceSummaryJSON{
		TraceID:   s.traceID,
		StartTime: s.startTime.UTC().Format(time.RFC3339Nano),
		EndTime:   s.endTime.UTC().Format(time.RFC3339Nano),
		SpanCount: s.spanCount,
		Services:  s.services,
		Status:    s.status,
	}
}
