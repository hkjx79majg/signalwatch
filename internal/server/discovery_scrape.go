package server

import (
	"context"
	"io"
	"math"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	scrapeHTTPTimeout     = 5 * time.Second
	reservedScrapeLabel   = "target_id"
	scrapeStatusOK        = "ok"
	scrapeStatusError     = "error"
	unsupportedTypeMarker = "__unsupported__"
)

// scrapeHTTPClient never follows redirects (a 3xx response is therefore
// observable as a non-200 status) and bounds every target exchange.
var scrapeHTTPClient = &http.Client{
	Timeout: scrapeHTTPTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// ---- wire shapes -----------------------------------------------------------

type discoveryScrapeEnvelope struct {
	TargetIDs *[]string `json:"target_ids"`
}

type scrapeResultJSON struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Accepted int    `json:"accepted"`
	Code     string `json:"code,omitempty"`
}

// expositionSample is one parsed metric{labels} value line. The type is the
// counter/gauge decided by the nearest preceding TYPE declaration.
type expositionSample struct {
	name   string
	typ    string
	labels map[string]string
	value  float64
}

// ---- HTTP handler ----------------------------------------------------------

func handleDiscoveryScrape(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, "invalid_discovery_scrape", http.StatusBadRequest)
		return
	}

	ids, ok := decodeDiscoveryScrapeBody(body)
	if !ok {
		writeAPIError(w, "invalid_discovery_scrape", http.StatusBadRequest)
		return
	}

	store := tenantStore(r)

	// Pin the generation and the full target snapshot once. A concurrent
	// reload swaps the snapshot pointer but never mutates this one, so the
	// rest of the request — selection, URLs, labels and the response
	// generation — observes exactly this configuration.
	snap := store.discoverySnapshotNow()
	targets, code := selectScrapeTargets(snap, ids)
	if code != "" {
		switch code {
		case "discovery_target_not_found":
			writeAPIError(w, code, http.StatusNotFound)
		case "discovery_target_disabled":
			writeAPIError(w, code, http.StatusConflict)
		}
		return
	}

	scrapedAt := time.Now()
	results := make([]scrapeResultJSON, 0, len(targets))
	for _, t := range targets {
		results = append(results, scrapeOneTarget(r.Context(), store, snap.generation, t, scrapedAt))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"generation": snap.generation,
		"results":    results,
	})
}

// decodeDiscoveryScrapeBody accepts exactly one JSON object whose sole field
// target_ids is an array of valid, unique identifiers. An empty array is
// legal and means "every enabled target".
func decodeDiscoveryScrapeBody(body []byte) ([]string, bool) {
	var env discoveryScrapeEnvelope
	if !strictDecode(body, &env) || env.TargetIDs == nil {
		return nil, false
	}
	ids := *env.TargetIDs
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !identPattern.MatchString(id) || seen[id] {
			return nil, false
		}
		seen[id] = true
	}
	sort.Strings(ids)
	return ids, true
}

// selectScrapeTargets resolves the pinned snapshot into the targets to
// scrape, already in id order. Explicit ids are validated in two passes —
// every id must exist before any enabled check — and then collected in id
// order. An empty selection means every enabled target of the snapshot.
func selectScrapeTargets(snap *discoverySnapshot, ids []string) ([]discoveryTarget, string) {
	if len(ids) == 0 {
		out := make([]discoveryTarget, 0)
		for _, t := range snap.targets {
			if t.enabled {
				out = append(out, t)
			}
		}
		return out, ""
	}

	byID := make(map[string]discoveryTarget, len(snap.targets))
	for _, t := range snap.targets {
		byID[t.id] = t
	}
	for _, id := range ids {
		if _, exists := byID[id]; !exists {
			return nil, "discovery_target_not_found"
		}
	}
	out := make([]discoveryTarget, 0, len(ids))
	for _, id := range ids {
		t := byID[id]
		if !t.enabled {
			return nil, "discovery_target_disabled"
		}
		out = append(out, t)
	}
	return out, ""
}

