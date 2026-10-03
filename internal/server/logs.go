package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const logsPath = "/api/v1/logs"

const (
	logsPerBatchMax  = 500
	logsPerTenant    = 10000
	logsDefaultLimit = 100
	logsMaxLimit     = 200
)

var (
	validLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ---- stored model ----------------------------------------------------------

// logEntry is an immutable stored log record. seq is the per-tenant,
// monotonically increasing snapshot number assigned when the id is first
// accepted; replays keep the original record and seq.
type logEntry struct {
	id      string
	ts      time.Time
	level   string
	message string
	labels  map[string]string
	traceID string // empty means the optional field was absent
	seq     int64
}

// sameLogContent reports whether two records carry the same timestamp instant
// and remaining content; ids are compared by the caller.
func sameLogContent(a, b *logEntry) bool {
	if !a.ts.Equal(b.ts) || a.level != b.level || a.message != b.message || a.traceID != b.traceID {
		return false
	}
	if len(a.labels) != len(b.labels) {
		return false
	}
	for k, v := range a.labels {
		if b.labels[k] != v {
			return false
		}
	}
	return true
}

// logRing is a FIFO queue of retained log ids with bounded retained storage.
// Dead entries before head are compacted away once they reach half the slice.
type logRing struct {
	items []string
	head  int
}

func (r *logRing) push(id string) {
	if r.head > 0 && r.head >= len(r.items)/2 {
		n := copy(r.items, r.items[r.head:])
		r.items = r.items[:n]
		r.head = 0
	}
	r.items = append(r.items, id)
}

func (r *logRing) popFront() string {
	id := r.items[r.head]
	r.head++
	if r.head == len(r.items) {
		r.items = r.items[:0]
		r.head = 0
	}
	return id
}

func (r *logRing) len() int {
	return len(r.items) - r.head
}

// ---- request decoding ------------------------------------------------------

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

// decodeLogsBatch performs every structural and value check. It never returns
// a partially validated batch.
func decodeLogsBatch(body []byte) ([]*logEntry, bool) {
	var env logsEnvelope
	if !strictDecode(body, &env) || env.Entries == nil {
		return nil, false
	}
	raws := *env.Entries
	if len(raws) == 0 || len(raws) > logsPerBatchMax {
		return nil, false
	}

	batch := make([]*logEntry, 0, len(raws))
	for _, raw := range raws {
		var e rawLogEntry
		if !strictDecode(raw, &e) {
			return nil, false
		}
		if e.ID == nil || e.Timestamp == nil || e.Level == nil || e.Message == nil || e.Labels == nil {
			return nil, false
		}
		id := *e.ID
		if len(id) < 1 || len(id) > 128 || !isPrintableASCII(id) {
			return nil, false
		}
		ts, err := time.Parse(time.RFC3339Nano, *e.Timestamp)
		if err != nil {
			return nil, false
		}
		level := *e.Level
		if !validLogLevels[level] {
			return nil, false
		}
		labels := *e.Labels
		if labels == nil {
			return nil, false // explicit null labels
		}
		for k := range labels {
			if !identPattern.MatchString(k) {
				return nil, false
			}
		}
		traceID := ""
		if e.TraceID != nil {
			traceID = *e.TraceID
			if !traceIDPattern.MatchString(traceID) {
				return nil, false
			}
		}

		batch = append(batch, &logEntry{
			id:      id,
			ts:      ts,
			level:   level,
			message: *e.Message,
			labels:  copyLabels(labels),
			traceID: traceID,
		})
	}
	return batch, true
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// ---- store operations ------------------------------------------------------

// appendLogs atomically validates the batch against committed state (and
// against itself) and then commits the novel records in array order, evicting
// the oldest retained ids afterwards. A conflict leaves all state untouched.
// Replays (same id, same timestamp instant and content, including an identical
// duplicate earlier in the same batch) neither move retention order nor get a
// new snapshot number.
func (s *metricStore) appendLogs(batch []*logEntry) (accepted, replayed int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	novel := make([]*logEntry, 0, len(batch))
	novelIndex := make(map[string]int, len(batch))
	for _, e := range batch {
		if existing, exists := s.logs[e.id]; exists {
			if !sameLogContent(existing, e) {
				return 0, 0, false
			}
			replayed++
			continue
		}
		if idx, seen := novelIndex[e.id]; seen {
			// Duplicate id inside the batch is judged exactly like an
			// existing id: identical content replays, anything else conflicts.
			if !sameLogContent(novel[idx], e) {
				return 0, 0, false
			}
			replayed++
			continue
		}
		novelIndex[e.id] = len(novel)
		novel = append(novel, e)
	}

	for _, e := range novel {
		s.logSeq++
		e.seq = s.logSeq
		s.logs[e.id] = e
		s.logRing.push(e.id)
	}
	for s.logRing.len() > logsPerTenant {
		delete(s.logs, s.logRing.popFront())
	}
	return len(novel), replayed, true
}

// logFilter is the immutable first-request filter carried by cursors.
type logFilter struct {
	hasStart bool
	start    time.Time // inclusive
	hasEnd   bool
	end      time.Time // exclusive
	levels   map[string]bool
	hasTrace bool
	traceID  string
	hasQ     bool
	q        string
	labels   map[string]string
}

func (f logFilter) matches(e *logEntry) bool {
	if f.hasStart && e.ts.Before(f.start) {
		return false
	}
	if f.hasEnd && !e.ts.Before(f.end) {
		return false
	}
	if len(f.levels) > 0 && !f.levels[e.level] {
		return false
	}
	if f.hasTrace && e.traceID != f.traceID {
		return false
	}
	if f.hasQ && !strings.Contains(e.message, f.q) {
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

// scanLogs returns copies of every retained entry with seq <= seqMax that
// matches, ordered by timestamp descending and then id ascending. The bound
// comes from the first page of a query so later writes never enter a paged
// result set.
func (s *metricStore) scanLogs(f logFilter, seqMax int64) []*logEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*logEntry, 0)
	for _, e := range s.logs {
		if e.seq > seqMax || !f.matches(e) {
			continue
		}
		cp := *e
		cp.labels = copyLabels(e.labels)
		out = append(out, &cp)
	}
	sortLogs(out)
	return out
}

// currentLogSeq returns the current snapshot upper bound.
func (s *metricStore) currentLogSeq() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.logSeq
}

func sortLogs(out []*logEntry) {
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ts.Equal(out[j].ts) {
			// Newer instant first; time.Time comparisons stay exact outside
			// the range representable by UnixNano.
			return out[j].ts.Before(out[i].ts)
		}
		return out[i].id < out[j].id
	})
}

