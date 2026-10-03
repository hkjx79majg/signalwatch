package server

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
)

const (
	discoveryTargetsPath = "/api/v1/discovery-targets"
	discoveryReloadPath  = "/api/v1/discovery-targets/reload"

	maxDiscoveryTargets = 1000
)

// discoveryTarget is one validated static target. Targets are immutable once
// committed: reloads swap the whole snapshot atomically, so readers never
// observe a half-updated configuration.
type discoveryTarget struct {
	id      string
	url     string
	labels  map[string]string
	enabled bool
}

// discoverySnapshot is the tenant's full static target configuration.
// generation starts at 0 with an empty target list and increments only when
// the effective configuration changes; reordering targets or label keys does
// not count as a change.
type discoverySnapshot struct {
	generation int64
	targets    []discoveryTarget // sorted by id, immutable after commit
}

// ---- JSON wire shapes ------------------------------------------------------

type discoveryTargetJSON struct {
	ID      string            `json:"id"`
	URL     string            `json:"url"`
	Labels  map[string]string `json:"labels"`
	Enabled bool              `json:"enabled"`
}

func discoveryWireJSON(snap discoverySnapshot) map[string]any {
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

type rawDiscoveryEnvelope struct {
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
// id so equality and responses are order-independent.
func decodeDiscoveryTargets(body []byte) ([]discoveryTarget, bool) {
	var env rawDiscoveryEnvelope
	if !strictDecode(body, &env) || env.Targets == nil {
		return nil, false
	}
	raws := *env.Targets
	if len(raws) > maxDiscoveryTargets {
		return nil, false
	}

	targets := make([]discoveryTarget, 0, len(raws))
	seen := make(map[string]bool, len(raws))
	for _, raw := range raws {
		var rt rawDiscoveryTarget
		if !strictDecode(raw, &rt) {
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

// validDiscoveryURL reports whether raw is an absolute http or https URL with
// a non-empty host and no userinfo, fragment or illegal port.
func validDiscoveryURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		return false
	}
	if u.Hostname() == "" {
		return false
	}
	// url.Parse already rejects non-numeric ports; range-check the rest.
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n > 65535 {
			return false
		}
	}
	return true
}

// ---- store operations ------------------------------------------------------

// reloadDiscovery atomically replaces the tenant's snapshot. The generation
// increments only when the effective configuration changed; the committed
// snapshot is returned either way.
func (s *metricStore) reloadDiscovery(targets []discoveryTarget) discoverySnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !discoveryTargetsEqual(s.discovery.targets, targets) {
		s.discovery.generation++
	}
	s.discovery.targets = targets
	return s.discovery
}

// snapshotDiscovery returns the tenant's current snapshot. The targets slice
// is immutable after commit, so it stays safe after the lock is released.
func (s *metricStore) snapshotDiscovery() discoverySnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.discovery
}

// discoveryTargetsEqual compares two id-sorted snapshots for effective
// equality: id, url, label content and enabled per target.
func discoveryTargetsEqual(a, b []discoveryTarget) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].id != b[i].id || a[i].url != b[i].url || a[i].enabled != b[i].enabled {
			return false
		}
		if !stringMapsEqual(a[i].labels, b[i].labels) {
			return false
		}
	}
	return true
}

func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// ---- HTTP handlers ---------------------------------------------------------

func registerDiscoveryHandlers(mux *http.ServeMux) {
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
		writeJSON(w, http.StatusOK, discoveryWireJSON(tenantStore(r).snapshotDiscovery()))
	})

	mux.HandleFunc(discoveryReloadPath, func(w http.ResponseWriter, r *http.Request) {
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

		snap := tenantStore(r).reloadDiscovery(targets)
		writeJSON(w, http.StatusOK, discoveryWireJSON(snap))
	})
}
