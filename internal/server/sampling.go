package server

import (
	"crypto/sha256"
	"encoding/binary"
	"net/http"
)

// writeIngestResponse writes the 202 ingestion result. While both rates are 1
// sampling cannot drop anything, so the baseline two-field body is preserved
// exactly. Once either rate is below 1 the body also reports sampled_out
// (even when this particular batch dropped nothing), and the three counts sum
// to the request array length. samplingActive reflects the policy snapshot
// the commit actually used.
func writeIngestResponse(w http.ResponseWriter, accepted, replayed, sampledOut int, samplingActive bool) {
	if !samplingActive {
		writeJSON(w, http.StatusAccepted, map[string]int{
			"accepted": accepted,
			"replayed": replayed,
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{
		"accepted":    accepted,
		"replayed":    replayed,
		"sampled_out": sampledOut,
	})
}

// sampleIn reports whether an identity is retained under rate. The decision is
// a pure, deterministic function of the identity and the rate: it does not
// depend on batch membership, arrival order or time, so splitting one batch
// across requests or replaying it cannot change which identities stay while
// the policy is unchanged.
//
// A rate of 1 keeps every identity and a rate of 0 drops every one; an
// intermediate rate keeps the identity when a uniform 32-bit hash of it falls
// below the rate. Logs and spans of one trace share the trace id as their
// identity, so they are always decided together under trace_rate.
func sampleIn(identity string, rate float64) bool {
	switch {
	case rate >= 1:
		return true
	case rate <= 0:
		return false
	}
	sum := sha256.Sum256([]byte(identity))
	v := binary.BigEndian.Uint32(sum[:4])
	return float64(v)/float64(1<<32) < rate
}
