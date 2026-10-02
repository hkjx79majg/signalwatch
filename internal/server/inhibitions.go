package server

import (
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
)

const (
	inhibitRulesPath   = "/api/v1/inhibit-rules"
	inhibitRulesPrefix = inhibitRulesPath + "/"
)

// inhibitRule is an immutable inhibition document. A rule suppresses every
// target alert (by alert rule id) while at least one existing source alert is
// firing. Replacements swap the pointer atomically, so readers never observe a
// half-updated rule.
type inhibitRule struct {
	id            string
	sourceRuleIDs []string
	targetRuleIDs []string
	comment       string
}

// ---- JSON wire shapes ------------------------------------------------------

type inhibitRuleJSON struct {
	ID            string   `json:"id"`
	SourceRuleIDs []string `json:"source_rule_ids"`
	TargetRuleIDs []string `json:"target_rule_ids"`
	Comment       string   `json:"comment"`
}

func inhibitWireJSON(r *inhibitRule) inhibitRuleJSON {
	return inhibitRuleJSON{
		ID:            r.id,
		SourceRuleIDs: append([]string(nil), r.sourceRuleIDs...),
		TargetRuleIDs: append([]string(nil), r.targetRuleIDs...),
		Comment:       r.comment,
	}
}

// ---- request decoding ------------------------------------------------------

type rawInhibitRule struct {
	SourceRuleIDs *[]string `json:"source_rule_ids"`
	TargetRuleIDs *[]string `json:"target_rule_ids"`
	Comment       *string   `json:"comment"`
}

// decodeInhibitRule parses and fully validates a rule body. It never returns a
// partially validated rule.
func decodeInhibitRule(body []byte) (*inhibitRule, bool) {
	var raw rawInhibitRule
	if !strictDecode(body, &raw) {
		return nil, false
	}
	if raw.SourceRuleIDs == nil || raw.TargetRuleIDs == nil || raw.Comment == nil {
		return nil, false
	}

	sourceIDs, ok := validateIDList(*raw.SourceRuleIDs)
	if !ok {
		return nil, false
	}
	targetIDs, ok := validateIDList(*raw.TargetRuleIDs)
	if !ok {
		return nil, false
	}
	// Source and target sets must not intersect.
	inTarget := make(map[string]bool, len(targetIDs))
	for _, id := range targetIDs {
		inTarget[id] = true
	}
	for _, id := range sourceIDs {
		if inTarget[id] {
			return nil, false
		}
	}

	return &inhibitRule{
		sourceRuleIDs: sourceIDs,
		targetRuleIDs: targetIDs,
		comment:       *raw.Comment,
	}, true
}

// validateIDList requires a non-empty list of distinct valid identifiers and
// returns a defensive copy preserving the submitted order.
func validateIDList(ids []string) ([]string, bool) {
	if len(ids) == 0 {
		return nil, false
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !identPattern.MatchString(id) || seen[id] {
			return nil, false
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, true
}

// ---- store operations ------------------------------------------------------

func (s *metricStore) putInhibitRule(r *inhibitRule) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.inhibitions[r.id]
	s.inhibitions[r.id] = r
	return !existed
}

func (s *metricStore) getInhibitRule(id string) (*inhibitRule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.inhibitions[id]
	return r, ok
}

func (s *metricStore) deleteInhibitRule(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inhibitions[id]; !ok {
		return false
	}
	delete(s.inhibitions, id)
	return true
}

// snapshotInhibitRules returns all rules sorted by id. Rule objects are
// immutable, so the pointers stay safe after the lock is released.
func (s *metricStore) snapshotInhibitRules() []*inhibitRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*inhibitRule, 0, len(s.inhibitions))
	for _, r := range s.inhibitions {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// ---- HTTP handlers ---------------------------------------------------------

func registerInhibitHandlers(mux *http.ServeMux, store *metricStore) {
	mux.HandleFunc(inhibitRulesPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		rules := store.snapshotInhibitRules()
		body := make([]inhibitRuleJSON, 0, len(rules))
		for _, rule := range rules {
			body = append(body, inhibitWireJSON(rule))
		}
		writeJSON(w, http.StatusOK, map[string]any{"rules": body})
	})

	mux.HandleFunc(inhibitRulesPrefix, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, inhibitRulesPrefix)
		switch r.Method {
		case http.MethodGet, http.MethodDelete:
			// Reads and deletions can only address an existing, syntactically
			// valid rule id; anything else is simply not found.
			if id == "" || strings.Contains(id, "/") || !identPattern.MatchString(id) {
				writeAPIError(w, "inhibit_rule_not_found", http.StatusNotFound)
				return
			}
			if r.Method == http.MethodGet {
				handleInhibitGet(w, id, store)
			} else {
				handleInhibitDelete(w, id, store)
			}
		case http.MethodPut:
			// A structurally nested path is outside this resource subtree;
			// an identifier-shaped but invalid id is a bad rule identifier.
			if id == "" || strings.Contains(id, "/") {
				writeAPIError(w, "inhibit_rule_not_found", http.StatusNotFound)
				return
			}
			if !identPattern.MatchString(id) {
				writeAPIError(w, "invalid_inhibit_rule", http.StatusBadRequest)
				return
			}
			handleInhibitPut(w, r, id, store)
		default:
			w.Header().Set("Allow", "GET, PUT, DELETE")
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
		}
	})
}

func handleInhibitGet(w http.ResponseWriter, id string, store *metricStore) {
	rule, ok := store.getInhibitRule(id)
	if !ok {
		writeAPIError(w, "inhibit_rule_not_found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, inhibitWireJSON(rule))
}

func handleInhibitPut(w http.ResponseWriter, r *http.Request, id string, store *metricStore) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, "invalid_inhibit_rule", http.StatusBadRequest)
		return
	}

	rule, ok := decodeInhibitRule(body)
	if !ok {
		writeAPIError(w, "invalid_inhibit_rule", http.StatusBadRequest)
		return
	}
	rule.id = id

	created := store.putInhibitRule(rule)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, inhibitWireJSON(rule))
}

func handleInhibitDelete(w http.ResponseWriter, id string, store *metricStore) {
	if !store.deleteInhibitRule(id) {
		writeAPIError(w, "inhibit_rule_not_found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
