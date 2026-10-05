package server

import (
	"io"
	"math"
	"mime"
	"net/http"
	"sort"
	"strings"
)

const (
	slosPath      = "/api/v1/slos"
	slosPrefix    = slosPath + "/"
	sloStatusPath = "/api/v1/slo-status"
)

// sloDefinition is an immutable SLO document: a target objective plus the
// good/total counter selectors that define the measured event streams.
// Replacements swap the pointer atomically, so readers never observe a
// half-updated definition.
type sloDefinition struct {
	id        string
	objective float64
	good      *ruleSelector
	total     *ruleSelector
	comment   string
}

// ---- JSON wire shapes ------------------------------------------------------

type sloJSON struct {
	ID        string       `json:"id"`
	Objective float64      `json:"objective"`
	Good      ruleSideJSON `json:"good"`
	Total     ruleSideJSON `json:"total"`
	Comment   string       `json:"comment"`
}

func sloWireJSON(d *sloDefinition) sloJSON {
	return sloJSON{
		ID:        d.id,
		Objective: d.objective,
		Good:      sideWireJSON(d.good),
		Total:     sideWireJSON(d.total),
		Comment:   d.comment,
	}
}

// ---- request decoding ------------------------------------------------------

type rawSLO struct {
	Objective *float64     `json:"objective"`
	Good      *rawRuleSide `json:"good"`
	Total     *rawRuleSide `json:"total"`
	Comment   *string      `json:"comment"`
}

// decodeSLO parses and fully validates an SLO body. It never returns a
// partially validated definition.
func decodeSLO(body []byte) (*sloDefinition, bool) {
	var raw rawSLO
	if !strictDecode(body, &raw) {
		return nil, false
	}
	if raw.Objective == nil || raw.Good == nil || raw.Total == nil || raw.Comment == nil {
		return nil, false
	}
	objective := *raw.Objective
	if math.IsNaN(objective) || math.IsInf(objective, 0) || objective <= 0 || objective >= 1 {
		return nil, false
	}
	good, ok := decodeRawSide(raw.Good)
	if !ok {
		return nil, false
	}
	total, ok := decodeRawSide(raw.Total)
	if !ok {
		return nil, false
	}
	return &sloDefinition{
		objective: objective,
		good:      good,
		total:     total,
		comment:   *raw.Comment,
	}, true
}

// ---- store operations ------------------------------------------------------

func (s *metricStore) putSLO(d *sloDefinition) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.slos[d.id]
	s.slos[d.id] = d
	return !existed
}

func (s *metricStore) getSLO(id string) (*sloDefinition, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.slos[id]
	return d, ok
}

func (s *metricStore) deleteSLO(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.slos[id]; !ok {
		return false
	}
	delete(s.slos, id)
	return true
}

// snapshotSLOs returns all definitions sorted by id. Definition objects are
// immutable, so the pointers stay safe after the lock is released.
func (s *metricStore) snapshotSLOs() []*sloDefinition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotSLOsLocked()
}

