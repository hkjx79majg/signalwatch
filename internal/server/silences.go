package server

import (
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	silencesPath   = "/api/v1/silences"
	silencesPrefix = silencesPath + "/"
)

// silence is an immutable silence document. Replacements swap the pointer
// atomically, so readers never observe a half-updated silence.
type silence struct {
	id       string
	ruleIDs  []string
	startsAt time.Time
	endsAt   time.Time
	comment  string

	// raw timestamps are echoed verbatim so GET returns exactly what was PUT.
	startsAtRaw string
	endsAtRaw   string
}

// stateAt classifies the silence relative to the given evaluation instant.
func (s *silence) stateAt(now time.Time) string {
	switch {
	case now.Before(s.startsAt):
		return "upcoming"
	case now.Before(s.endsAt):
		return "active"
	default:
		return "expired"
	}
}

// activeAt reports whether the silence covers now, i.e.
// starts_at <= now < ends_at.
func (s *silence) activeAt(now time.Time) bool {
	return !now.Before(s.startsAt) && now.Before(s.endsAt)
}

// ---- JSON wire shapes ------------------------------------------------------

type silenceJSON struct {
	ID       string   `json:"id"`
	RuleIDs  []string `json:"rule_ids"`
	StartsAt string   `json:"starts_at"`
	EndsAt   string   `json:"ends_at"`
	Comment  string   `json:"comment"`
	State    string   `json:"state"`
}

func silenceWireJSON(s *silence, now time.Time) silenceJSON {
	return silenceJSON{
		ID:       s.id,
		RuleIDs:  append([]string(nil), s.ruleIDs...),
		StartsAt: s.startsAtRaw,
		EndsAt:   s.endsAtRaw,
		Comment:  s.comment,
		State:    s.stateAt(now),
	}
}

// ---- request decoding ------------------------------------------------------

type rawSilence struct {
	RuleIDs  *[]string `json:"rule_ids"`
	StartsAt *string   `json:"starts_at"`
	EndsAt   *string   `json:"ends_at"`
	Comment  *string   `json:"comment"`
}

// decodeSilence parses and fully validates a silence body. It never returns a
// partially validated silence.
func decodeSilence(body []byte) (*silence, bool) {
	var raw rawSilence
	if !strictDecode(body, &raw) {
		return nil, false
	}
	if raw.RuleIDs == nil || raw.StartsAt == nil || raw.EndsAt == nil || raw.Comment == nil {
		return nil, false
	}

	ruleIDs := *raw.RuleIDs
	if len(ruleIDs) == 0 {
		return nil, false
	}
	seen := make(map[string]bool, len(ruleIDs))
	for _, id := range ruleIDs {
		if !identPattern.MatchString(id) || seen[id] {
			return nil, false
		}
		seen[id] = true
	}

	startsAt, err := time.Parse(time.RFC3339, *raw.StartsAt)
	if err != nil {
		return nil, false
	}
	endsAt, err := time.Parse(time.RFC3339, *raw.EndsAt)
	if err != nil {
		return nil, false
	}
	if !startsAt.Before(endsAt) {
		return nil, false
	}

	return &silence{
		ruleIDs:     append([]string(nil), ruleIDs...),
		startsAt:    startsAt,
		endsAt:      endsAt,
		comment:     *raw.Comment,
		startsAtRaw: *raw.StartsAt,
		endsAtRaw:   *raw.EndsAt,
	}, true
}

// ---- store operations ------------------------------------------------------

func (s *metricStore) putSilence(sil *silence) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.silences[sil.id]
	s.silences[sil.id] = sil
	return !existed
}

func (s *metricStore) getSilence(id string) (*silence, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sil, ok := s.silences[id]
	return sil, ok
}

func (s *metricStore) deleteSilence(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.silences[id]; !ok {
		return false
	}
	delete(s.silences, id)
	return true
}

// snapshotSilences returns all silences sorted by id. Silence objects are
// immutable, so the pointers stay safe after the lock is released.
func (s *metricStore) snapshotSilences() []*silence {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*silence, 0, len(s.silences))
	for _, sil := range s.silences {
		out = append(out, sil)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// ---- HTTP handlers ---------------------------------------------------------

func registerSilenceHandlers(mux *http.ServeMux) {
	mux.HandleFunc(silencesPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		now := time.Now()
		silences := tenantStore(r).snapshotSilences()
		body := make([]silenceJSON, 0, len(silences))
		for _, sil := range silences {
			body = append(body, silenceWireJSON(sil, now))
		}
		writeJSON(w, http.StatusOK, map[string]any{"silences": body})
	})

	mux.HandleFunc(silencesPrefix, func(w http.ResponseWriter, r *http.Request) {
		store := tenantStore(r)
		id := strings.TrimPrefix(r.URL.Path, silencesPrefix)
		switch r.Method {
		case http.MethodGet, http.MethodDelete:
			// Reads and deletions can only address an existing, syntactically
			// valid silence id; anything else is simply not found.
			if id == "" || strings.Contains(id, "/") || !identPattern.MatchString(id) {
				writeAPIError(w, "silence_not_found", http.StatusNotFound)
				return
			}
			if r.Method == http.MethodGet {
				handleSilenceGet(w, id, store)
			} else {
				handleSilenceDelete(w, id, store)
			}
		case http.MethodPut:
			// A structurally nested path is outside this resource subtree;
			// an identifier-shaped but invalid id is a bad identifier.
			if id == "" || strings.Contains(id, "/") {
				writeAPIError(w, "silence_not_found", http.StatusNotFound)
				return
			}
			if !identPattern.MatchString(id) {
				writeAPIError(w, "invalid_silence", http.StatusBadRequest)
				return
			}
			handleSilencePut(w, r, id, store)
		default:
			w.Header().Set("Allow", "GET, PUT, DELETE")
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
		}
	})
}

func handleSilenceGet(w http.ResponseWriter, id string, store *metricStore) {
	sil, ok := store.getSilence(id)
	if !ok {
		writeAPIError(w, "silence_not_found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, silenceWireJSON(sil, time.Now()))
}

func handleSilencePut(w http.ResponseWriter, r *http.Request, id string, store *metricStore) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, "invalid_silence", http.StatusBadRequest)
		return
	}

	sil, ok := decodeSilence(body)
	if !ok {
		writeAPIError(w, "invalid_silence", http.StatusBadRequest)
		return
	}
	sil.id = id

	created := store.putSilence(sil)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, silenceWireJSON(sil, time.Now()))
}

func handleSilenceDelete(w http.ResponseWriter, id string, store *metricStore) {
	if !store.deleteSilence(id) {
		writeAPIError(w, "silence_not_found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// silenceRuleHits maps a rule id to the ids of every active silence covering
// it at now; silence id lists are sorted lexicographically. Built from a
// silence snapshot taken under the same lock as alert evaluation.
func activeSilencesByRule(silences []*silence, now time.Time) map[string][]string {
	hits := make(map[string][]string)
	for _, sil := range silences {
		if !sil.activeAt(now) {
			continue
		}
		for _, ruleID := range sil.ruleIDs {
			hits[ruleID] = append(hits[ruleID], sil.id)
		}
	}
	for ruleID := range hits {
		sort.Strings(hits[ruleID])
	}
	return hits
}
