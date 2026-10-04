package server

import (
	"encoding/json"
	"io"
	"math"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const discoveryTargetsScrapePath = "/api/v1/discovery-targets/scrape"

// scrapeHTTPClient fetches target expositions: GET only, five second timeout,
// and redirects are never followed (a 3xx is simply a non-200 response).
var scrapeHTTPClient = &http.Client{
	Timeout: 5 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// ---- JSON wire shapes ------------------------------------------------------

type scrapeResultJSON struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Accepted int    `json:"accepted"`
	Code     string `json:"code,omitempty"`
}

// ---- request decoding ------------------------------------------------------

type scrapeEnvelope struct {
	TargetIDs *[]json.RawMessage `json:"target_ids"`
}

// decodeScrapeRequest parses and fully validates a scrape body. The envelope
// must contain exactly target_ids; every id must be a well-formed, unique
// identifier. An empty array selects all enabled targets.
func decodeScrapeRequest(body []byte) ([]string, bool) {
	var env scrapeEnvelope
	if !strictDecode(body, &env) || env.TargetIDs == nil {
		return nil, false
	}
	raw := *env.TargetIDs
	ids := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		var id string
		if err := json.Unmarshal(item, &id); err != nil {
			return nil, false
		}
		if !identPattern.MatchString(id) || seen[id] {
			return nil, false
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, true
}

// ---- exposition parsing ----------------------------------------------------

// expositionSample is one parsed sample line before static-label merging.
type expositionSample struct {
	name   string
	typ    string
	labels map[string]string
	value  float64
}

// parseExposition parses a Prometheus text exposition. Every sample's type is
// resolved from the most recent TYPE declaration for its metric and must be
// counter or gauge; unknown types, duplicate labels, invalid identifiers and
// non-finite numbers all fail the whole exposition.
func parseExposition(text string) ([]expositionSample, bool) {
	types := make(map[string]string)
	samples := make([]expositionSample, 0)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			fields := strings.Fields(line[1:])
			if len(fields) > 0 && fields[0] == "TYPE" {
				if len(fields) != 3 || !identPattern.MatchString(fields[1]) {
					return nil, false
				}
				types[fields[1]] = fields[2]
			}
			continue
		}

		name, labels, valueText, ok := parseSampleLine(line)
		if !ok || !identPattern.MatchString(name) {
			return nil, false
		}
		typ, declared := types[name]
		if !declared || (typ != "counter" && typ != "gauge") {
			return nil, false
		}
		value, err := strconv.ParseFloat(valueText, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, false
		}
		samples = append(samples, expositionSample{name: name, typ: typ, labels: labels, value: value})
	}
	return samples, true
}

// parseSampleLine parses "name{k=\"v\",...} value" or "name value". The value
// must be the final token; trailing content (such as a timestamp) is rejected.
func parseSampleLine(line string) (name string, labels map[string]string, valueText string, ok bool) {
	i := 0
	for i < len(line) && line[i] != '{' && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	name = line[:i]
	labels = make(map[string]string)

	if i < len(line) && line[i] == '{' {
		i++
		for {
			if i < len(line) && line[i] == '}' {
				i++
				break
			}
			start := i
			for i < len(line) && line[i] != '=' && line[i] != ',' && line[i] != '}' {
				i++
			}
			key := line[start:i]
			if !identPattern.MatchString(key) {
				return "", nil, "", false
			}
			if i >= len(line) || line[i] != '=' {
				return "", nil, "", false
			}
			i++
			if i >= len(line) || line[i] != '"' {
				return "", nil, "", false
			}
			i++
			var sb strings.Builder
			closed := false
			for i < len(line) {
				c := line[i]
				i++
				switch c {
				case '"':
					closed = true
				case '\\':
					if i >= len(line) {
						return "", nil, "", false
					}
					switch line[i] {
					case 'n':
						sb.WriteByte('\n')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					default:
						return "", nil, "", false
					}
					i++
				default:
					sb.WriteByte(c)
				}
				if closed {
					break
				}
			}
			if !closed {
				return "", nil, "", false
			}
			if _, dup := labels[key]; dup {
				return "", nil, "", false
			}
			labels[key] = sb.String()

			if i < len(line) && line[i] == ',' {
				i++
				continue
			}
			if i < len(line) && line[i] == '}' {
				i++
				break
			}
			return "", nil, "", false
		}
	}

	if i >= len(line) || (line[i] != ' ' && line[i] != '\t') {
		return "", nil, "", false
	}
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	start := i
	for i < len(line) && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	valueText = line[start:i]
	if valueText == "" || i != len(line) {
		return "", nil, "", false
	}
	return name, labels, valueText, true
}