// ---- wire shapes ------------------------------------------------------------

type logEntryJSON struct {
	ID        string            `json:"id"`
	Timestamp string            `json:"timestamp"`
	Level     string            `json:"level"`
	Message   string            `json:"message"`
	Labels    map[string]string `json:"labels"`
	TraceID   string            `json:"trace_id,omitempty"`
}

func logWireJSON(e *logEntry) logEntryJSON {
	return logEntryJSON{
		ID:        e.id,
		Timestamp: e.ts.UTC().Format(time.RFC3339Nano),
		Level:     e.level,
		Message:   e.message,
		Labels:    copyLabels(e.labels),
		TraceID:   e.traceID,
	}
}

// ---- cursors ----------------------------------------------------------------

// pageCursor is the opaque, authenticated pagination state. It pins the
// tenant, the first-request filter, the page size, the snapshot upper bound
// and the keyset position (last returned timestamp/id).
type pageCursor struct {
	Tenant   string            `json:"t"`
	HasStart bool              `json:"hs,omitempty"`
	Start    time.Time         `json:"s,omitempty"`
	HasEnd   bool              `json:"he,omitempty"`
	End      time.Time         `json:"e,omitempty"`
	Levels   []string          `json:"lv,omitempty"`
	HasTrace bool              `json:"ht,omitempty"`
	TraceID  string            `json:"tr,omitempty"`
	HasQ     bool              `json:"hq,omitempty"`
	Q        string            `json:"q,omitempty"`
	Labels   map[string]string `json:"lb,omitempty"`
	Limit    int               `json:"l"`
	SeqMax   int64             `json:"m"`
	HasAfter bool              `json:"ha,omitempty"`
	AfterTS  time.Time         `json:"at,omitempty"`
	AfterID  string            `json:"ai,omitempty"`
}

