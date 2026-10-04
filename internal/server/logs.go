package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	logsPath = "/api/v1/logs"

	maxLogBatch        = 500
	maxLogIDsPerTenant = 10000
	maxLogIDLen        = 128
	defaultLogLimit    = 100
	maxLogLimit        = 200
)

var logLevels = map[string]bool{
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
}

// logEntry is one committed log record. Entries are immutable once stored;
// only eviction removes them.
type logEntry struct {
	id        string
	timestamp time.Time
	level     string
	message   string
	labels    map[string]string
	traceID   string // "" when absent
	seq       int64  // commit sequence, assigned at insert
}

// sameContent reports whether two entries with the same id carry the same
// timestamp instant and the same remaining content, making the later one a
// replay rather than a conflict.
func (e *logEntry) sameContent(other *logEntry) bool {
	return e.id == other.id &&
		e.timestamp.Equal(other.timestamp) &&
		e.level == other.level &&
		e.message == other.message &&
		e.traceID == other.traceID &&
		maps.Equal(e.labels, other.labels)
}

// checkAndApplyLogs validates the batch against committed state (and against
// itself) and then commits it in array order. A false result signals a
// content conflict on a repeated id; in that case no state is mutated.
// Replays — repeats of an id with identical content — are counted but do not
// mutate state or refresh eviction order. Sampling runs only after every
// format, duplicate and conflict check passes: a not-yet-retained legal entry
// is kept or dropped deterministically by log id, or by trace id under
// trace_rate when it carries one. Dropped entries consume no id, capacity or
// commit sequence.
func (s *metricStore) checkAndApplyLogs(batch []*logEntry) (accepted, replayed, sampledOut int, samplingActive, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// First pass: conflict detection against committed entries and earlier
	// entries in the same batch.
	seen := make(map[string]*logEntry, len(batch))
	for _, e := range batch {
		if prev, dup := seen[e.id]; dup {
			if !prev.sameContent(e) {
				return 0, 0, 0, false, false
			}
			continue
		}
		if existing, committed := s.logs[e.id]; committed && !existing.sameContent(e) {
			return 0, 0, 0, false, false
		}
		seen[e.id] = e
	}

	policy := s.policyLocked()
	samplingActive = policy.logRate < 1 || policy.traceRate < 1

	// Second pass: commit in array order, then evict the oldest ids until
	// the tenant is back within the retention bound.
	for _, e := range batch {
		if _, exists := s.logs[e.id]; exists {
			replayed++
			continue
		}
		// An already retained identity always replays; only fresh identities
		// are sampled. A trace id groups the log with the trace's spans.
		rate, identity := policy.logRate, e.id
		if e.traceID != "" {
			rate, identity = policy.traceRate, e.traceID
		}
		if !sampleIn(identity, rate) {
			sampledOut++
			continue
		}
		s.logSeq++
		e.seq = s.logSeq
		s.logs[e.id] = e
		s.logOrder = append(s.logOrder, e.id)
		accepted++
	}
	if len(s.logOrder) > maxLogIDsPerTenant {
		excess := len(s.logOrder) - maxLogIDsPerTenant
		for _, id := range s.logOrder[:excess] {
			delete(s.logs, id)
		}
		s.logOrder = append([]string(nil), s.logOrder[excess:]...)
	}
	return accepted, replayed, sampledOut, samplingActive, true
}

// logFilter is the immutable condition set of a log query. A nil entry in
// hasStart/hasEnd marks an open bound.
type logFilter struct {
	start    time.Time
	hasStart bool
	end      time.Time
	hasEnd   bool
	level    string
	traceID  string
	q        string
	labels   map[string]string
}

