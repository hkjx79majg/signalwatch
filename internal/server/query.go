package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"sort"
)

const queryPath = "/api/v1/query"

// parsedExpr is a validated query expression: either a plain selector or one
// aggregation over a selector, optionally grouped by a label projection.
type parsedExpr struct {
	agg string // "", "sum", "avg", "min" or "max"
	by  []string
	sel ruleSelector
}

func isAggFunc(name string) bool {
	switch name {
	case "sum", "avg", "min", "max":
		return true
	}
	return false
}

// exprParser is a cursor over the raw expression text. ASCII whitespace is
// skipped around tokens and never changes meaning.
type exprParser struct {
	s string
	i int
}

func (p *exprParser) skipWS() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			p.i++
		default:
			return
		}
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func (p *exprParser) parseIdent() (string, bool) {
	if p.i >= len(p.s) || !isIdentStart(p.s[p.i]) {
		return "", false
	}
	start := p.i
	p.i++
	for p.i < len(p.s) && isIdentChar(p.s[p.i]) {
		p.i++
	}
	return p.s[start:p.i], true
}

// peekIdent reads an identifier without advancing the cursor.
func (p *exprParser) peekIdent() (string, bool) {
	save := p.i
	id, ok := p.parseIdent()
	p.i = save
	return id, ok
}

// parseJSONString consumes one JSON string token (including escapes) and
// returns the decoded value.
func (p *exprParser) parseJSONString() (string, bool) {
	if p.i >= len(p.s) || p.s[p.i] != '"' {
		return "", false
	}
	j := p.i + 1
	for j < len(p.s) {
		switch p.s[j] {
		case '\\':
			j += 2
			continue
		case '"':
			var out string
			if err := json.Unmarshal([]byte(p.s[p.i:j+1]), &out); err != nil {
				return "", false
			}
			p.i = j + 1
			return out, true
		}
		j++
	}
	return "", false
}

// parseSelector parses `metric{key="value",...}`; the brace block is optional
// and an empty block matches every series of the metric.
func (p *exprParser) parseSelector() (ruleSelector, bool) {
	name, ok := p.parseIdent()
	if !ok {
		return ruleSelector{}, false
	}
	sel := ruleSelector{metric: name, labels: map[string]string{}}
	p.skipWS()
	if p.i >= len(p.s) || p.s[p.i] != '{' {
		return sel, true
	}
	p.i++
	p.skipWS()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return sel, true
	}
	for {
		p.skipWS()
		key, ok := p.parseIdent()
		if !ok {
			return ruleSelector{}, false
		}
		p.skipWS()
		if p.i >= len(p.s) || p.s[p.i] != '=' {
			return ruleSelector{}, false
		}
		p.i++
		p.skipWS()
		val, ok := p.parseJSONString()
		if !ok {
			return ruleSelector{}, false
		}
		if _, dup := sel.labels[key]; dup {
			return ruleSelector{}, false
		}
		sel.labels[key] = val
		p.skipWS()
		if p.i >= len(p.s) {
			return ruleSelector{}, false
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return sel, true
		default:
			return ruleSelector{}, false
		}
	}
}

// parseLabelList parses `label,...` up to and including the closing ')'. The
// cursor is positioned just after the opening '('. At least one label is
// required and duplicates are rejected.
func (p *exprParser) parseLabelList() ([]string, bool) {
	seen := map[string]bool{}
	labels := []string{}
	for {
		p.skipWS()
		id, ok := p.parseIdent()
		if !ok {
			return nil, false
		}
		if seen[id] {
			return nil, false
		}
		seen[id] = true
		labels = append(labels, id)
		p.skipWS()
		if p.i >= len(p.s) {
			return nil, false
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ')':
			p.i++
			return labels, true
		default:
			return nil, false
		}
	}
}

// parseArgSelector parses `( selector )`.
func (p *exprParser) parseArgSelector() (ruleSelector, bool) {
	p.skipWS()
	if p.i >= len(p.s) || p.s[p.i] != '(' {
		return ruleSelector{}, false
	}
	p.i++
	p.skipWS()
	sel, ok := p.parseSelector()
	if !ok {
		return ruleSelector{}, false
	}
	p.skipWS()
	if p.i >= len(p.s) || p.s[p.i] != ')' {
		return ruleSelector{}, false
	}
	p.i++
	return sel, true
}