func (s *metricStore) snapshotSLOsLocked() []*sloDefinition {
	out := make([]*sloDefinition, 0, len(s.slos))
	for _, d := range s.slos {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// counterSum sums current values of all matching counter series. ok is false
// when no counter series matches; gauges and histograms are ignored, so a
// selector matching only those counts as unavailable.
func (s *metricStore) counterSum(sel *ruleSelector) (sum float64, ok bool) {
	for _, cur := range s.series {
		if cur.typ != "counter" {
			continue
		}
		if cur.name != sel.metric {
			continue
		}
		matched := true
		for k, v := range sel.labels {
			got, exists := cur.labels[k]
			if !exists || got != v {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		sum += cur.value
		ok = true
	}
	return sum, ok
}

// ---- status evaluation -----------------------------------------------------

type sloStatusOutput struct {
	ID                        string   `json:"id"`
	State                     string   `json:"state"`
	GoodEvents                *float64 `json:"good_events"`
	TotalEvents               *float64 `json:"total_events"`
	Compliance                *float64 `json:"compliance"`
	ErrorBudgetTotal          *float64 `json:"error_budget_total"`
	ErrorBudgetRemaining      *float64 `json:"error_budget_remaining"`
	ErrorBudgetRemainingRatio *float64 `json:"error_budget_remaining_ratio"`
}

// evalSLOStatus computes every SLO definition against a single consistent
// snapshot of definitions and counter series; results are sorted by id.
//
// Each side sums the current values of matching counters only. A side with no
// matching counter, or a zero total, yields no_data. Non-finite aggregates or
// good exceeding total yield invalid_data. In both cases every numeric field
// is null. Otherwise compliance, budget totals and remaining amounts are
// derived from the event counts; remaining amounts may go negative.
func (s *metricStore) evalSLOStatus() []sloStatusOutput {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.evalSLOStatusLocked()
}

// evalSLOStatusLocked is evalSLOStatus for callers already holding the store
// lock, so the diagnostic package's SLO section shares one snapshot.
func (s *metricStore) evalSLOStatusLocked() []sloStatusOutput {
	defs := s.snapshotSLOsLocked()
	out := make([]sloStatusOutput, 0, len(defs))
	for _, d := range defs {
		st := sloStatusOutput{ID: d.id}

		good, goodOK := s.counterSum(d.good)
		total, totalOK := s.counterSum(d.total)
		switch {
		case !goodOK || !totalOK || total == 0:
			st.State = "no_data"
		case math.IsNaN(good) || math.IsInf(good, 0) ||
			math.IsNaN(total) || math.IsInf(total, 0) || good > total:
			st.State = "invalid_data"
		default:
			compliance := good / total
			budgetTotal := total * (1 - d.objective)
			remaining := budgetTotal - (total - good)
			remainingRatio := remaining / budgetTotal

			st.State = "breached"
			if compliance >= d.objective {
				st.State = "met"
			}
			st.GoodEvents = &good
			st.TotalEvents = &total
			st.Compliance = &compliance
			st.ErrorBudgetTotal = &budgetTotal
			st.ErrorBudgetRemaining = &remaining
			st.ErrorBudgetRemainingRatio = &remainingRatio
		}
		out = append(out, st)
	}
	return out
}

// ---- HTTP handlers ---------------------------------------------------------

func registerSLOHandlers(mux *http.ServeMux) {
	mux.HandleFunc(slosPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		defs := tenantStore(r).snapshotSLOs()
		body := make([]sloJSON, 0, len(defs))
		for _, d := range defs {
			body = append(body, sloWireJSON(d))
		}
		writeJSON(w, http.StatusOK, map[string]any{"slos": body})
	})

	mux.HandleFunc(slosPrefix, func(w http.ResponseWriter, r *http.Request) {
		store := tenantStore(r)
		id := strings.TrimPrefix(r.URL.Path, slosPrefix)
		switch r.Method {
		case http.MethodGet, http.MethodDelete:
			// Reads and deletions can only address an existing, syntactically
			// valid SLO id; anything else is simply not found.
			if id == "" || strings.Contains(id, "/") || !identPattern.MatchString(id) {
				writeAPIError(w, "slo_not_found", http.StatusNotFound)
				return
			}
			if r.Method == http.MethodGet {
				handleSLOGet(w, id, store)
			} else {
				handleSLODelete(w, id, store)
			}
		case http.MethodPut:
			// A structurally nested path is outside this resource subtree;
			// an identifier-shaped but invalid id is a bad identifier.
			if id == "" || strings.Contains(id, "/") {
				writeAPIError(w, "slo_not_found", http.StatusNotFound)
				return
			}
			if !identPattern.MatchString(id) {
				writeAPIError(w, "invalid_slo", http.StatusBadRequest)
				return
			}
			handleSLOPut(w, r, id, store)
		default:
			w.Header().Set("Allow", "GET, PUT, DELETE")
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(sloStatusPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"slos": tenantStore(r).evalSLOStatus()})
	})
}

func handleSLOGet(w http.ResponseWriter, id string, store *metricStore) {
	d, ok := store.getSLO(id)
	if !ok {
		writeAPIError(w, "slo_not_found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, sloWireJSON(d))
}

func handleSLOPut(w http.ResponseWriter, r *http.Request, id string, store *metricStore) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, "invalid_slo", http.StatusBadRequest)
		return
	}

	def, ok := decodeSLO(body)
	if !ok {
		writeAPIError(w, "invalid_slo", http.StatusBadRequest)
		return
	}
	def.id = id

	created := store.putSLO(def)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, sloWireJSON(def))
}

func handleSLODelete(w http.ResponseWriter, id string, store *metricStore) {
	if !store.deleteSLO(id) {
		writeAPIError(w, "slo_not_found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
