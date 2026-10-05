package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"sort"
	"time"
)

const (
	diagnosticExportPath = "/api/v1/diagnostic-export"

	// maxDiagnosticExportBytes bounds a successful package at 10 MiB. The
	// whole document is built and measured before any byte is written, so an
	// oversized export never returns partial content.
	maxDiagnosticExportBytes = 10 * 1024 * 1024
)

// diagnosticSectionNames are the selectable package sections; every name in a
// request must come from this set.
var diagnosticSectionNames = map[string]bool{
	"metrics":       true,
	"alerts":        true,
	"logs":          true,
	"traces":        true,
	"configuration": true,
}

// ---- request decoding ------------------------------------------------------

type rawDiagnosticExport struct {
	Start    *string   `json:"start"`
	End      *string   `json:"end"`
	Sections *[]string `json:"sections"`
}

// decodeDiagnosticExport parses and fully validates an export body. Times are
// timezone-aware RFC3339Nano instants with start strictly before end and a
// span of at most 24 hours; sections is a non-empty list of distinct known
// names, kept in request order so the response lists sections the same way.
func decodeDiagnosticExport(body []byte) (start, end time.Time, sections []string, ok bool) {
	var raw rawDiagnosticExport
	if !strictDecode(body, &raw) || raw.Start == nil || raw.End == nil || raw.Sections == nil {
		return start, end, nil, false
	}

	start, err := time.Parse(time.RFC3339Nano, *raw.Start)
	if err != nil {
		return start, end, nil, false
	}
	end, err = time.Parse(time.RFC3339Nano, *raw.End)
	if err != nil {
		return start, end, nil, false
	}
	if !start.Before(end) || end.Sub(start) > metricRetention {
		return start, end, nil, false
	}

	requested := *raw.Sections
	if len(requested) == 0 {
		return start, end, nil, false
	}
	seen := make(map[string]bool, len(requested))
	for _, name := range requested {
		if !diagnosticSectionNames[name] || seen[name] {
			return start, end, nil, false
		}
		seen[name] = true
	}
	return start, end, requested, true
}

// ---- response shapes -------------------------------------------------------

type exportRangeJSON struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// exportSample is one raw retained observation. Counter samples keep the
// written increment; every sample carries what the write accepted, and a
// histogram sample additionally carries the series' fixed bucket bounds.
type exportSample struct {
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Labels    map[string]string `json:"labels"`
	Value     float64           `json:"value"`
	Timestamp string            `json:"timestamp"`
	Buckets   []float64         `json:"buckets,omitempty"`
}

type exportMetricsSection struct {
	Current []seriesOutput `json:"current"`
	Samples []exportSample `json:"samples"`
}

type exportNotificationPlan struct {
	Deliveries       []deliveryOutput `json:"deliveries"`
	UnroutedAlertIDs []string         `json:"unrouted_alert_ids"`
}

type exportAlertsSection struct {
	Alerts           []alertOutput          `json:"alerts"`
	NotificationPlan exportNotificationPlan `json:"notification_plan"`
	Slos             []sloStatusOutput      `json:"slos"`
}

type exportLogsSection struct {
	Entries []logEntryJSON `json:"entries"`
}

type exportTracesSection struct {
	Spans []spanJSON `json:"spans"`
}

type exportConfigurationSection struct {
	AlertRules         []any                   `json:"alert_rules"`
	Silences           []silenceJSON           `json:"silences"`
	InhibitRules       []inhibitRuleJSON       `json:"inhibit_rules"`
	NotificationRoutes []notificationRouteJSON `json:"notification_routes"`
	Slos               []sloJSON               `json:"slos"`
	SamplingPolicy     samplingPolicy          `json:"sampling_policy"`
	DiscoveryTargets   map[string]any          `json:"discovery_targets"`
}

// currentSeriesOutput renders one series with the GET /api/v1/metrics current
// structure: counter/gauge carry value, histograms carry count, sum and the
// cumulative buckets.
func currentSeriesOutput(cur *series) seriesOutput {
	item := seriesOutput{
		Name:   cur.name,
		Type:   cur.typ,
		Labels: copyLabels(cur.labels),
	}
	switch cur.typ {
	case "counter", "gauge":
		v := cur.value
		item.Value = &v
	case "histogram":
		c, sum := cur.count, cur.sum
		item.Count = &c
		item.Sum = &sum
		item.Buckets = make([]bucketOutput, len(cur.bucketBounds))
		for i, bound := range cur.bucketBounds {
			item.Buckets[i] = bucketOutput{LE: bound, Count: cur.bucketCounts[i]}
		}
	}
	return item
}

// ---- snapshot build --------------------------------------------------------

// collectedSample pairs a raw observation with its per-series commit position,
// which is the final sample ordering tie-break.
type collectedSample struct {
	sample exportSample
	ts     time.Time
	name   string
	labels map[string]string
	commit int
}