func (f *logFilter) matches(e *logEntry) bool {
	if f.hasStart && e.timestamp.Before(f.start) {
		return false
	}
	if f.hasEnd && !e.timestamp.Before(f.end) {
		return false
	}
	if f.level != "" && e.level != f.level {
		return false
	}
	if f.traceID != "" && e.traceID != f.traceID {
		return false
	}
	if f.q != "" && !strings.Contains(e.message, f.q) {
		return false
	}
	for k, v := range f.labels {
		got, ok := e.labels[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

// queryLogs returns the page of entries matching the filter within the
// snapshot (entries with seq <= snapshotSeq), strictly after the position
// (afterTS, afterID) in result order, plus whether more entries remain.
// Result order is timestamp descending, id ascending within one instant.
func (s *metricStore) queryLogs(f *logFilter, snapshotSeq int64, afterTS time.Time, afterID string, hasAfter bool, limit int) ([]*logEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	matched := make([]*logEntry, 0)
	for _, e := range s.logs {
		if e.seq > snapshotSeq {
			continue // written after the snapshot anchor; never mixes in
		}
		if !f.matches(e) {
			continue
		}
		if hasAfter && !comesAfter(e, afterTS, afterID) {
			continue
		}
		matched = append(matched, e)
	}
	sort.Slice(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if !a.timestamp.Equal(b.timestamp) {
			return a.timestamp.After(b.timestamp)
		}
		return a.id < b.id
	})

	more := len(matched) > limit
	if more {
		matched = matched[:limit]
	}
	return matched, more
}

// comesAfter reports whether e sorts strictly after the position (ts, id) in
// result order.
func comesAfter(e *logEntry, ts time.Time, id string) bool {
	if e.timestamp.Equal(ts) {
		return e.id > id
	}
	return e.timestamp.Before(ts)
}

// ---- JSON wire shapes ------------------------------------------------------

type logsEnvelope struct {
	Entries *[]json.RawMessage `json:"entries"`
}

type rawLogEntry struct {
	ID        *string            `json:"id"`
	Timestamp *string            `json:"timestamp"`
	Level     *string            `json:"level"`
	Message   *string            `json:"message"`
	Labels    *map[string]string `json:"labels"`
	TraceID   *string            `json:"trace_id"`
}

type logEntryJSON struct {
	ID        string            `json:"id"`
	Timestamp string            `json:"timestamp"`
	Level     string            `json:"level"`
	Message   string            `json:"message"`
	Labels    map[string]string `json:"labels"`
	TraceID   string            `json:"trace_id,omitempty"`
}

func (e *logEntry) wireJSON() logEntryJSON {
	return logEntryJSON{
		ID:        e.id,
		Timestamp: e.timestamp.UTC().Format(time.RFC3339Nano),
		Level:     e.level,
		Message:   e.message,
		Labels:    copyLabels(e.labels),
		TraceID:   e.traceID,
	}
}

// decodeLogBatch performs every format and value check. It never returns a
// partially validated batch.
func decodeLogBatch(body []byte) ([]*logEntry, bool) {
	var env logsEnvelope
	if !strictDecode(body, &env) || env.Entries == nil || len(*env.Entries) == 0 || len(*env.Entries) > maxLogBatch {
		return nil, false
	}

	batch := make([]*logEntry, 0, len(*env.Entries))
	for _, raw := range *env.Entries {
		var re rawLogEntry
		if !strictDecode(raw, &re) {
			return nil, false
		}
		if re.ID == nil || re.Timestamp == nil || re.Level == nil || re.Message == nil || re.Labels == nil {
			return nil, false
		}
		id := *re.ID
		if !validLogID(id) {
			return nil, false
		}
		ts, err := time.Parse(time.RFC3339Nano, *re.Timestamp)
		if err != nil {
			return nil, false
		}
		level := *re.Level
		if !logLevels[level] {
			return nil, false
		}
		labels := *re.Labels
		if labels == nil {
			return nil, false // explicit null labels
		}
		for k := range labels {
			if !identPattern.MatchString(k) {
				return nil, false
			}
		}
		traceID := ""
		if re.TraceID != nil {
			traceID = *re.TraceID
			if !validTraceID(traceID) {
				return nil, false
			}
		}
		batch = append(batch, &logEntry{
			id:        id,
			timestamp: ts,
			level:     level,
			message:   *re.Message,
			labels:    copyLabels(labels),
			traceID:   traceID,
		})
	}
	return batch, true
}

// validLogID reports whether id is 1-128 printable ASCII characters.
func validLogID(id string) bool {
	if len(id) == 0 || len(id) > maxLogIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

func validTraceID(id string) bool {
	if len(id) != 32 {
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

// ---- cursor ----------------------------------------------------------------

// logCursor pins the first request's conditions, page size and snapshot
// upper bound, plus the position the next page continues from.
type logCursor struct {
	V        int               `json:"v"`
	Tenant   string            `json:"tenant"`
	Start    string            `json:"start,omitempty"`
	End      string            `json:"end,omitempty"`
	Level    string            `json:"level,omitempty"`
	TraceID  string            `json:"trace_id,omitempty"`
	Q        string            `json:"q,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Limit    int               `json:"limit"`
	Snapshot int64             `json:"snapshot"`
	LastTS   string            `json:"last_ts"`
	LastID   string            `json:"last_id"`
}

// cursorKey signs cursors so tampered or foreign tokens are rejected as
// invalid_cursor. Log state is process-local, so a random per-process key is
// sufficient and cursors are not expected to survive restarts.
var cursorKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}()

func encodeCursor(c *logCursor) string {
	payload, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, cursorKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// decodeCursor parses and validates a cursor token. The boolean is false for
// any malformed, tampered or otherwise unusable token.
func decodeCursor(token string) (*logCursor, bool) {
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
	var c logCursor
	if !strictDecode(payload, &c) {
		return nil, false
	}
	if c.V != 1 || c.Tenant == "" || c.Limit < 1 || c.Limit > maxLogLimit || c.Snapshot < 0 {
		return nil, false
	}
	if c.Level != "" && !logLevels[c.Level] {
		return nil, false
	}
	if c.TraceID != "" && !validTraceID(c.TraceID) {
		return nil, false
	}
	for k := range c.Labels {
		if !identPattern.MatchString(k) {
			return nil, false
		}
	}
	if c.Start != "" {
		if _, err := time.Parse(time.RFC3339Nano, c.Start); err != nil {
			return nil, false
		}
	}
	if c.End != "" {
		if _, err := time.Parse(time.RFC3339Nano, c.End); err != nil {
			return nil, false
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, c.LastTS); err != nil {
		return nil, false
	}
	if !validLogID(c.LastID) {
		return nil, false
	}
	return &c, true
}

// ---- HTTP handlers ---------------------------------------------------------

func registerLogHandlers(mux *http.ServeMux) {
	mux.HandleFunc(logsPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleLogsGet(w, r)
		case http.MethodPost:
			handleLogsPost(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
		}
	})
}

func handleLogsPost(w http.ResponseWriter, r *http.Request) {
	store := tenantStore(r)
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, "invalid_logs", http.StatusBadRequest)
		return
	}

	batch, ok := decodeLogBatch(body)
	if !ok {
		writeAPIError(w, "invalid_logs", http.StatusBadRequest)
		return
	}

	accepted, replayed, sampledOut, samplingActive, applied := store.checkAndApplyLogs(batch)
	if !applied {
		writeAPIError(w, "log_conflict", http.StatusConflict)
		return
	}

	writeIngestResponse(w, accepted, replayed, sampledOut, samplingActive)
}

func handleLogsGet(w http.ResponseWriter, r *http.Request) {
	store := tenantStore(r)
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeAPIError(w, "invalid_log_query", http.StatusBadRequest)
		return
	}

	if cursors, present := values["cursor"]; present {
		// A follow-up request carries exactly one cursor and nothing else.
		if len(values) != 1 || len(cursors) != 1 {
			writeAPIError(w, "invalid_log_query", http.StatusBadRequest)
			return
		}
		cursor, ok := decodeCursor(cursors[0])
		if !ok || cursor.Tenant != tenantName(r) {
			writeAPIError(w, "invalid_cursor", http.StatusBadRequest)
			return
		}
		serveLogPage(w, store, cursor)
		return
	}

	filter, limit, ok := parseLogQuery(values)
	if !ok {
		writeAPIError(w, "invalid_log_query", http.StatusBadRequest)
		return
	}

	store.mu.RLock()
	snapshot := store.logSeq
	store.mu.RUnlock()

	cursor := &logCursor{
		V:        1,
		Tenant:   tenantName(r),
		Level:    filter.level,
		TraceID:  filter.traceID,
		Q:        filter.q,
		Labels:   filter.labels,
		Limit:    limit,
		Snapshot: snapshot,
	}
	if filter.hasStart {
		cursor.Start = filter.start.UTC().Format(time.RFC3339Nano)
	}
	if filter.hasEnd {
		cursor.End = filter.end.UTC().Format(time.RFC3339Nano)
	}
	serveLogPage(w, store, cursor)
}

// serveLogPage runs the paginated query described by cursor and writes the
// response, refreshing the cursor's position for the next page.
func serveLogPage(w http.ResponseWriter, store *metricStore, cursor *logCursor) {
	filter := &logFilter{
		level:   cursor.Level,
		traceID: cursor.TraceID,
		q:       cursor.Q,
		labels:  cursor.Labels,
	}
	if cursor.Start != "" {
		filter.start, _ = time.Parse(time.RFC3339Nano, cursor.Start)
		filter.hasStart = true
	}
	if cursor.End != "" {
		filter.end, _ = time.Parse(time.RFC3339Nano, cursor.End)
		filter.hasEnd = true
	}

	var afterTS time.Time
	hasAfter := cursor.LastTS != ""
	if hasAfter {
		afterTS, _ = time.Parse(time.RFC3339Nano, cursor.LastTS)
	}

	page, more := store.queryLogs(filter, cursor.Snapshot, afterTS, cursor.LastID, hasAfter, cursor.Limit)

	entries := make([]logEntryJSON, 0, len(page))
	for _, e := range page {
		entries = append(entries, e.wireJSON())
	}

	var nextCursor *string
	if more {
		last := page[len(page)-1]
		cursor.LastTS = last.timestamp.UTC().Format(time.RFC3339Nano)
		cursor.LastID = last.id
		token := encodeCursor(cursor)
		nextCursor = &token
	}

	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "next_cursor": nextCursor})
}

// parseLogQuery validates the first-request query parameters and builds the
// filter. Only the documented parameter vocabulary is accepted.
func parseLogQuery(values url.Values) (*logFilter, int, bool) {
	filter := &logFilter{labels: map[string]string{}}
	limit := defaultLogLimit

	for key, vals := range values {
		if len(vals) == 0 {
			return nil, 0, false
		}
		// A repeated parameter is accepted only when every value agrees.
		for _, v := range vals[1:] {
			if v != vals[0] {
				return nil, 0, false
			}
		}
		value := vals[0]
		switch {
		case key == "start":
			ts, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return nil, 0, false
			}
			filter.start, filter.hasStart = ts, true
		case key == "end":
			ts, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return nil, 0, false
			}
			filter.end, filter.hasEnd = ts, true
		case key == "level":
			if !logLevels[value] {
				return nil, 0, false
			}
			filter.level = value
		case key == "trace_id":
			if !validTraceID(value) {
				return nil, 0, false
			}
			filter.traceID = value
		case key == "q":
			filter.q = value
		case key == "limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > maxLogLimit {
				return nil, 0, false
			}
			limit = n
		case strings.HasPrefix(key, "label."):
			labelKey := strings.TrimPrefix(key, "label.")
			if !identPattern.MatchString(labelKey) {
				return nil, 0, false
			}
			filter.labels[labelKey] = value
		default:
			return nil, 0, false
		}
	}
	return filter, limit, true
}