// scrapeOneTarget fetches, parses and commits one target. Every failure is
// local to the target: it yields an error result without touching any other
// target's outcome.
func scrapeOneTarget(ctx context.Context, store *metricStore, generation int64, t discoveryTarget, at time.Time) scrapeResultJSON {
	fail := func(code string) scrapeResultJSON {
		return scrapeResultJSON{ID: t.id, Status: scrapeStatusError, Accepted: 0, Code: code}
	}

	status, payload, reached := fetchScrapeTarget(ctx, t.url)
	if !reached {
		return fail("target_unreachable")
	}
	if status != http.StatusOK {
		return fail("target_http_error")
	}

	samples, ok := parseExposition(payload)
	if !ok {
		return fail("invalid_exposition")
	}

	batch := make([]validatedSample, 0, len(samples))
	for _, sm := range samples {
		labels, ok := mergeScrapeLabels(t.labels, sm.labels, t.id)
		if !ok {
			return fail("label_conflict")
		}
		batch = append(batch, validatedSample{
			key:    seriesKey(sm.name, labels),
			name:   sm.name,
			typ:    sm.typ,
			labels: labels,
			value:  sm.value,
			ts:     at,
		})
	}

	if code := store.commitScrape(generation, batch); code != "" {
		return fail(code)
	}
	return scrapeResultJSON{ID: t.id, Status: scrapeStatusOK, Accepted: len(batch)}
}

// fetchScrapeTarget performs the single GET against a target. reached is
// false for any transport-level failure (including the 5s timeout); a
// delivered response — whatever its status — reports reached with the body.
func fetchScrapeTarget(ctx context.Context, targetURL string) (status int, body []byte, reached bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return 0, nil, false
	}
	resp, err := scrapeHTTPClient.Do(req)
	if err != nil {
		return 0, nil, false
	}
	defer resp.Body.Close()

	// The client's overall 5s timeout bounds the read as well.
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, false
	}
	return resp.StatusCode, payload, true
}

// ---- exposition parsing ----------------------------------------------------

// parseExposition parses Prometheus text exposition: TYPE comments and
// metric{labels} number / metric number sample lines. A sample's type is its
// metric name's nearest preceding TYPE declaration and must be counter or
// gauge. Duplicate labels, illegal identifiers, non-finite numbers, stray
// tokens and malformed lines all reject the whole exposition.
func parseExposition(data []byte) ([]expositionSample, bool) {
	types := make(map[string]string)
	samples := make([]expositionSample, 0)

	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(rawLine, "\r")
		i := skipExpositionSpace(line, 0)
		if i == len(line) {
			continue
		}
		if line[i] == '#' {
			if !parseExpositionTypeLine(line, types) {
				return nil, false
			}
			continue
		}

		name, j, ok := readExpositionIdent(line, i)
		if !ok {
			return nil, false
		}
		j = skipExpositionSpace(line, j)

		labels := map[string]string{}
		if j < len(line) && line[j] == '{' {
			// A closing brace is itself a delimiter, so the value may follow
			// without whitespace, e.g. m{}1.
			labels, j, ok = readExpositionLabels(line, j+1)
			if !ok {
				return nil, false
			}
			j = skipExpositionSpace(line, j)
		} else if j == i+len(name) {
			// Without a label block a sample-less name has no value here.
			return nil, false
		}

		end := j
		for end < len(line) && !isExpositionSpace(line[end]) {
			end++
		}
		token := line[j:end]
		value, err := strconv.ParseFloat(token, 64)
		if token == "" || err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, false
		}
		if skipExpositionSpace(line, end) != len(line) {
			return nil, false // trailing tokens (e.g. an explicit timestamp)
		}

		typ, declared := types[name]
		if !declared || (typ != "counter" && typ != "gauge") {
			return nil, false
		}
		samples = append(samples, expositionSample{
			name:   name,
			typ:    typ,
			labels: labels,
			value:  value,
		})
	}
	return samples, true
}

// parseExpositionTypeLine handles comments. "# TYPE name type" lines declare
// a metric's type: counter/gauge are recorded, the other standard
// Prometheus types and any unrecognized word are recorded as unsupported so
// a later sample of that name fails the type check. HELP lines, free-form
// comments and malformed comments carry no samples and are ignored.
func parseExpositionTypeLine(line string, types map[string]string) bool {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[1] != "TYPE" {
		return true
	}
	if len(fields) != 4 || !identPattern.MatchString(fields[2]) {
		return true
	}
	switch fields[3] {
	case "counter", "gauge":
		types[fields[2]] = fields[3]
	default:
		types[fields[2]] = unsupportedTypeMarker
	}
	return true
}

// readExpositionIdent consumes [a-zA-Z_][a-zA-Z0-9_]* at offset i.
func readExpositionIdent(line string, i int) (string, int, bool) {
	start := i
	for i < len(line) {
		c := line[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > start && c >= '0' && c <= '9') {
			i++
			continue
		}
		break
	}
	if i == start || !identPattern.MatchString(line[start:i]) {
		return "", 0, false
	}
	return line[start:i], i, true
}