// diagnosticSnapshot is the single consistent read of every selected section
// taken at request start. It is assembled entirely under one store read lock,
// so concurrent commits can neither mix into it nor shift the evaluation
// instant; generatedAt anchors silence state and the retention cutoff.
type diagnosticSnapshot struct {
	tenant      string
	generatedAt time.Time
	start       time.Time
	end         time.Time
	sections    map[string]any
}

func (s *metricStore) buildDiagnosticSnapshot(tenant string, now, start, end time.Time, requested []string) diagnosticSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := diagnosticSnapshot{
		tenant:      tenant,
		generatedAt: now,
		start:       start,
		end:         end,
		sections:    make(map[string]any, len(requested)),
	}
	for _, name := range requested {
		switch name {
		case "metrics":
			snap.sections[name] = s.buildMetricsSectionLocked(now, start, end)
		case "alerts":
			snap.sections[name] = s.buildAlertsSectionLocked(now)
		case "logs":
			snap.sections[name] = s.buildLogsSectionLocked(start, end)
		case "traces":
			snap.sections[name] = s.buildTracesSectionLocked(start, end)
		case "configuration":
			snap.sections[name] = s.buildConfigurationSectionLocked(now)
		}
	}
	return snap
}

// sortedSeriesLocked returns all series ordered by name, then by the
// normalized full-label key — the canonical series order shared with the
// metrics query surface.
func (s *metricStore) sortedSeriesLocked() []*series {
	out := make([]*series, 0, len(s.series))
	for _, cur := range s.series {
		out = append(out, cur)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return compareLabels(out[i].labels, out[j].labels) < 0
	})
	return out
}

func (s *metricStore) buildMetricsSectionLocked(now, start, end time.Time) exportMetricsSection {
	seriesList := s.sortedSeriesLocked()
	current := make([]seriesOutput, 0, len(seriesList))
	for _, cur := range seriesList {
		current = append(current, currentSeriesOutput(cur))
	}

	// Raw observations must still be retained (at or after the retention
	// boundary) and fall into the half-open [start, end) window. The
	// per-series history index is the commit-order tie-break at one instant.
	cutoff := now.Add(-metricRetention)
	collected := make([]collectedSample, 0)
	for _, cur := range seriesList {
		for idx, p := range cur.history {
			if p.ts.Before(cutoff) || p.ts.Before(start) || !p.ts.Before(end) {
				continue
			}
			sm := exportSample{
				Name:      cur.name,
				Type:      cur.typ,
				Labels:    copyLabels(cur.labels),
				Value:     p.value,
				Timestamp: p.ts.UTC().Format(time.RFC3339Nano),
			}
			if cur.typ == "histogram" {
				sm.Buckets = append([]float64(nil), cur.bucketBounds...)
			}
			collected = append(collected, collectedSample{
				sample: sm,
				ts:     p.ts,
				name:   cur.name,
				labels: cur.labels,
				commit: idx,
			})
		}
	}
	sort.SliceStable(collected, func(i, j int) bool {
		a, b := collected[i], collected[j]
		if !a.ts.Equal(b.ts) {
			return a.ts.Before(b.ts)
		}
		if a.name != b.name {
			return a.name < b.name
		}
		if cmp := compareLabels(a.labels, b.labels); cmp != 0 {
			return cmp < 0
		}
		return a.commit < b.commit
	})
	samples := make([]exportSample, 0, len(collected))
	for _, c := range collected {
		samples = append(samples, c.sample)
	}
	return exportMetricsSection{Current: current, Samples: samples}
}

func (s *metricStore) buildAlertsSectionLocked(now time.Time) exportAlertsSection {
	// Alerts, the plan and SLO status all derive from this one snapshot and
	// share the same evaluation instant; the plan is computed from the exact
	// alert slice emitted in this section.
	alerts := s.evalAlertsLockedAt(now)
	deliveries, unrouted := s.planFromAlertsLocked(alerts)
	return exportAlertsSection{
		Alerts: alerts,
		NotificationPlan: exportNotificationPlan{
			Deliveries:       deliveries,
			UnroutedAlertIDs: unrouted,
		},
		Slos: s.evalSLOStatusLocked(),
	}
}

func (s *metricStore) buildLogsSectionLocked(start, end time.Time) exportLogsSection {
	entries := make([]*logEntry, 0)
	for _, e := range s.logs {
		// Only retained entries exist in s.logs; the window is [start, end).
		if e.timestamp.Before(start) || !e.timestamp.Before(end) {
			continue
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.timestamp.Equal(b.timestamp) {
			return a.timestamp.Before(b.timestamp)
		}
		return a.id < b.id
	})
	out := make([]logEntryJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.wireJSON())
	}
	return exportLogsSection{Entries: out}
}