var (
	logCursorKeyOnce sync.Once
	logCursorKey     []byte
)

func cursorSigningKey() []byte {
	logCursorKeyOnce.Do(func() {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			panic(err)
		}
		logCursorKey = k
	})
	return logCursorKey
}

func encodeCursor(c *pageCursor) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, cursorSigningKey())
	mac.Write(raw)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig, nil
}

// decodeCursor parses and authenticates a cursor token. Structural sanity is
// checked here as well; any failure means invalid_cursor.
func decodeCursor(token string) (*pageCursor, bool) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || payload == "" || sig == "" {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, false
	}
	wantSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, cursorSigningKey())
	mac.Write(raw)
	if !hmac.Equal(mac.Sum(nil), wantSig) {
		return nil, false
	}
	var c pageCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, false
	}
	if !tenantPattern.MatchString(c.Tenant) || c.Limit < 1 || c.Limit > logsMaxLimit || c.SeqMax < 0 {
		return nil, false
	}
	for _, lv := range c.Levels {
		if !validLogLevels[lv] {
			return nil, false
		}
	}
	if c.HasTrace && !traceIDPattern.MatchString(c.TraceID) {
		return nil, false
	}
	for k := range c.Labels {
		if !identPattern.MatchString(k) {
			return nil, false
		}
	}
	if c.HasAfter && (c.AfterID == "" || len(c.AfterID) > 128 || !isPrintableASCII(c.AfterID)) {
		return nil, false
	}
	return &c, true
}

func (c *pageCursor) filter() logFilter {
	f := logFilter{
		hasStart: c.HasStart,
		start:    c.Start,
		hasEnd:   c.HasEnd,
		end:      c.End,
		hasTrace: c.HasTrace,
		traceID:  c.TraceID,
		hasQ:     c.HasQ,
		q:        c.Q,
		labels:   c.Labels,
	}
	if len(c.Levels) > 0 {
		f.levels = make(map[string]bool, len(c.Levels))
		for _, lv := range c.Levels {
			f.levels[lv] = true
		}
	}
	return f
}

// ---- HTTP handlers ----------------------------------------------------------

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

	batch, ok := decodeLogsBatch(body)
	if !ok {
		writeAPIError(w, "invalid_logs", http.StatusBadRequest)
		return
	}

	accepted, replayed, committed := tenantStore(r).appendLogs(batch)
	if !committed {
		writeAPIError(w, "log_conflict", http.StatusConflict)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]int{"accepted": accepted, "replayed": replayed})
}

