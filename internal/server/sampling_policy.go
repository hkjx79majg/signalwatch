package server

import (
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
)

const samplingPolicyPath = "/api/v1/sampling-policy"

// samplingPolicy is one tenant's deterministic ingestion rates. Both values
// are finite numbers in [0, 1]. The zero value is not usable on its own; new
// stores start from defaultSamplingPolicy and policies only live in memory.
type samplingPolicy struct {
	logRate   float64
	traceRate float64
}

// defaultSamplingPolicy keeps everything, preserving baseline behavior for
// new tenants and after a restart.
var defaultSamplingPolicy = samplingPolicy{logRate: 1, traceRate: 1}

type samplingPolicyJSON struct {
	LogRate   float64 `json:"log_rate"`
	TraceRate float64 `json:"trace_rate"`
}

// samplingPolicy returns the tenant's current policy. Absence of an explicit
// policy means the keep-everything default.
func (s *metricStore) getSamplingPolicy() samplingPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.policyLocked()
}

// policyLocked returns the tenant's current policy; the caller must hold
// (read or write) s.mu so a commit samples against one stable policy.
func (s *metricStore) policyLocked() samplingPolicy {
	if s.policy == nil {
		return defaultSamplingPolicy
	}
	return *s.policy
}

// replaceSamplingPolicy atomically swaps the tenant's policy. The update never
// retroactively removes already retained data.
func (s *metricStore) replaceSamplingPolicy(p samplingPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = &p
}

// registerSamplingPolicyHandler exposes GET and PUT on the tenant-scoped
// policy resource.
func registerSamplingPolicyHandler(mux *http.ServeMux) {
	mux.HandleFunc(samplingPolicyPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleSamplingPolicyGet(w, r)
		case http.MethodPut:
			handleSamplingPolicyPut(w, r)
		default:
			w.Header().Set("Allow", "GET, PUT")
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
		}
	})
}

func handleSamplingPolicyGet(w http.ResponseWriter, r *http.Request) {
	// The resource defines no query vocabulary; any parameter, including a
	// malformed query string, is rejected before the policy is read.
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values) != 0 {
		writeAPIError(w, "invalid_sampling_query", http.StatusBadRequest)
		return
	}
	policy := tenantStore(r).getSamplingPolicy()
	writeJSON(w, http.StatusOK, samplingPolicyJSON{
		LogRate:   policy.logRate,
		TraceRate: policy.traceRate,
	})
}

func handleSamplingPolicyPut(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, "invalid_sampling_policy", http.StatusBadRequest)
		return
	}

	var raw struct {
		LogRate   *float64 `json:"log_rate"`
		TraceRate *float64 `json:"trace_rate"`
	}
	// A single JSON object with exactly the two known fields; strictDecode
	// rejects non-objects, multiple/trailing values and unknown fields.
	if !strictDecode(body, &raw) || raw.LogRate == nil || raw.TraceRate == nil ||
		!validSamplingRate(*raw.LogRate) || !validSamplingRate(*raw.TraceRate) {
		writeAPIError(w, "invalid_sampling_policy", http.StatusBadRequest)
		return
	}

	tenantStore(r).replaceSamplingPolicy(samplingPolicy{
		logRate:   *raw.LogRate,
		traceRate: *raw.TraceRate,
	})
	writeJSON(w, http.StatusOK, samplingPolicyJSON{
		LogRate:   *raw.LogRate,
		TraceRate: *raw.TraceRate,
	})
}

// validSamplingRate reports whether rate is a finite number in [0, 1].
func validSamplingRate(rate float64) bool {
	return !math.IsNaN(rate) && !math.IsInf(rate, 0) && rate >= 0 && rate <= 1
}