// mergeScrapeLabels combines the target's static labels with one sample's
// labels and adds the reserved target_id label. It fails when either source
// uses target_id itself or both sources set the same label to different
// values.
func mergeScrapeLabels(static, sample map[string]string, targetID string) (map[string]string, bool) {
	merged := make(map[string]string, len(static)+len(sample)+1)
	for k, v := range static {
		if k == "target_id" {
			return nil, false
		}
		merged[k] = v
	}
	for k, v := range sample {
		if k == "target_id" {
			return nil, false
		}
		if sv, exists := merged[k]; exists && sv != v {
			return nil, false
		}
		merged[k] = v
	}
	merged["target_id"] = targetID
	return merged, true
}

// ---- store operations ------------------------------------------------------

// scrapedSample is one exposition sample after label merging, ready to commit.
// For counters value is the scraped cumulative reading, not a delta.
type scrapedSample struct {
	key    string
	name   string
	typ    string
	labels map[string]string
	value  float64
}

// commitScrape atomically commits one target's batch. Counter readings are
// cumulative: the first reading commits in full, a non-decreasing reading
// commits the delta against the baseline, and a decrease is treated as a
// reset and commits the new reading in full. The baseline advances only when
// the whole batch commits; a type conflict against existing series leaves
// both metrics and baselines untouched.
func (s *metricStore) commitScrape(batch []scrapedSample, ts time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// First pass: type-conflict detection against committed state and
	// earlier samples in the same batch.
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
				return false
			}
			resolved[sm.key] = existing
			continue
		}
		resolved[sm.key] = &series{typ: sm.typ}
	}

	// Second pass: commit in order.
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
		case "counter":
			base, seen := s.scrapeBaseline[sm.key]
			delta := sm.value
			if seen && sm.value >= base {
				delta = sm.value - base
			}
			cur.value += delta
			s.scrapeBaseline[sm.key] = sm.value
			written = delta
		case "gauge":
			cur.value = sm.value
			written = sm.value
		}
		cur.history = append(cur.history, historyPoint{ts: ts, value: written})
	}
	return true
}

// ---- scrape execution ------------------------------------------------------

// scrapeOneTarget fetches, parses and commits a single target. Failures never
// affect other targets; each failure maps to its own result code.
func scrapeOneTarget(store *metricStore, target discoveryTarget) scrapeResultJSON {
	result := scrapeResultJSON{ID: target.id, Status: "ok"}
	fail := func(code string) scrapeResultJSON {
		return scrapeResultJSON{ID: target.id, Status: "error", Accepted: 0, Code: code}
	}

	req, err := http.NewRequest(http.MethodGet, target.url, nil)
	if err != nil {
		return fail("target_unreachable")
	}
	resp, err := scrapeHTTPClient.Do(req)
	if err != nil {
		return fail("target_unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail("target_http_error")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail("target_unreachable")
	}

	parsed, ok := parseExposition(string(body))
	if !ok {
		return fail("invalid_exposition")
	}

	batch := make([]scrapedSample, 0, len(parsed))
	for _, sm := range parsed {
		labels, ok := mergeScrapeLabels(target.labels, sm.labels, target.id)
		if !ok {
			return fail("label_conflict")
		}
		batch = append(batch, scrapedSample{
			key:    seriesKey(sm.name, labels),
			name:   sm.name,
			typ:    sm.typ,
			labels: labels,
			value:  sm.value,
		})
	}

	if !store.commitScrape(batch, time.Now()) {
		return fail("metric_conflict")
	}
	result.Accepted = len(batch)
	return result
}

// ---- HTTP handler ----------------------------------------------------------

func registerDiscoveryScrapeHandler(mux *http.ServeMux) {
	mux.HandleFunc(discoveryTargetsScrapePath, func(w http.ResponseWriter, r *http.Request) {
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
		ids, ok := decodeScrapeRequest(body)
		if !ok {
			writeAPIError(w, "invalid_discovery_scrape", http.StatusBadRequest)
			return
		}

		// Pin the generation and target list for the whole request; a
		// concurrent reload does not change what this scrape sees.
		store := tenantStore(r)
		snap := store.discoverySnapshotNow()

		var selected []discoveryTarget
		if len(ids) == 0 {
			for _, t := range snap.targets {
				if t.enabled {
					selected = append(selected, t)
				}
			}
		} else {
			byID := make(map[string]discoveryTarget, len(snap.targets))
			for _, t := range snap.targets {
				byID[t.id] = t
			}
			for _, id := range ids {
				if _, found := byID[id]; !found {
					writeAPIError(w, "discovery_target_not_found", http.StatusNotFound)
					return
				}
			}
			for _, id := range ids {
				if !byID[id].enabled {
					writeAPIError(w, "discovery_target_disabled", http.StatusConflict)
					return
				}
			}
			for _, id := range ids {
				selected = append(selected, byID[id])
			}
			sort.Slice(selected, func(i, j int) bool { return selected[i].id < selected[j].id })
		}

		results := make([]scrapeResultJSON, 0, len(selected))
		for _, t := range selected {
			results = append(results, scrapeOneTarget(store, t))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"generation": snap.generation,
			"results":    results,
		})
	})
}
