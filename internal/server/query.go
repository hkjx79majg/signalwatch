package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"sort"
)

const queryPath = "/api/v1/query"

// ---- expression AST --------------------------------------------------------

// queryExpr is one parsed expression: either a bare selector (aggregate ==
// "") or one of sum/avg/min/max, optionally grouped by non-empty by labels.
type queryExpr struct {
	aggregate string
	by        []string
	metric    string
	selector  map[string]string
}

var queryAggregates = map[string]bool{
	"sum": true,
	"avg": true,
	"min": true,
	"max": true,
}

// ---- expression parser -----------------------------------------------------

type exprParser struct {
	s string
	p int
}

// parseQueryExpr parses the single expression accepted by GET /api/v1/query.
// Whitespace is ASCII only and may surround any punctuation or keyword. The
// grammar:
//
//	expr        := selector | aggregation
//	aggregation := ident ("by" "(" ident ("," ident)* ")")? "(" selector ")"
//	selector    := ident "{" (ident "=" jsonString ("," ident "=" jsonString)*)? "}"
//
// The aggregation identifier must be one of sum/avg/min/max. Duplicate
// matchers or grouping labels, nested aggregations, unknown functions and any
// trailing input are rejected.
func parseQueryExpr(in string) (*queryExpr, bool) {
	p := &exprParser{s: in}
	p.skipSpaces()

	word, ok := p.parseIdent()
	if !ok {
		return nil, false
	}
	p.skipSpaces()

	e := &queryExpr{}
	// A function name introduces an aggregation only when it is followed by a
	// call ("(" or a by clause); followed by "{" it is simply a metric named
	// like the keyword (metric writes impose no such reservation).
	if queryAggregates[word] && (p.next() == '(' || p.hasIdent("by")) {
		e.aggregate = word
		if p.hasIdent("by") {
			p.p += 2
			p.skipSpaces()
			if !p.expect('(') {
				return nil, false
			}
			seen := make(map[string]bool)
			for {
				p.skipSpaces()
				label, ok := p.parseIdent()
				if !ok {
					return nil, false
				}
				if seen[label] {
					return nil, false
				}
				seen[label] = true
				e.by = append(e.by, label)
				p.skipSpaces()
				switch p.next() {
				case ',':
					p.p++
					continue
				case ')':
					p.p++
				default:
					return nil, false
				}
				break
			}
			p.skipSpaces()
		}
		if !p.expect('(') {
			return nil, false
		}
		p.skipSpaces()
		if !p.parseSelectorInto(e) {
			return nil, false
		}
		p.skipSpaces()
		if !p.expect(')') {
			return nil, false
		}
	} else {
		// The first identifier is the metric name; the selector body follows.
		e.metric = word
		if !p.expect('{') {
			return nil, false
		}
		if !p.parseMatcherListInto(e) {
			return nil, false
		}
	}

	p.skipSpaces()
	if p.p != len(p.s) {
		return nil, false
	}
	return e, true
}

// parseSelectorInto parses metric "{" matcher-list "}" starting at the metric
// name; used for selectors nested inside an aggregation call.
func (p *exprParser) parseSelectorInto(e *queryExpr) bool {
	name, ok := p.parseIdent()
	if !ok {
		return false
	}
	e.metric = name
	p.skipSpaces()
	if !p.expect('{') {
		return false
	}
	return p.parseMatcherListInto(e)
}

// parseMatcherListInto parses matcher-list "}" with the opening "{" consumed.
func (p *exprParser) parseMatcherListInto(e *queryExpr) bool {
	e.selector = make(map[string]string)
	p.skipSpaces()
	if p.next() == '}' {
		p.p++
		return true
	}
	for {
		p.skipSpaces()
		key, ok := p.parseIdent()
		if !ok {
			return false
		}
		if _, dup := e.selector[key]; dup {
			return false
		}
		p.skipSpaces()
		if !p.expect('=') {
			return false
		}
		p.skipSpaces()
		value, ok := p.parseJSONString()
		if !ok {
			return false
		}
		e.selector[key] = value
		p.skipSpaces()
		switch p.next() {
		case ',':
			p.p++
			continue
		case '}':
			p.p++
			return true
		default:
			return false
		}
	}
}

func (p *exprParser) next() byte {
	if p.p >= len(p.s) {
		return 0
	}
	return p.s[p.p]
}

func (p *exprParser) expect(c byte) bool {
	if p.next() != c {
		return false
	}
	p.p++
	return true
}

func (p *exprParser) skipSpaces() {
	for p.p < len(p.s) && isASCIISpace(p.s[p.p]) {
		p.p++
	}
}

func isASCIISpace(c byte) bool {
	return c == ' ' || ('\t' <= c && c <= '\r')
}

func isIdentStart(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || '0' <= c && c <= '9'
}

