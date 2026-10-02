package server

import (
	"encoding/json"
	"io"
	"math"
	"mime"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	alertRulesPath   = "/api/v1/alert-rules"
	alertRulesPrefix = alertRulesPath + "/"
	alertsPath       = "/api/v1/alerts"
)

// ruleSelector selects series by exact metric name and a subset of labels.
type ruleSelector struct {
	metric string
	labels map[string]string
}

// alertRule is an immutable rule document. Replacements swap the pointer
// atomically, so readers never observe a half-updated rule.
type alertRule struct {
	id        string
	kind      string // "threshold" | "ratio"
	operator  string // gt | gte | lt | lte
	threshold float64

	// threshold rules use left only; ratio rules use both sides.
	left  *ruleSelector
	right *ruleSelector
}

// ---- JSON wire shapes ------------------------------------------------------

type ruleSideJSON struct {
	Metric string            `json:"metric"`
	Labels map[string]string `json:"labels"`
}

type thresholdRuleJSON struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Operator  string            `json:"operator"`
	Threshold float64           `json:"threshold"`
	Metric    string            `json:"metric"`
	Labels    map[string]string `json:"labels"`
}

type ratioRuleJSON struct {
	ID          string       `json:"id"`
	Kind        string       `json:"kind"`
	Operator    string       `json:"operator"`
	Threshold   float64      `json:"threshold"`
	Numerator   ruleSideJSON `json:"numerator"`
	Denominator ruleSideJSON `json:"denominator"`
}

func ruleWireJSON(r *alertRule) any {
	if r.kind == "ratio" {
		return ratioRuleJSON{
			ID:          r.id,
			Kind:        r.kind,
			Operator:    r.operator,
			Threshold:   r.threshold,
			Numerator:   sideWireJSON(r.left),
			Denominator: sideWireJSON(r.right),
		}
	}
	return thresholdRuleJSON{
		ID:        r.id,
		Kind:      r.kind,
		Operator:  r.operator,
		Threshold: r.threshold,
		Metric:    r.left.metric,
		Labels:    copyLabels(r.left.labels),
	}
}

func sideWireJSON(s *ruleSelector) ruleSideJSON {
	return ruleSideJSON{Metric: s.metric, Labels: copyLabels(s.labels)}
}

// ---- request decoding ------------------------------------------------------

type rawRuleSide struct {
	Metric *string            `json:"metric"`
	Labels *map[string]string `json:"labels"`
}

type rawRule struct {
	Kind        *string            `json:"kind"`
	Operator    *string            `json:"operator"`
	Threshold   *float64           `json:"threshold"`
	Metric      *string            `json:"metric"`
	Labels      *map[string]string `json:"labels"`
	Numerator   *rawRuleSide       `json:"numerator"`
	Denominator *rawRuleSide       `json:"denominator"`
}

// decodeRule parses and fully validates a rule body. It never returns a
// partially validated rule.
func decodeRule(body []byte) (*alertRule, bool) {
	var raw rawRule
	if !strictDecode(body, &raw) {
		return nil, false
	}
	if raw.Kind == nil || raw.Operator == nil || raw.Threshold == nil {
		return nil, false
	}
	kind := *raw.Kind
	if kind != "threshold" && kind != "ratio" {
		return nil, false
	}
	operator := *raw.Operator
	if !validOperators[operator] {
		return nil, false
	}
	threshold := *raw.Threshold
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		return nil, false
	}

	build := func() *alertRule {
		return &alertRule{kind: kind, operator: operator, threshold: threshold}
	}

	switch kind {
	case "threshold":
		if raw.Metric == nil || raw.Labels == nil || raw.Numerator != nil || raw.Denominator != nil {
			return nil, false
		}
		sel, ok := decodeSelector(*raw.Metric, *raw.Labels)
		if !ok {
			return nil, false
		}
		r := build()
		r.left = sel
		return r, true
	case "ratio":
		if raw.Metric != nil || raw.Labels != nil || raw.Numerator == nil || raw.Denominator == nil {
			return nil, false
		}
		num, ok := decodeRawSide(raw.Numerator)
		if !ok {
			return nil, false
		}
		den, ok := decodeRawSide(raw.Denominator)
		if !ok {
			return nil, false
		}
		r := build()
		r.left, r.right = num, den
		return r, true
	}
	return nil, false
}

func decodeRawSide(raw *rawRuleSide) (*ruleSelector, bool) {
	if raw.Metric == nil || raw.Labels == nil {
		return nil, false
	}
	return decodeSelector(*raw.Metric, *raw.Labels)
}

func decodeSelector(metric string, labels map[string]string) (*ruleSelector, bool) {
	if !identPattern.MatchString(metric) || labels == nil {
		return nil, false
	}
	for k := range labels {
		if !identPattern.MatchString(k) {
			return nil, false
		}
	}
	return &ruleSelector{metric: metric, labels: copyLabels(labels)}, true
}

var validOperators = map[string]bool{
	"gt":  true,
	"gte": true,
	"lt":  true,
	"lte": true,
}

// ---- store operations ------------------------------------------------------

func (s *metricStore) putRule(r *alertRule) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.rules[r.id]
	s.rules[r.id] = r
	return !existed
}

