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

// slo is an immutable SLO definition. Replacements swap the pointer
// atomically, so readers never observe a half-updated definition.
type slo struct {
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

func sloWireJSON(d *slo) sloJSON {
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
func decodeSLO(body []byte) (*slo, bool) {
	var raw rawSLO
	if !strictDecode(body, &raw) {
		return nil, false
	}
	if raw.Objective == nil || raw.Good == nil || raw.Total == nil || raw.Comment == nil {
		return nil, false
	}

	objective := *raw.Objective
	if math.IsNaN(objective) || math.IsInf(objective, 0) || objective < 0 || objective > 1 {
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

	return &slo{
		objective: objective,
		good:      good,
		total:     total,
		comment:   *raw.Comment,
	}, true
}

// ---- store operations ------------------------------------------------------

func (s *metricStore) putSLO(d *slo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.slos[d.id]
	s.slos[d.id] = d
	return !existed
}

func (s *metricStore) getSLO(id string) (*slo, bool) {
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

// snapshotSLOs returns all definitions sorted by id. SLO objects are
// immutable, so the pointers stay safe after the lock is released.
func (s *metricStore) snapshotSLOs() []*slo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotSLOsLocked()
}

// snapshotSLOsLocked returns all definitions sorted by id. SLO objects are
// immutable, so the pointers stay safe after the lock is released.
func (s *metricStore) snapshotSLOsLocked() []*slo {
	out := make([]*slo, 0, len(s.slos))
	for _, d := range s.slos {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// selectorCounterSumLocked sums current values of all matching counter
// series. ok is false when no counter series matches; gauges and histograms
// are ignored, so a selector matching those only counts as unavailable. The
// caller must hold s.mu (shared with metric commits for one snapshot).
func (s *metricStore) selectorCounterSumLocked(sel *ruleSelector) (sum float64, ok bool) {
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

type sloStatusJSON struct {
	ID                        string   `json:"id"`
	State                     string   `json:"state"`
	GoodEvents                *float64 `json:"good_events"`
	TotalEvents               *float64 `json:"total_events"`
	Compliance                *float64 `json:"compliance"`
	ErrorBudgetTotal          *float64 `json:"error_budget_total"`
	ErrorBudgetRemaining      *float64 `json:"error_budget_remaining"`
	ErrorBudgetRemainingRatio *float64 `json:"error_budget_remaining_ratio"`
}

// evalSLOs evaluates every definition against a single consistent snapshot
// of metrics and definitions; results are sorted by id.
func (s *metricStore) evalSLOs() []sloStatusJSON {
	s.mu.RLock()
	defer s.mu.RUnlock()

	defs := s.snapshotSLOsLocked()
	out := make([]sloStatusJSON, 0, len(defs))
	for _, d := range defs {
		item := sloStatusJSON{ID: d.id, State: "no_data"}

		good, goodOK := s.selectorCounterSumLocked(d.good)
		total, totalOK := s.selectorCounterSumLocked(d.total)
		if !goodOK || !totalOK || total == 0 {
			out = append(out, item)
			continue
		}
		if math.IsNaN(good) || math.IsInf(good, 0) ||
			math.IsNaN(total) || math.IsInf(total, 0) || good > total {
			item.State = "invalid_data"
			out = append(out, item)
			continue
		}

		compliance := good / total
		budgetTotal := total * (1 - d.objective)
		budgetRemaining := budgetTotal - (total - good)
		state := "met"
		if compliance < d.objective {
			state = "breached"
		}

		item.State = state
		item.GoodEvents = floatPtr(good)
		item.TotalEvents = floatPtr(total)
		item.Compliance = floatPtr(compliance)
		item.ErrorBudgetTotal = floatPtr(budgetTotal)
		item.ErrorBudgetRemaining = floatPtr(budgetRemaining)
		// The only non-finite case is a zero total budget (objective 1); the
		// spec assigns no ratio there, so emit null rather than invalid JSON.
		ratio := budgetRemaining / budgetTotal
		if !math.IsNaN(ratio) && !math.IsInf(ratio, 0) {
			item.ErrorBudgetRemainingRatio = floatPtr(ratio)
		}
		out = append(out, item)
	}
	return out
}

func floatPtr(v float64) *float64 {
	return &v
}

// ---- HTTP handlers ---------------------------------------------------------

func registerSLOHandlers(mux *http.ServeMux, store *metricStore) {
	mux.HandleFunc(slosPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		defs := store.snapshotSLOs()
		body := make([]sloJSON, 0, len(defs))
		for _, d := range defs {
			body = append(body, sloWireJSON(d))
		}
		writeJSON(w, http.StatusOK, map[string]any{"slos": body})
	})

	mux.HandleFunc(slosPrefix, func(w http.ResponseWriter, r *http.Request) {
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
			// an identifier-shaped but invalid id is a bad SLO identifier.
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
		writeJSON(w, http.StatusOK, map[string]any{"slos": store.evalSLOs()})
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

	// The whole body is validated before the store swap, so a rejected
	// replacement always leaves the old definition in place.
	d, ok := decodeSLO(body)
	if !ok {
		writeAPIError(w, "invalid_slo", http.StatusBadRequest)
		return
	}
	d.id = id

	created := store.putSLO(d)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, sloWireJSON(d))
}

func handleSLODelete(w http.ResponseWriter, id string, store *metricStore) {
	if !store.deleteSLO(id) {
		writeAPIError(w, "slo_not_found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
