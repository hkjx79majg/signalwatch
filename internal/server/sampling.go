package server

import (
	"crypto/sha256"
	"encoding/binary"
	"io"
	"math"
	"mime"
	"net/http"
)

const samplingPolicyPath = "/api/v1/sampling-policy"

// samplingPolicy is the per-tenant deterministic ingest sampling
// configuration. Both rates are finite numbers in [0, 1]; the zero value is
// never stored — new tenants and restarts start from the default of 1.
type samplingPolicy struct {
	LogRate   float64 `json:"log_rate"`
	TraceRate float64 `json:"trace_rate"`
}

// defaultSamplingPolicy keeps every log and span.
func defaultSamplingPolicy() samplingPolicy {
	return samplingPolicy{LogRate: 1, TraceRate: 1}
}

// active reports whether any rate drops below 1, i.e. whether ingest
// responses must carry the sampled_out count.
func (p samplingPolicy) active() bool {
	return p.LogRate < 1 || p.TraceRate < 1
}

// getSampling returns the tenant's current policy.
func (s *metricStore) getSampling() samplingPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sampling
}

// samplingLocked is getSampling for callers already holding the store lock.
func (s *metricStore) samplingLocked() samplingPolicy {
	return s.sampling
}

// setSampling atomically replaces the tenant's policy. Existing data is
// never touched; the new policy only governs future ingest decisions.
func (s *metricStore) setSampling(p samplingPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sampling = p
}

// sampledOut deterministically decides whether identity is dropped at the
// given rate. The decision depends only on the identity string and the rate,
// so the same tenant identity under the same policy is kept or dropped
// regardless of batch splitting, arrival order or replay. Hashing the trace
// id for both traced logs and spans keeps a whole trace together.
func sampledOut(identity string, rate float64) bool {
	if rate >= 1 {
		return false
	}
	if rate <= 0 {
		return true
	}
	sum := sha256.Sum256([]byte(identity))
	h := binary.BigEndian.Uint64(sum[:8])
	// Compare against rate * 2^64 in float64: 2^64 is exact and h < 2^64,
	// so rate == 1 would keep everything and rate == 0 drop everything.
	return float64(h) >= rate*18446744073709551616.0
}

// ---- HTTP handlers ---------------------------------------------------------

func registerSamplingPolicyHandlers(mux *http.ServeMux) {
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
	// The endpoint defines no query vocabulary; any parameter is invalid.
	if r.URL.RawQuery != "" {
		writeAPIError(w, "invalid_sampling_query", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, tenantStore(r).getSampling())
}

func handleSamplingPolicyPut(w http.ResponseWriter, r *http.Request) {
	store := tenantStore(r)
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

	policy, ok := decodeSamplingPolicy(body)
	if !ok {
		writeAPIError(w, "invalid_sampling_policy", http.StatusBadRequest)
		return
	}

	store.setSampling(policy)
	writeJSON(w, http.StatusOK, policy)
}

// rawSamplingPolicy keeps both fields as pointers so a missing or null field
// is distinguishable from an explicit zero.
type rawSamplingPolicy struct {
	LogRate   *float64 `json:"log_rate"`
	TraceRate *float64 `json:"trace_rate"`
}

// decodeSamplingPolicy performs every format and value check: exactly the
// two documented fields, both finite numbers in [0, 1] inclusive.
func decodeSamplingPolicy(body []byte) (samplingPolicy, bool) {
	var raw rawSamplingPolicy
	if !strictDecode(body, &raw) || raw.LogRate == nil || raw.TraceRate == nil {
		return samplingPolicy{}, false
	}
	logRate, traceRate := *raw.LogRate, *raw.TraceRate
	if !validSamplingRate(logRate) || !validSamplingRate(traceRate) {
		return samplingPolicy{}, false
	}
	return samplingPolicy{LogRate: logRate, TraceRate: traceRate}, true
}

func validSamplingRate(rate float64) bool {
	return !math.IsNaN(rate) && !math.IsInf(rate, 0) && rate >= 0 && rate <= 1
}