func (s *metricStore) getRule(id string) (*alertRule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rules[id]
	return r, ok
}

func (s *metricStore) deleteRule(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rules[id]; !ok {
		return false
	}
	delete(s.rules, id)
	return true
}

// snapshotRules returns all rules sorted by id. Rule objects are immutable,
// so the pointers stay safe after the lock is released.
func (s *metricStore) snapshotRules() []*alertRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*alertRule, 0, len(s.rules))
	for _, r := range s.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// selectorSum sums current values of all matching counter/gauge series.
// ok is false when no counter/gauge series matches; histograms are ignored,
// so a selector matching histograms only counts as unavailable.
func (s *metricStore) selectorSum(sel *ruleSelector) (sum float64, ok bool) {
	for _, cur := range s.series {
		if cur.typ != "counter" && cur.typ != "gauge" {
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

type alertOutput struct {
	ID         string   `json:"id"`
	State      string   `json:"state"`
	Value      *float64 `json:"value"`
	Silenced   bool     `json:"silenced"`
	SilenceIDs []string `json:"silence_ids"`
}

// evalAlerts computes every rule against a single consistent snapshot of
// metrics, rules and silences; results are sorted by rule id.
func (s *metricStore) evalAlerts() []alertOutput {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rules := make([]*alertRule, 0, len(s.rules))
	for _, r := range s.rules {
		rules = append(rules, r)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].id < rules[j].id })

	silences := make([]*silence, 0, len(s.silences))
	for _, sil := range s.silences {
		silences = append(silences, sil)
	}
	// Evaluation instant and silence windowing share the snapshot.
	now := time.Now()
	activeByRule := activeSilencesByRule(silences, now)

	out := make([]alertOutput, 0, len(rules))
	for _, r := range rules {
		al := alertOutput{ID: r.id, State: "inactive", SilenceIDs: []string{}}

		var value float64
		if r.kind == "threshold" {
			sum, ok := s.selectorSum(r.left)
			if !ok {
				al.State = "no_data"
				out = append(out, al)
				continue
			}
			value = sum
		} else {
			num, numOK := s.selectorSum(r.left)
			den, denOK := s.selectorSum(r.right)
			if !numOK || !denOK || den == 0 {
				al.State = "no_data"
				out = append(out, al)
				continue
			}
			value = num / den
		}

		v := value
		al.Value = &v
		if compareHolds(r.operator, value, r.threshold) {
			al.State = "firing"
			if hits := activeByRule[r.id]; len(hits) > 0 {
				al.Silenced = true
				al.SilenceIDs = append([]string(nil), hits...)
			}
		}
		out = append(out, al)
	}
	return out
}

func compareHolds(operator string, value, threshold float64) bool {
	switch operator {
	case "gt":
		return value > threshold
	case "gte":
		return value >= threshold
	case "lt":
		return value < threshold
	case "lte":
		return value <= threshold
	}
	return false
}

// ---- HTTP handlers ---------------------------------------------------------

func registerAlertHandlers(mux *http.ServeMux, store *metricStore) {
	mux.HandleFunc(alertRulesPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		rules := store.snapshotRules()
		body := make([]any, 0, len(rules))
		for _, rule := range rules {
			body = append(body, ruleWireJSON(rule))
		}
		writeJSON(w, http.StatusOK, map[string]any{"rules": body})
	})

	mux.HandleFunc(alertRulesPrefix, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, alertRulesPrefix)
		switch r.Method {
		case http.MethodGet, http.MethodDelete:
			// Reads and deletions can only address an existing, syntactically
			// valid rule id; anything else is simply not found.
			if id == "" || strings.Contains(id, "/") || !identPattern.MatchString(id) {
				writeAPIError(w, "rule_not_found", http.StatusNotFound)
				return
			}
			if r.Method == http.MethodGet {
				handleRuleGet(w, id, store)
			} else {
				handleRuleDelete(w, id, store)
			}
		case http.MethodPut:
			// A structurally nested path is outside this resource subtree;
			// an identifier-shaped but invalid id is a bad rule identifier.
			if id == "" || strings.Contains(id, "/") {
				writeAPIError(w, "rule_not_found", http.StatusNotFound)
				return
			}
			if !identPattern.MatchString(id) {
				writeAPIError(w, "invalid_rule", http.StatusBadRequest)
				return
			}
			handleRulePut(w, r, id, store)
		default:
			w.Header().Set("Allow", "GET, PUT, DELETE")
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(alertsPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"alerts": store.evalAlerts()})
	})
}

func handleRuleGet(w http.ResponseWriter, id string, store *metricStore) {
	rule, ok := store.getRule(id)
	if !ok {
		writeAPIError(w, "rule_not_found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, ruleWireJSON(rule))
}

func handleRulePut(w http.ResponseWriter, r *http.Request, id string, store *metricStore) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, "invalid_rule", http.StatusBadRequest)
		return
	}

	rule, ok := decodeRule(body)
	if !ok {
		writeAPIError(w, "invalid_rule", http.StatusBadRequest)
		return
	}
	rule.id = id

	created := store.putRule(rule)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, ruleWireJSON(rule))
}

func handleRuleDelete(w http.ResponseWriter, id string, store *metricStore) {
	if !store.deleteRule(id) {
		writeAPIError(w, "rule_not_found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