// parseIdent consumes an identifier matching identPattern and reports whether
// one was present.
func (p *exprParser) parseIdent() (string, bool) {
	start := p.p
	if p.p >= len(p.s) || !isIdentStart(p.s[p.p]) {
		return "", false
	}
	for p.p < len(p.s) && isIdentCont(p.s[p.p]) {
		p.p++
	}
	return p.s[start:p.p], true
}

// hasIdent reports whether the text at the current position is exactly the
// given identifier (not part of a longer one), without consuming it.
func (p *exprParser) hasIdent(want string) bool {
	end := p.p + len(want)
	if end > len(p.s) || p.s[p.p:end] != want {
		return false
	}
	return end == len(p.s) || !isIdentCont(p.s[end])
}

// parseJSONString consumes a double-quoted JSON string literal and returns its
// decoded value. Escaping and Unicode handling follow JSON exactly.
func (p *exprParser) parseJSONString() (string, bool) {
	if p.next() != '"' {
		return "", false
	}
	start := p.p
	p.p++
	for p.p < len(p.s) {
		switch c := p.s[p.p]; {
		case c == '\\':
			p.p += 2
		case c == '"':
			lit := p.s[start : p.p+1]
			p.p++
			var value string
			if err := json.Unmarshal([]byte(lit), &value); err != nil {
				return "", false
			}
			return value, true
		default:
			p.p++
		}
	}
	return "", false
}

// ---- execution -------------------------------------------------------------

type querySample struct {
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

type queryResponse struct {
	ResultType string        `json:"result_type"`
	Result     []querySample `json:"result"`
}

type matchedSeries struct {
	labels map[string]string
	value  float64
}

// executeQuery evaluates a parsed expression against a single consistent
// store snapshot. Only current values of matching counter and gauge series
// participate; histograms are ignored. The third result is false when a
// selected value or an aggregate is not a finite number.
func (s *metricStore) executeQuery(e *queryExpr) ([]querySample, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	matched := make([]matchedSeries, 0)
	for _, cur := range s.series {
		if cur.typ != "counter" && cur.typ != "gauge" {
			continue
		}
		if cur.name != e.metric {
			continue
		}
		ok := true
		for k, v := range e.selector {
			got, exists := cur.labels[k]
			if !exists || got != v {
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
		matched = append(matched, matchedSeries{labels: copyLabels(cur.labels), value: cur.value})
	}

	if e.aggregate == "" {
		out := make([]querySample, 0, len(matched))
		for _, m := range matched {
			out = append(out, querySample{Labels: m.labels, Value: m.value})
		}
		sort.Slice(out, func(i, j int) bool {
			return compareLabels(out[i].Labels, out[j].Labels) < 0
		})
		return out, true
	}

	type groupAcc struct {
		labels map[string]string
		value  float64
		count  int
	}
	groups := make(map[string]*groupAcc)
	order := make([]string, 0)
	for _, m := range matched {
		projected := make(map[string]string, len(e.by))
		for _, label := range e.by {
			if v, ok := m.labels[label]; ok {
				projected[label] = v
			}
		}
		key := seriesKey("", projected)
		g, seen := groups[key]
		if !seen {
			g = &groupAcc{labels: projected, value: m.value, count: 1}
			groups[key] = g
			order = append(order, key)
			continue
		}
		g.count++
		switch e.aggregate {
		case "sum", "avg":
			g.value += m.value
		case "min":
			if m.value < g.value {
				g.value = m.value
			}
		case "max":
			if m.value > g.value {
				g.value = m.value
			}
		}
	}

	out := make([]querySample, 0, len(order))
	for _, key := range order {
		g := groups[key]
		value := g.value
		if e.aggregate == "avg" {
			value /= float64(g.count)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, false
		}
		out = append(out, querySample{Labels: g.labels, Value: value})
	}
	sort.Slice(out, func(i, j int) bool {
		return compareLabels(out[i].Labels, out[j].Labels) < 0
	})
	return out, true
}

// ---- HTTP handler ----------------------------------------------------------

func registerQueryHandler(mux *http.ServeMux) {
	mux.HandleFunc(queryPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}

		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			writeAPIError(w, "invalid_expression", http.StatusBadRequest)
			return
		}
		// Exactly one query parameter named expr, present exactly once.
		exprs := values["expr"]
		if len(values) != 1 || len(exprs) != 1 {
			writeAPIError(w, "invalid_expression", http.StatusBadRequest)
			return
		}
		expr, ok := parseQueryExpr(exprs[0])
		if !ok {
			writeAPIError(w, "invalid_expression", http.StatusBadRequest)
			return
		}

		result, finite := tenantStore(r).executeQuery(expr)
		if !finite {
			writeAPIError(w, "invalid_query_data", http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, http.StatusOK, queryResponse{ResultType: "vector", Result: result})
	})
}