func (p *exprParser) parseExpr() (parsedExpr, bool) {
	p.skipWS()
	save := p.i
	name, ok := p.parseIdent()
	if !ok {
		return parsedExpr{}, false
	}
	p.skipWS()

	// Aggregation without grouping: fn(selector).
	if p.i < len(p.s) && p.s[p.i] == '(' {
		if !isAggFunc(name) {
			return parsedExpr{}, false
		}
		sel, ok := p.parseArgSelector()
		if !ok {
			return parsedExpr{}, false
		}
		return parsedExpr{agg: name, sel: sel}, true
	}

	// Aggregation with grouping: fn by (labels)(selector).
	if word, ok := p.peekIdent(); ok && word == "by" {
		if !isAggFunc(name) {
			return parsedExpr{}, false
		}
		_, _ = p.parseIdent() // consume "by"
		p.skipWS()
		if p.i >= len(p.s) || p.s[p.i] != '(' {
			return parsedExpr{}, false
		}
		p.i++
		by, ok := p.parseLabelList()
		if !ok {
			return parsedExpr{}, false
		}
		sel, ok := p.parseArgSelector()
		if !ok {
			return parsedExpr{}, false
		}
		return parsedExpr{agg: name, by: by, sel: sel}, true
	}

	// Plain selector; reparse from the metric name.
	p.i = save
	sel, ok := p.parseSelector()
	if !ok {
		return parsedExpr{}, false
	}
	return parsedExpr{sel: sel}, true
}

// parseExpression validates the full expression and rejects trailing content.
func parseExpression(s string) (parsedExpr, bool) {
	p := &exprParser{s: s}
	expr, ok := p.parseExpr()
	if !ok {
		return parsedExpr{}, false
	}
	p.skipWS()
	if p.i != len(p.s) {
		return parsedExpr{}, false
	}
	return expr, true
}

// vectorEntry is one element of the query result vector.
type vectorEntry struct {
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

type queryResponse struct {
	ResultType string        `json:"result_type"`
	Result     []vectorEntry `json:"result"`
}

// evalQuery evaluates the expression against one consistent snapshot of the
// store. Only counter and gauge current values participate. A false result
// means a selected value or an aggregation result was not finite.
func (s *metricStore) evalQuery(expr parsedExpr) ([]vectorEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type sample struct {
		labels map[string]string
		value  float64
	}
	var matched []sample
	for _, cur := range s.series {
		if cur.typ != "counter" && cur.typ != "gauge" {
			continue
		}
		if cur.name != expr.sel.metric {
			continue
		}
		ok := true
		for k, v := range expr.sel.labels {
			got, has := cur.labels[k]
			if !has || got != v {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if math.IsNaN(cur.value) || math.IsInf(cur.value, 0) {
			return nil, false
		}
		matched = append(matched, sample{labels: copyLabels(cur.labels), value: cur.value})
	}

	if expr.agg == "" {
		out := make([]vectorEntry, 0, len(matched))
		for _, m := range matched {
			out = append(out, vectorEntry{Labels: m.labels, Value: m.value})
		}
		sort.Slice(out, func(i, j int) bool {
			return compareLabels(out[i].Labels, out[j].Labels) < 0
		})
		return out, true
	}

	type group struct {
		labels map[string]string
		sum    float64
		min    float64
		max    float64
		count  int
	}
	groups := make(map[string]*group)
	for _, m := range matched {
		proj := make(map[string]string, len(expr.by))
		for _, l := range expr.by {
			if v, ok := m.labels[l]; ok {
				proj[l] = v
			}
		}
		key := seriesKey("", proj)
		g, ok := groups[key]
		if !ok {
			g = &group{labels: proj, min: m.value, max: m.value}
			groups[key] = g
		}
		g.sum += m.value
		if m.value < g.min {
			g.min = m.value
		}
		if m.value > g.max {
			g.max = m.value
		}
		g.count++
	}

	out := make([]vectorEntry, 0, len(groups))
	for _, g := range groups {
		var v float64
		switch expr.agg {
		case "sum":
			v = g.sum
		case "min":
			v = g.min
		case "max":
			v = g.max
		case "avg":
			v = g.sum / float64(g.count)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		out = append(out, vectorEntry{Labels: g.labels, Value: v})
	}
	sort.Slice(out, func(i, j int) bool {
		return compareLabels(out[i].Labels, out[j].Labels) < 0
	})
	return out, true
}

func handleQueryGet(w http.ResponseWriter, r *http.Request) {
	store := tenantStore(r)
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeAPIError(w, "invalid_expression", http.StatusBadRequest)
		return
	}
	exprs, ok := values["expr"]
	if len(values) != 1 || !ok || len(exprs) != 1 {
		writeAPIError(w, "invalid_expression", http.StatusBadRequest)
		return
	}
	expr, ok := parseExpression(exprs[0])
	if !ok {
		writeAPIError(w, "invalid_expression", http.StatusBadRequest)
		return
	}
	result, ok := store.evalQuery(expr)
	if !ok {
		writeAPIError(w, "invalid_query_data", http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(queryResponse{ResultType: "vector", Result: result})
}
