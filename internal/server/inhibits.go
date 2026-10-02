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

// inhibitRule is an immutable inhibit-rule document. Replacements swap the
// pointer atomically, so readers never observe a half-updated rule.
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

func inhibitRuleWireJSON(r *inhibitRule) inhibitRuleJSON {
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

// decodeInhibitRule parses and fully validates an inhibit-rule body. It never
// returns a partially validated rule.
func decodeInhibitRule(body []byte) (*inhibitRule, bool) {
	var raw rawInhibitRule
	if !strictDecode(body, &raw) {
		return nil, false
	}
	if raw.SourceRuleIDs == nil || raw.TargetRuleIDs == nil || raw.Comment == nil {
		return nil, false
	}

	sources, ok := validRuleIDSet(*raw.SourceRuleIDs)
	if !ok {
		return nil, false
	}
	targets, ok := validRuleIDSet(*raw.TargetRuleIDs)
	if !ok {
		return nil, false
	}
	for _, id := range sources {
		if _, crossed := indexOf(targets, id); crossed {
			return nil, false
		}
	}

	return &inhibitRule{
		sourceRuleIDs: sources,
		targetRuleIDs: targets,
		comment:       *raw.Comment,
	}, true
}

// validRuleIDSet checks a non-empty list of unique, syntactically valid rule
// identifiers and returns a defensive copy.
func validRuleIDSet(ids []string) ([]string, bool) {
	if len(ids) == 0 {
		return nil, false
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !identPattern.MatchString(id) || seen[id] {
			return nil, false
		}
		seen[id] = true
	}
	return append([]string(nil), ids...), true
}

func indexOf(ids []string, want string) (int, bool) {
	for i, id := range ids {
		if id == want {
			return i, true
		}
	}
	return -1, false
}

// ---- store operations ------------------------------------------------------

func (s *metricStore) putInhibitRule(r *inhibitRule) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.inhibitRules[r.id]
	s.inhibitRules[r.id] = r
	return !existed
}

func (s *metricStore) getInhibitRule(id string) (*inhibitRule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.inhibitRules[id]
	return r, ok
}

func (s *metricStore) deleteInhibitRule(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inhibitRules[id]; !ok {
		return false
	}
	delete(s.inhibitRules, id)
	return true
}

// snapshotInhibitRules returns all inhibit rules sorted by id. Rule objects
// are immutable, so the pointers stay safe after the lock is released.
func (s *metricStore) snapshotInhibitRules() []*inhibitRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*inhibitRule, 0, len(s.inhibitRules))
	for _, r := range s.inhibitRules {
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
			body = append(body, inhibitRuleWireJSON(rule))
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
				handleInhibitRuleGet(w, id, store)
			} else {
				handleInhibitRuleDelete(w, id, store)
			}
		case http.MethodPut:
			// A structurally nested path is outside this resource subtree;
			// an identifier-shaped but invalid id is a bad identifier.
			if id == "" || strings.Contains(id, "/") {
				writeAPIError(w, "inhibit_rule_not_found", http.StatusNotFound)
				return
			}
			if !identPattern.MatchString(id) {
				writeAPIError(w, "invalid_inhibit_rule", http.StatusBadRequest)
				return
			}
			handleInhibitRulePut(w, r, id, store)
		default:
			w.Header().Set("Allow", "GET, PUT, DELETE")
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
		}
	})
}

func handleInhibitRuleGet(w http.ResponseWriter, id string, store *metricStore) {
	rule, ok := store.getInhibitRule(id)
	if !ok {
		writeAPIError(w, "inhibit_rule_not_found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, inhibitRuleWireJSON(rule))
}

func handleInhibitRulePut(w http.ResponseWriter, r *http.Request, id string, store *metricStore) {
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
	writeJSON(w, status, inhibitRuleWireJSON(rule))
}

func handleInhibitRuleDelete(w http.ResponseWriter, id string, store *metricStore) {
	if !store.deleteInhibitRule(id) {
		writeAPIError(w, "inhibit_rule_not_found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