func handleLogsGet(w http.ResponseWriter, r *http.Request) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeAPIError(w, "invalid_log_query", http.StatusBadRequest)
		return
	}

	store := tenantStore(r)
	tenant := tenantName(r)

	var cur *pageCursor
	if tokens := values["cursor"]; len(tokens) > 0 {
		if len(tokens) != 1 {
			writeAPIError(w, "invalid_cursor", http.StatusBadRequest)
			return
		}
		// A valid paged request carries the cursor alone; combining it with
		// any other parameter is an invalid query, not a broken cursor.
		if len(values) != 1 {
			writeAPIError(w, "invalid_log_query", http.StatusBadRequest)
			return
		}
		var valid bool
		cur, valid = decodeCursor(tokens[0])
		if !valid {
			writeAPIError(w, "invalid_cursor", http.StatusBadRequest)
			return
		}
		if cur.Tenant != tenant {
			writeAPIError(w, "invalid_cursor", http.StatusBadRequest)
			return
		}
	} else {
		filter, limit, valid := parseLogFilter(values)
		if !valid {
			writeAPIError(w, "invalid_log_query", http.StatusBadRequest)
			return
		}
		seqMax := store.currentLogSeq()
		cur = &pageCursor{
			Tenant: tenant,
			Limit:  limit,
			SeqMax: seqMax,
		}
		if filter.hasStart {
			cur.HasStart, cur.Start = true, filter.start
		}
		if filter.hasEnd {
			cur.HasEnd, cur.End = true, filter.end
		}
		if len(filter.levels) > 0 {
			cur.Levels = make([]string, 0, len(filter.levels))
			for lv := range filter.levels {
				cur.Levels = append(cur.Levels, lv)
			}
			sort.Strings(cur.Levels)
		}
		if filter.hasTrace {
			cur.HasTrace, cur.TraceID = true, filter.traceID
		}
		if filter.hasQ {
			cur.HasQ, cur.Q = true, filter.q
		}
		if len(filter.labels) > 0 {
			cur.Labels = copyLabels(filter.labels)
		}
	}

	rows := store.scanLogs(cur.filter(), cur.SeqMax)

	start := 0
	if cur.HasAfter {
		// Skip every row sorting at or before the keyset position: same
		// instant with id <= the last returned id, or any earlier instant.
		for start < len(rows) {
			n := rows[start].ts
			if n.Before(cur.AfterTS) {
				break
			}
			if n.Equal(cur.AfterTS) && rows[start].id > cur.AfterID {
				break
			}
			start++
		}
	}
	end := start + cur.Limit
	if end > len(rows) {
		end = len(rows)
	}
	page := rows[start:end]

	entries := make([]logEntryJSON, 0, len(page))
	for _, e := range page {
		entries = append(entries, logWireJSON(e))
	}

	var nextCursor any
	if end < len(rows) {
		last := page[len(page)-1]
		cur.HasAfter = true
		cur.AfterTS = last.ts
		cur.AfterID = last.id
		token, err := encodeCursor(cur)
		if err != nil {
			writeAPIError(w, "invalid_log_query", http.StatusBadRequest)
			return
		}
		nextCursor = token
	}

	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "next_cursor": nextCursor})
}

// parseLogFilter parses the first-request query parameters. Scalar parameters
// may appear at most once; label.<key> may repeat only with the same value,
// matching the metrics query convention.
func parseLogFilter(values url.Values) (logFilter, int, bool) {
	f := logFilter{}
	limit := logsDefaultLimit
	for key, vals := range values {
		switch {
		case key == "start" || key == "end" || key == "level" || key == "trace_id" ||
			key == "q" || key == "limit":
			if len(vals) != 1 {
				return f, 0, false
			}
		case strings.HasPrefix(key, "label."):
			labelKey := strings.TrimPrefix(key, "label.")
			if !identPattern.MatchString(labelKey) {
				return f, 0, false
			}
			for _, v := range vals[1:] {
				if v != vals[0] {
					return f, 0, false
				}
			}
		default:
			return f, 0, false
		}
	}

	if sv, ok := values["start"]; ok {
		t, err := time.Parse(time.RFC3339Nano, sv[0])
		if err != nil {
			return f, 0, false
		}
		f.hasStart, f.start = true, t.UTC()
	}
	if ev, ok := values["end"]; ok {
		t, err := time.Parse(time.RFC3339Nano, ev[0])
		if err != nil {
			return f, 0, false
		}
		f.hasEnd, f.end = true, t.UTC()
	}
	if lv := values["level"]; len(lv) > 0 {
		if !validLogLevels[lv[0]] {
			return f, 0, false
		}
		f.levels = map[string]bool{lv[0]: true}
	}
	if tv, ok := values["trace_id"]; ok {
		if !traceIDPattern.MatchString(tv[0]) {
			return f, 0, false
		}
		f.hasTrace, f.traceID = true, tv[0]
	}
	if qs, ok := values["q"]; ok {
		f.hasQ, f.q = true, qs[0]
	}
	if len(values["limit"]) > 0 {
		n, err := strconv.Atoi(values["limit"][0])
		if err != nil || n < 1 || n > logsMaxLimit {
			return f, 0, false
		}
		limit = n
	}
	if labelVals := valuesWithPrefix(values, "label."); len(labelVals) > 0 {
		f.labels = make(map[string]string, len(labelVals))
		for k, v := range labelVals {
			f.labels[k] = v
		}
	}
	return f, limit, true
}

func valuesWithPrefix(values url.Values, prefix string) map[string]string {
	out := make(map[string]string)
	for key, vals := range values {
		if strings.HasPrefix(key, prefix) {
			out[strings.TrimPrefix(key, prefix)] = vals[0]
		}
	}
	return out
}