func (s *metricStore) buildTracesSectionLocked(start, end time.Time) exportTracesSection {
	spans := make([]*span, 0)
	for _, sp := range s.spans {
		if sp.startTime.Before(start) || !sp.startTime.Before(end) {
			continue
		}
		spans = append(spans, sp)
	}
	sort.Slice(spans, func(i, j int) bool {
		a, b := spans[i], spans[j]
		if !a.startTime.Equal(b.startTime) {
			return a.startTime.Before(b.startTime)
		}
		if a.traceID != b.traceID {
			return a.traceID < b.traceID
		}
		return a.spanID < b.spanID
	})
	out := make([]spanJSON, 0, len(spans))
	for _, sp := range spans {
		out = append(out, sp.wireJSON())
	}
	return exportTracesSection{Spans: out}
}

func (s *metricStore) buildConfigurationSectionLocked(now time.Time) exportConfigurationSection {
	rules := s.snapshotRulesLocked()
	alertRules := make([]any, 0, len(rules))
	for _, r := range rules {
		alertRules = append(alertRules, ruleWireJSON(r))
	}

	silences := s.snapshotSilencesLocked()
	silenceOut := make([]silenceJSON, 0, len(silences))
	for _, sil := range silences {
		silenceOut = append(silenceOut, silenceWireJSON(sil, now))
	}

	inhibitions := s.snapshotInhibitRulesLocked()
	inhibitOut := make([]inhibitRuleJSON, 0, len(inhibitions))
	for _, ir := range inhibitions {
		inhibitOut = append(inhibitOut, inhibitWireJSON(ir))
	}

	routes := s.snapshotNotificationRoutesLocked()
	routeOut := make([]notificationRouteJSON, 0, len(routes))
	for _, rt := range routes {
		routeOut = append(routeOut, notificationRouteWireJSON(rt))
	}

	slos := s.snapshotSLOsLocked()
	sloOut := make([]sloJSON, 0, len(slos))
	for _, d := range slos {
		sloOut = append(sloOut, sloWireJSON(d))
	}

	return exportConfigurationSection{
		AlertRules:         alertRules,
		Silences:           silenceOut,
		InhibitRules:       inhibitOut,
		NotificationRoutes: routeOut,
		Slos:               sloOut,
		SamplingPolicy:     s.samplingLocked(),
		DiscoveryTargets:   discoverySnapshotWireJSON(s.discovery),
	}
}

// ---- envelope encoding -----------------------------------------------------

type exportEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Tenant        string          `json:"tenant"`
	GeneratedAt   string          `json:"generated_at"`
	Range         exportRangeJSON `json:"range"`
	Sections      json.RawMessage `json:"sections"`
}

// marshalPackage renders the package with sections in request order. ok is
// false when a section contains a non-finite number: encoding/json rejects
// NaN/Inf while marshaling, and such a package must fail as 422 instead of
// emitting broken JSON. Only selected sections ever appear.
func marshalPackage(snap diagnosticSnapshot, requested []string) ([]byte, bool) {
	var sectionsBuf bytes.Buffer
	sectionsBuf.WriteByte('{')
	for i, name := range requested {
		if i > 0 {
			sectionsBuf.WriteByte(',')
		}
		raw, err := json.Marshal(snap.sections[name])
		if err != nil {
			return nil, false
		}
		sectionsBuf.WriteByte('"')
		sectionsBuf.WriteString(name)
		sectionsBuf.WriteString(`":`)
		sectionsBuf.Write(raw)
	}
	sectionsBuf.WriteByte('}')

	env := exportEnvelope{
		SchemaVersion: 1,
		Tenant:        snap.tenant,
		GeneratedAt:   snap.generatedAt.UTC().Format(time.RFC3339Nano),
		Range: exportRangeJSON{
			Start: snap.start.UTC().Format(time.RFC3339Nano),
			End:   snap.end.UTC().Format(time.RFC3339Nano),
		},
		Sections: sectionsBuf.Bytes(),
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return nil, false
	}
	return payload, true
}

// ---- HTTP handler ----------------------------------------------------------

func registerDiagnosticExportHandler(mux *http.ServeMux) {
	mux.HandleFunc(diagnosticExportPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}

		// Request-start instant: it anchors the single snapshot, silence
		// windowing, retention trimming and the generated_at stamp.
		now := time.Now()

		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeAPIError(w, "invalid_diagnostic_export", http.StatusBadRequest)
			return
		}

		start, end, sections, ok := decodeDiagnosticExport(body)
		if !ok {
			writeAPIError(w, "invalid_diagnostic_export", http.StatusBadRequest)
			return
		}

		// The read lock covers the whole build, so concurrent changes cannot
		// interleave across sections.
		snap := tenantStore(r).buildDiagnosticSnapshot(tenantName(r), now, start, end, sections)

		payload, finite := marshalPackage(snap, sections)
		if !finite {
			writeAPIError(w, "invalid_diagnostic_export_data", http.StatusUnprocessableEntity)
			return
		}
		if len(payload) > maxDiagnosticExportBytes {
			writeAPIError(w, "diagnostic_export_too_large", http.StatusRequestEntityTooLarge)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	})
}
