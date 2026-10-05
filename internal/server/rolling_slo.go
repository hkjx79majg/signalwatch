package server

import (
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	rollingSLOsPath       = "/api/v1/rolling-slos"
	rollingSLORecordsPath = rollingSLOsPath + "/records"
	rollingSLOReportPath  = rollingSLOsPath + "/report"
)

// rollingSLO is a request-based rolling SLO definition plus its append-only
// record log. Definition fields never change after registration and records
// are only appended, so a report already handed out can never be mutated by
// later writes.
type rollingSLO struct {
	name      string
	objective float64
	window    time.Duration
	records   []rollingSLORecord
}

// rollingSLORecord is one committed batch observation: total requests and
// failed requests attributed to a single timezone-aware instant.
type rollingSLORecord struct {
	ts     time.Time
	total  int64
	failed int64
}

// ---- JSON wire shapes ------------------------------------------------------

type rollingSLOJSON struct {
	Name          string  `json:"name"`
	Objective     float64 `json:"objective"`
	WindowSeconds float64 `json:"window_seconds"`
}

func rollingSLOWireJSON(d *rollingSLO) rollingSLOJSON {
	return rollingSLOJSON{
		Name:          d.name,
		Objective:     d.objective,
		WindowSeconds: float64(d.window) / float64(time.Second),
	}
}

// ---- request decoding ------------------------------------------------------

type rawRollingSLO struct {
	Name          *string  `json:"name"`
	Objective     *float64 `json:"objective"`
	WindowSeconds *float64 `json:"window_seconds"`
}

// decodeRollingSLO parses and fully validates a definition body. It never
// returns a partially validated definition.
func decodeRollingSLO(body []byte) (name string, objective float64, window time.Duration, ok bool) {
	var raw rawRollingSLO
	if !strictDecode(body, &raw) {
		return "", 0, 0, false
	}
	if raw.Name == nil || raw.Objective == nil || raw.WindowSeconds == nil {
		return "", 0, 0, false
	}
	name = *raw.Name
	if strings.TrimSpace(name) == "" {
		return "", 0, 0, false
	}
	objective = *raw.Objective
	if math.IsNaN(objective) || math.IsInf(objective, 0) || objective <= 0 || objective >= 1 {
		return "", 0, 0, false
	}
	seconds := *raw.WindowSeconds
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 ||
		seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return "", 0, 0, false
	}
	window = time.Duration(seconds * float64(time.Second))
	if window <= 0 {
		// A positive but sub-nanosecond window truncates to zero.
		return "", 0, 0, false
	}
	return name, objective, window, true
}

type rawRollingSLORecord struct {
	Name      *string  `json:"name"`
	Timestamp *string  `json:"timestamp"`
	Total     *float64 `json:"total"`
	Failed    *float64 `json:"failed"`
}

// decodeRollingSLORecord parses and fully validates one record body. The
// timestamp must be timezone-aware (RFC3339Nano always carries an offset),
// total a non-negative integer and failed an integer in [0, total]; nothing
// is returned unless every check passes, so a failed write leaves no partial
// data behind.
func decodeRollingSLORecord(body []byte) (name string, rec rollingSLORecord, ok bool) {
	var raw rawRollingSLORecord
	if !strictDecode(body, &raw) {
		return "", rollingSLORecord{}, false
	}
	if raw.Name == nil || raw.Timestamp == nil || raw.Total == nil || raw.Failed == nil {
		return "", rollingSLORecord{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, *raw.Timestamp)
	if err != nil {
		return "", rollingSLORecord{}, false
	}
	total, ok := nonNegativeInteger(*raw.Total)
	if !ok {
		return "", rollingSLORecord{}, false
	}
	failed, ok := nonNegativeInteger(*raw.Failed)
	if !ok || failed > total {
		return "", rollingSLORecord{}, false
	}
	return *raw.Name, rollingSLORecord{ts: ts, total: total, failed: failed}, true
}

// nonNegativeInteger accepts only finite, integral, non-negative values that
// fit in an int64.
func nonNegativeInteger(v float64) (int64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v != math.Trunc(v) || v >= 9223372036854775808.0 {
		return 0, false
	}
	return int64(v), true
}

// ---- store operations ------------------------------------------------------

// registerRollingSLO adds a new definition. It returns false when the name is
// already registered; registration is create-only and never replaces.
func (s *metricStore) registerRollingSLO(def *rollingSLO) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.rollingSLOs[def.name]; exists {
		return false
	}
	s.rollingSLOs[def.name] = def
	return true
}

// snapshotRollingSLOs returns all definitions sorted by name. Definition
// objects are immutable, so the pointers stay safe after the lock is
// released.
func (s *metricStore) snapshotRollingSLOs() []*rollingSLO {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*rollingSLO, 0, len(s.rollingSLOs))
	for _, d := range s.rollingSLOs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// appendRollingSLORecord commits one validated record to the named
// definition. Records with the same instant accumulate, and out-of-order
// instants are stored as-is because reports filter by absolute time.
func (s *metricStore) appendRollingSLORecord(name string, rec rollingSLORecord) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	def, ok := s.rollingSLOs[name]
	if !ok {
		return false
	}
	def.records = append(def.records, rec)
	return true
}

