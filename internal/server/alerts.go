package server

import (
	"encoding/json"
	"io"
	"math"
	"mime"
	"net/http"
	"sort"
)

// alertRule is a validated, stored alerting rule. Rules live only in process
// memory and are lost on restart, exactly like metric state.
type alertRule struct {
	ID        string
	Kind      string // "threshold" or "ratio"
	Operator  string // "gt", "gte", "lt" or "lte"
	Threshold float64

	// threshold kind: single selector.
	Metric string
	Labels map[string]string

	// ratio kind: two selectors.
	Numerator   ruleSelector
	Denominator ruleSelector
}

// ruleSelector picks the series one rule side aggregates: every counter and
// gauge series whose name equals Metric and that carries all of Labels.
type ruleSelector struct {
	Metric string            `json:"metric"`
	Labels map[string]string `json:"labels"`
}

// ruleOutput is the wire representation of a stored rule. Only the fields
// relevant to the rule's kind are emitted, and selector labels are always
// present (possibly as {}) so a rule round-trips through read and write.
type ruleOutput struct {
	ID          string
	Kind        string
	Metric      string
	Labels      map[string]string
	Numerator   *ruleSelector
	Denominator *ruleSelector
	Operator    string
	Threshold   float64
}

func (o ruleOutput) MarshalJSON() ([]byte, error) {
	m := map[string]any{
		"id":        o.ID,
		"kind":      o.Kind,
		"operator":  o.Operator,
		"threshold": o.Threshold,
	}
	if o.Kind == "threshold" {
		m["metric"] = o.Metric
		m["labels"] = o.Labels
	} else {
		m["numerator"] = o.Numerator
		m["denominator"] = o.Denominator
	}
	return json.Marshal(m)
}

func (r alertRule) toOutput() ruleOutput {
	out := ruleOutput{
		ID:        r.ID,
		Kind:      r.Kind,
		Operator:  r.Operator,
		Threshold: r.Threshold,
	}
	if r.Kind == "threshold" {
		out.Metric = r.Metric
		out.Labels = r.Labels
	} else {
		num, den := r.Numerator, r.Denominator
		out.Numerator = &num
		out.Denominator = &den
	}
	return out
}

// alertOutput is one evaluated rule in the /api/v1/alerts response. Value is
// nil exactly when State is "no_data".
type alertOutput struct {
	ID    string   `json:"id"`
	State string   `json:"state"`
	Value *float64 `json:"value"`
}

// rawRule mirrors the accepted PUT body. Pointers distinguish missing fields
// from zero values; strictDecode rejects unknown fields outright.
type rawRule struct {
	Kind        *string            `json:"kind"`
	Metric      *string            `json:"metric"`
	Labels      *map[string]string `json:"labels"`
	Numerator   *json.RawMessage   `json:"numerator"`
	Denominator *json.RawMessage   `json:"denominator"`
	Operator    *string            `json:"operator"`
	Threshold   *float64           `json:"threshold"`
}

type rawSelector struct {
	Metric *string            `json:"metric"`
	Labels *map[string]string `json:"labels"`
}

// decodeRule fully validates a PUT body together with the path id. It never
// returns a partially validated rule.
func decodeRule(body []byte, id string) (alertRule, bool) {
	if !identPattern.MatchString(id) {
		return alertRule{}, false
	}

	var raw rawRule
	if !strictDecode(body, &raw) {
		return alertRule{}, false
	}
	if raw.Kind == nil || raw.Operator == nil || raw.Threshold == nil {
		return alertRule{}, false
	}
	kind, op, threshold := *raw.Kind, *raw.Operator, *raw.Threshold
	if kind != "threshold" && kind != "ratio" {
		return alertRule{}, false
	}
	switch op {
	case "gt", "gte", "lt", "lte":
	default:
		return alertRule{}, false
	}
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		return alertRule{}, false // encoding/json already rejects NaN/Inf tokens
	}

	rule := alertRule{ID: id, Kind: kind, Operator: op, Threshold: threshold}
	switch kind {
	case "threshold":
		if raw.Metric == nil || raw.Labels == nil || raw.Numerator != nil || raw.Denominator != nil {
			return alertRule{}, false
		}
		sel, ok := validateSelector(*raw.Metric, *raw.Labels)
		if !ok {
			return alertRule{}, false
		}
		rule.Metric, rule.Labels = sel.Metric, sel.Labels
	case "ratio":
		if raw.Metric != nil || raw.Labels != nil || raw.Numerator == nil || raw.Denominator == nil {
			return alertRule{}, false
		}
		num, ok := decodeSelector(*raw.Numerator)
		if !ok {
			return alertRule{}, false
		}
		den, ok := decodeSelector(*raw.Denominator)
		if !ok {
			return alertRule{}, false
		}
		rule.Numerator, rule.Denominator = num, den
	}
	return rule, true
}