// readExpositionLabels parses the inside of a {...} label block starting
// after the opening brace, returning the labels and the offset just past the
// closing brace. Values use the Prometheus escapes \\, \" and \n.
func readExpositionLabels(line string, i int) (map[string]string, int, bool) {
	labels := map[string]string{}
	for {
		i = skipExpositionSpace(line, i)
		if i >= len(line) {
			return nil, 0, false
		}
		if line[i] == '}' {
			return labels, i + 1, true
		}
		key, j, ok := readExpositionIdent(line, i)
		if !ok {
			return nil, 0, false
		}
		if _, dup := labels[key]; dup {
			return nil, 0, false
		}
		j = skipExpositionSpace(line, j)
		if j >= len(line) || line[j] != '=' {
			return nil, 0, false
		}
		j = skipExpositionSpace(line, j+1)
		if j >= len(line) || line[j] != '"' {
			return nil, 0, false
		}
		value, j, ok := readExpositionQuoted(line, j+1)
		if !ok {
			return nil, 0, false
		}
		labels[key] = value
		j = skipExpositionSpace(line, j)
		if j >= len(line) {
			return nil, 0, false
		}
		switch line[j] {
		case ',':
			i = j + 1
		case '}':
			return labels, j + 1, true
		default:
			return nil, 0, false
		}
	}
}

// readExpositionQuoted consumes a double-quoted label value starting after
// the opening quote, returning the unescaped value and the offset past the
// closing quote.
func readExpositionQuoted(line string, i int) (string, int, bool) {
	var b strings.Builder
	for i < len(line) {
		switch c := line[i]; {
		case c == '"':
			return b.String(), i + 1, true
		case c == '\\':
			if i+1 >= len(line) {
				return "", 0, false
			}
			switch line[i+1] {
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			case 'n':
				b.WriteByte('\n')
			default:
				return "", 0, false
			}
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", 0, false
}

func skipExpositionSpace(line string, i int) int {
	for i < len(line) && isExpositionSpace(line[i]) {
		i++
	}
	return i
}

func isExpositionSpace(c byte) bool {
	return c == ' ' || c == '\t'
}

// ---- label merge -----------------------------------------------------------

// mergeScrapeLabels combines the target's static labels with one sample's
// labels. A key present in both with different values, or use of the
// reserved target_id key in either source, is a conflict. Otherwise
// target_id=<target id> is added.
func mergeScrapeLabels(static, sample map[string]string, targetID string) (map[string]string, bool) {
	if _, reserved := static[reservedScrapeLabel]; reserved {
		return nil, false
	}
	if _, reserved := sample[reservedScrapeLabel]; reserved {
		return nil, false
	}
	merged := make(map[string]string, len(static)+len(sample)+1)
	for k, v := range sample {
		merged[k] = v
	}
	for k, v := range static {
		if existing, ok := merged[k]; ok && existing != v {
			return nil, false
		}
		merged[k] = v
	}
	merged[reservedScrapeLabel] = targetID
	return merged, true
}

// commitScrape validates and applies one target's scrape batch atomically.
// Existing series must keep their type; on conflict nothing — metrics or
// baselines — changes. Gauges overwrite the current value. A counter is a
// cumulative reading: the first time the series is seen under the pinned
// generation the full value is written, later non-decreasing readings add
// the delta, and a decrease is treated as a reset (the new full value).
func (s *metricStore) commitScrape(generation int64, batch []validatedSample) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	// First pass: detect type conflicts against committed state and earlier
	// samples of the same series within this batch.
	resolved := make(map[string]*series, len(batch))
	for _, sm := range batch {
		existing, ok := s.series[sm.key]
		if !ok {
			if shadow, seen := resolved[sm.key]; seen {
				existing, ok = shadow, true
			}
		}
		if ok {
			if existing.typ != sm.typ {
				return "metric_conflict"
			}
			resolved[sm.key] = existing
			continue
		}
		resolved[sm.key] = &series{typ: sm.typ}
	}

	baselines, ok := s.scrapeBaselines[generation]
	if !ok {
		baselines = make(map[string]float64)
		s.scrapeBaselines[generation] = baselines
	}

	// Second pass: commit in array order.
	for _, sm := range batch {
		cur, ok := s.series[sm.key]
		if !ok {
			cur = &series{
				name:   sm.name,
				typ:    sm.typ,
				labels: sm.labels,
			}
			s.series[sm.key] = cur
		}

		var written float64
		switch sm.typ {
		case "gauge":
			written = sm.value
		case "counter":
			prev, seen := baselines[sm.key]
			switch {
			case !seen:
				written = sm.value // first reading under this generation
			case sm.value >= prev:
				written = sm.value - prev
			default:
				written = sm.value // counter reset
			}
		}
		baselines[sm.key] = sm.value

		switch sm.typ {
		case "counter":
			cur.value += written
		case "gauge":
			cur.value = written
		}
		cur.history = append(cur.history, historyPoint{ts: sm.ts, value: written})
	}
	return ""
}