// ---- report evaluation -----------------------------------------------------

type rollingSLOReport struct {
	Name                      string  `json:"name"`
	Objective                 float64 `json:"objective"`
	WindowSeconds             float64 `json:"window_seconds"`
	WindowStart               string  `json:"window_start"`
	WindowEnd                 string  `json:"window_end"`
	TotalRequests             int64   `json:"total_requests"`
	FailedRequests            int64   `json:"failed_requests"`
	SuccessRate               float64 `json:"success_rate"`
	AllowedFailures           float64 `json:"allowed_failures"`
	ErrorBudgetRemainingRatio float64 `json:"error_budget_remaining_ratio"`
	BurnRate                  float64 `json:"burn_rate"`
}

// rollingSLOReport computes the report for the rolling window (at-window,
// at]: the start is exclusive, the end is inclusive, and records later than
// at are never counted. All instants are compared absolutely, so equal
// moments written with different offsets behave identically. The result is a
// fresh value computed under the lock; later writes cannot mutate it.
func (s *metricStore) rollingSLOReport(name string, at time.Time) (rollingSLOReport, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	def, ok := s.rollingSLOs[name]
	if !ok {
		return rollingSLOReport{}, false
	}

	start := at.Add(-def.window)
	var total, failed int64
	for _, rec := range def.records {
		if rec.ts.After(start) && !rec.ts.After(at) {
			total += rec.total
			failed += rec.failed
		}
	}

	report := rollingSLOReport{
		Name:           def.name,
		Objective:      def.objective,
		WindowSeconds:  float64(def.window) / float64(time.Second),
		WindowStart:    start.UTC().Format(time.RFC3339Nano),
		WindowEnd:      at.UTC().Format(time.RFC3339Nano),
		TotalRequests:  total,
		FailedRequests: failed,
	}
	if total == 0 {
		// No requests in the window: perfect success rate and a fully
		// remaining error budget, nothing burning.
		report.SuccessRate = 1
		report.ErrorBudgetRemainingRatio = 1
		return report, true
	}

	failureRate := float64(failed) / float64(total)
	allowedRate := 1 - def.objective
	allowed := float64(total) * allowedRate
	remaining := 1 - float64(failed)/allowed
	if remaining < 0 {
		remaining = 0
	}
	report.SuccessRate = 1 - failureRate
	report.AllowedFailures = allowed
	report.ErrorBudgetRemainingRatio = remaining
	report.BurnRate = failureRate / allowedRate
	return report, true
}

// ---- HTTP handlers ---------------------------------------------------------

func registerRollingSLOHandlers(mux *http.ServeMux) {
	mux.HandleFunc(rollingSLOsPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			defs := tenantStore(r).snapshotRollingSLOs()
			body := make([]rollingSLOJSON, 0, len(defs))
			for _, d := range defs {
				body = append(body, rollingSLOWireJSON(d))
			}
			writeJSON(w, http.StatusOK, map[string]any{"slos": body})
		case http.MethodPost:
			handleRollingSLOCreate(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(rollingSLORecordsPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		handleRollingSLORecord(w, r)
	})

	mux.HandleFunc(rollingSLOReportPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		handleRollingSLOReport(w, r)
	})
}

func handleRollingSLOCreate(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, "invalid_rolling_slo", http.StatusBadRequest)
		return
	}

	name, objective, window, ok := decodeRollingSLO(body)
	if !ok {
		writeAPIError(w, "invalid_rolling_slo", http.StatusBadRequest)
		return
	}

	def := &rollingSLO{name: name, objective: objective, window: window}
	if !tenantStore(r).registerRollingSLO(def) {
		// Re-registering an existing name is a client error, never a replace.
		writeAPIError(w, "invalid_rolling_slo", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, rollingSLOWireJSON(def))
}

func handleRollingSLORecord(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, "invalid_rolling_slo_record", http.StatusBadRequest)
		return
	}

	name, rec, ok := decodeRollingSLORecord(body)
	if !ok {
		writeAPIError(w, "invalid_rolling_slo_record", http.StatusBadRequest)
		return
	}

	if !tenantStore(r).appendRollingSLORecord(name, rec) {
		writeAPIError(w, "rolling_slo_not_found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"accepted": 1})
}

func handleRollingSLOReport(w http.ResponseWriter, r *http.Request) {
	invalid := func() {
		writeAPIError(w, "invalid_rolling_slo_query", http.StatusBadRequest)
	}

	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		invalid()
		return
	}

	// Only name and at are accepted, each at most once.
	var name, atRaw string
	var haveName, haveAt bool
	for key, vals := range values {
		if len(vals) != 1 {
			invalid()
			return
		}
		switch key {
		case "name":
			name, haveName = vals[0], true
		case "at":
			atRaw, haveAt = vals[0], true
		default:
			invalid()
			return
		}
	}
	if !haveName || !haveAt {
		invalid()
		return
	}

	at, err := time.Parse(time.RFC3339Nano, atRaw)
	if err != nil {
		invalid()
		return
	}

	report, ok := tenantStore(r).rollingSLOReport(name, at)
	if !ok {
		writeAPIError(w, "rolling_slo_not_found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
