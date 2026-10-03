package server

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	discoveryTargetsPath       = "/api/v1/discovery-targets"
	discoveryTargetsReloadPath = "/api/v1/discovery-targets/reload"

	maxDiscoveryTargets = 1000
)

// discoveryTarget is one immutable scrape target. Snapshots are swapped
// atomically, so readers never observe a half-updated target list.
type discoveryTarget struct {
	id      string
	url     string // echoed verbatim from the accepted request
	labels  map[string]string
	enabled bool
}

// discoverySnapshot is the tenant's full target configuration plus the
// generation that last changed its content.
type discoverySnapshot struct {
	generation int64
	targets    []discoveryTarget // sorted by id, immutable after publication
}

// emptyDiscoverySnapshot is the restart baseline: generation 0, no targets.
func emptyDiscoverySnapshot() *discoverySnapshot {
	return &discoverySnapshot{generation: 0, targets: []discoveryTarget{}}
}

// ---- JSON wire shapes ------------------------------------------------------

type discoveryTargetJSON struct {
	ID      string            `json:"id"`
	URL     string            `json:"url"`
	Labels  map[string]string `json:"labels"`
	Enabled bool              `json:"enabled"`
}

func discoverySnapshotWireJSON(snap *discoverySnapshot) map[string]any {
	targets := make([]discoveryTargetJSON, 0, len(snap.targets))
	for _, t := range snap.targets {
		targets = append(targets, discoveryTargetJSON{
			ID:      t.id,
			URL:     t.url,
			Labels:  copyLabels(t.labels),
			Enabled: t.enabled,
		})
	}
	return map[string]any{"generation": snap.generation, "targets": targets}
}

// ---- request decoding ------------------------------------------------------

type discoveryEnvelope struct {
	Targets *[]json.RawMessage `json:"targets"`
}

type rawDiscoveryTarget struct {
	ID      *string            `json:"id"`
	URL     *string            `json:"url"`
	Labels  *map[string]string `json:"labels"`
	Enabled *bool              `json:"enabled"`
}

// decodeDiscoveryTargets parses and fully validates a reload body. It never
// returns a partially validated snapshot; the returned targets are sorted by
// id so content comparison and responses share one canonical order.
func decodeDiscoveryTargets(body []byte) ([]discoveryTarget, bool) {
	var env discoveryEnvelope
	if !strictDecode(body, &env) || env.Targets == nil {
		return nil, false
	}
	raw := *env.Targets
	if len(raw) > maxDiscoveryTargets {
		return nil, false
	}

	targets := make([]discoveryTarget, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		var rt rawDiscoveryTarget
		if !strictDecode(item, &rt) {
			return nil, false
		}
		if rt.ID == nil || rt.URL == nil || rt.Labels == nil || rt.Enabled == nil {
			return nil, false
		}
		id := *rt.ID
		if !identPattern.MatchString(id) || seen[id] {
			return nil, false
		}
		seen[id] = true

		if !validDiscoveryURL(*rt.URL) {
			return nil, false
		}

		labels := *rt.Labels
		if labels == nil {
			return nil, false // explicit null labels
		}
		for k := range labels {
			if !identPattern.MatchString(k) {
				return nil, false
			}
		}

		targets = append(targets, discoveryTarget{
			id:      id,
			url:     *rt.URL,
			labels:  copyLabels(labels),
			enabled: *rt.Enabled,
		})
	}

	sort.Slice(targets, func(i, j int) bool { return targets[i].id < targets[j].id })
	return targets, true
}

// validDiscoveryURL accepts only absolute http/https URLs with a non-empty
// host and no userinfo, fragment or invalid port.
func validDiscoveryURL(raw string) bool {
	if raw == "" || strings.Contains(raw, "#") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	if u.User != nil || u.Hostname() == "" {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n > 65535 {
			return false
		}
	}
	return true
}

// ---- store operations ------------------------------------------------------

// discoverySnapshotNow returns the tenant's current immutable snapshot.
func (s *metricStore) discoverySnapshotNow() *discoverySnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.discovery
}

// reloadDiscoveryTargets atomically replaces the snapshot after validation.
// The generation advances only when the content — ids, urls, label sets or
// enabled flags — actually changes; target and label-key order never count.
func (s *metricStore) reloadDiscoveryTargets(targets []discoveryTarget) *discoverySnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.discovery
	if !discoveryTargetsEqual(cur.targets, targets) {
		cur = &discoverySnapshot{generation: cur.generation + 1, targets: targets}
		s.discovery = cur
	}
	return cur
}

// discoveryTargetsEqual compares two id-sorted target lists by content.
func discoveryTargetsEqual(a, b []discoveryTarget) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].id != b[i].id || a[i].url != b[i].url || a[i].enabled != b[i].enabled {
			return false
		}
		if len(a[i].labels) != len(b[i].labels) {
			return false
		}
		for k, v := range a[i].labels {
			if bv, ok := b[i].labels[k]; !ok || bv != v {
				return false
			}
		}
	}
	return true
}

// ---- HTTP handlers ---------------------------------------------------------

func registerDiscoveryTargetHandlers(mux *http.ServeMux) {
	mux.HandleFunc(discoveryTargetsPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		// The query endpoint takes no parameters at all.
		if r.URL.RawQuery != "" {
			writeAPIError(w, "invalid_discovery_query", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, discoverySnapshotWireJSON(tenantStore(r).discoverySnapshotNow()))
	})

	mux.HandleFunc(discoveryTargetsReloadPath, func(w http.ResponseWriter, r *http.Request) {
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
			writeAPIError(w, "invalid_discovery_targets", http.StatusBadRequest)
			return
		}

		targets, ok := decodeDiscoveryTargets(body)
		if !ok {
			writeAPIError(w, "invalid_discovery_targets", http.StatusBadRequest)
			return
		}

		snap := tenantStore(r).reloadDiscoveryTargets(targets)
		writeJSON(w, http.StatusOK, discoverySnapshotWireJSON(snap))
	})
}