func decodeSelector(raw json.RawMessage) (ruleSelector, bool) {
	var rs rawSelector
	if !strictDecode(raw, &rs) || rs.Metric == nil || rs.Labels == nil {
		return ruleSelector{}, false
	}
	return validateSelector(*rs.Metric, *rs.Labels)
}

func validateSelector(metric string, labels map[string]string) (ruleSelector, bool) {
	if !identPattern.MatchString(metric) || labels == nil {
		return ruleSelector{}, false
	}
	for k := range labels {
		if !identPattern.MatchString(k) {
			return ruleSelector{}, false
		}
	}
	return ruleSelector{Metric: metric, Labels: copyLabels(labels)}, true
}

// putRule creates or atomically replaces a rule and reports whether it was
// newly created.
func (s *metricStore) putRule(rule alertRule) (created bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.rules[rule.ID]
	s.rules[rule.ID] = rule
	return !existed
}

func (s *metricStore) getRule(id string) (alertRule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rule, ok := s.rules[id]
	return rule, ok
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

// listRules returns every rule ordered by id.
func (s *metricStore) listRules() []alertRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]alertRule, 0, len(s.rules))
	for _, rule := range s.rules {
		out = append(out, rule)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// evalAlerts computes every rule against the current consistent snapshot of
// metrics and rules, ordered by rule id.
func (s *metricStore) evalAlerts() []alertOutput {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := make([]string, 0, len(s.rules))
	for id := range s.rules {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]alertOutput, 0, len(ids))
	for _, id := range ids {
		rule := s.rules[id]
		item := alertOutput{ID: id, State: "no_data"}
		var value float64
		usable := false
		switch rule.Kind {
		case "threshold":
			if sum, found := s.sumLocked(rule.Metric, rule.Labels); found {
				value, usable = sum, true
			}
		case "ratio":
			num, okNum := s.sumLocked(rule.Numerator.Metric, rule.Numerator.Labels)
			den, okDen := s.sumLocked(rule.Denominator.Metric, rule.Denominator.Labels)
			if okNum && okDen && den != 0 {
				value, usable = num/den, true
			}
		}
		if usable {
			v := value
			item.Value = &v
			if compareValues(rule.Operator, value, rule.Threshold) {
				item.State = "firing"
			} else {
				item.State = "inactive"
			}
		}
		out = append(out, item)
	}
	return out
}

// sumLocked sums the current values of all counter and gauge series matching
// the selector. Histograms never participate, so a selector matching only
// histograms reports no usable series. Callers must hold s.mu.
func (s *metricStore) sumLocked(metric string, selector map[string]string) (float64, bool) {
	var sum float64
	found := false
	for _, cur := range s.series {
		if cur.name != metric || (cur.typ != "counter" && cur.typ != "gauge") {
			continue
		}
		matched := true
		for k, v := range selector {
			got, ok := cur.labels[k]
			if !ok || got != v {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		sum += cur.value
		found = true
	}
	return sum, found
}

func compareValues(op string, value, threshold float64) bool {
	switch op {
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

func handleRulePut(w http.ResponseWriter, r *http.Request, store *metricStore) {
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

	rule, ok := decodeRule(body, r.PathValue("id"))
	if !ok {
		writeAPIError(w, "invalid_rule", http.StatusBadRequest)
		return
	}

	created := store.putRule(rule)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if created {
		w.WriteHeader(http.StatusCreated)
	}
	_ = json.NewEncoder(w).Encode(rule.toOutput())
}

func handleRuleGet(w http.ResponseWriter, r *http.Request, store *metricStore) {
	rule, ok := store.getRule(r.PathValue("id"))
	if !ok {
		writeAPIError(w, "rule_not_found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(rule.toOutput())
}

func handleRuleDelete(w http.ResponseWriter, r *http.Request, store *metricStore) {
	if !store.deleteRule(r.PathValue("id")) {
		writeAPIError(w, "rule_not_found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleRuleList(w http.ResponseWriter, store *metricStore) {
	rules := store.listRules()
	out := make([]ruleOutput, 0, len(rules))
	for _, rule := range rules {
		out = append(out, rule.toOutput())
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"rules": out})
}

func handleAlertsGet(w http.ResponseWriter, store *metricStore) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"alerts": store.evalAlerts()})
}
