package server

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"sort"
	"time"
)

const (
	diagnosticExportPath = "/api/v1/diagnostic-export"

	// maxDiagnosticExportBytes bounds the encoded bundle; a larger result is
	// rejected wholesale with 413 and no partial content is sent.
	maxDiagnosticExportBytes = 10 * 1024 * 1024

	// maxDiagnosticExportSpan is the longest exportable window.
	maxDiagnosticExportSpan = 24 * time.Hour
)

// diagnosticSectionNames is the closed vocabulary of exportable sections.
var diagnosticSectionNames = map[string]bool{
	"metrics":       true,
	"alerts":        true,
	"logs":          true,
	"traces":        true,
	"configuration": true,
}

// ---- JSON wire shapes ------------------------------------------------------

// diagnosticSampleJSON is one raw retained observation. Buckets carries the
// series' fixed histogram boundaries and appears only for histograms.
type diagnosticSampleJSON struct {
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Labels    map[string]string `json:"labels"`
	Value     float64           `json:"value"`
	Timestamp string            `json:"timestamp"`
	Buckets   []float64         `json:"buckets,omitempty"`
}

type diagnosticMetricsJSON struct {
	Current []seriesOutput         `json:"current"`
	Samples []diagnosticSampleJSON `json:"samples"`
}

type notificationPlanJSON struct {
	Deliveries       []deliveryOutput `json:"deliveries"`
	UnroutedAlertIDs []string         `json:"unrouted_alert_ids"`
}

type sloStatusListJSON struct {
	SLOs []sloStatusOutput `json:"slos"`
}

type diagnosticAlertsJSON struct {
	Alerts           []alertOutput        `json:"alerts"`
	NotificationPlan notificationPlanJSON `json:"notification_plan"`
	SLOStatus        sloStatusListJSON    `json:"slo_status"`
}

type diagnosticLogsJSON struct {
	Entries []logEntryJSON `json:"entries"`
}

type diagnosticTracesJSON struct {
	Spans []spanJSON `json:"spans"`
}

type diagnosticConfigurationJSON struct {
	AlertRules         []any                   `json:"alert_rules"`
	Silences           []silenceJSON           `json:"silences"`
	InhibitRules       []inhibitRuleJSON       `json:"inhibit_rules"`
	NotificationRoutes []notificationRouteJSON `json:"notification_routes"`
	SLOs               []sloJSON               `json:"slos"`
	SamplingPolicy     samplingPolicy          `json:"sampling_policy"`
	DiscoveryTargets   map[string]any          `json:"discovery_targets"`
}

type diagnosticRangeJSON struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type diagnosticBundleJSON struct {
	SchemaVersion int                 `json:"schema_version"`
	Tenant        string              `json:"tenant"`
	GeneratedAt   string              `json:"generated_at"`
	Range         diagnosticRangeJSON `json:"range"`
	Sections      map[string]any      `json:"sections"`
}

// ---- request decoding ------------------------------------------------------

type rawDiagnosticExport struct {
	Start    *string   `json:"start"`
	End      *string   `json:"end"`
	Sections *[]string `json:"sections"`
}

// decodeDiagnosticExportRequest performs every format and value check: exactly
// the three documented fields, timezone-carrying RFC3339Nano bounds with
// start < end and a span of at most 24 hours, and a non-empty duplicate-free
// section selection.
func decodeDiagnosticExportRequest(body []byte) (start, end time.Time, sections map[string]bool, ok bool) {
	var raw rawDiagnosticExport
	if !strictDecode(body, &raw) || raw.Start == nil || raw.End == nil || raw.Sections == nil {
		return time.Time{}, time.Time{}, nil, false
	}

	start, err := time.Parse(time.RFC3339Nano, *raw.Start)
	if err != nil {
		return time.Time{}, time.Time{}, nil, false
	}
	end, err = time.Parse(time.RFC3339Nano, *raw.End)
	if err != nil {
		return time.Time{}, time.Time{}, nil, false
	}
	if !start.Before(end) || end.Sub(start) > maxDiagnosticExportSpan {
		return time.Time{}, time.Time{}, nil, false
	}

	list := *raw.Sections
	if len(list) == 0 {
		return time.Time{}, time.Time{}, nil, false
	}
	sections = make(map[string]bool, len(list))
	for _, name := range list {
		if !diagnosticSectionNames[name] || sections[name] {
			return time.Time{}, time.Time{}, nil, false
		}
		sections[name] = true
	}
	return start, end, sections, true
}

// ---- bundle assembly -------------------------------------------------------

// diagnosticBundle assembles every selected section from the single
// consistent snapshot held under s.mu, so concurrent commits never mix into
// one export. now is the request-start instant: it is echoed as generated_at
// and anchors alert evaluation, silence states and the retention cutoff.
func (s *metricStore) diagnosticBundle(tenant string, now, start, end time.Time, selected map[string]bool) diagnosticBundleJSON {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sections := make(map[string]any, len(selected))
	if selected["metrics"] {
		sections["metrics"] = s.diagnosticMetricsLocked(now, start, end)
	}
	if selected["alerts"] {
		deliveries, unrouted := s.notificationPlanLocked(now)
		sections["alerts"] = diagnosticAlertsJSON{
			Alerts: s.evalAlertsLockedAt(now),
			NotificationPlan: notificationPlanJSON{
				Deliveries:       deliveries,
				UnroutedAlertIDs: unrouted,
			},
			SLOStatus: sloStatusListJSON{SLOs: s.evalSLOStatusLocked()},
		}
	}
	if selected["logs"] {
		sections["logs"] = s.diagnosticLogsLocked(start, end)
	}
	if selected["traces"] {
		sections["traces"] = s.diagnosticTracesLocked(start, end)
	}
	if selected["configuration"] {
		sections["configuration"] = s.diagnosticConfigurationLocked(now)
	}

	return diagnosticBundleJSON{
		SchemaVersion: 1,
		Tenant:        tenant,
		GeneratedAt:   now.UTC().Format(time.RFC3339Nano),
		Range: diagnosticRangeJSON{
			Start: start.UTC().Format(time.RFC3339Nano),
			End:   end.UTC().Format(time.RFC3339Nano),
		},
		Sections: sections,
	}
}

// diagnosticMetricsLocked renders the current value of every series (same
// wire shape as the metric query endpoint) plus the raw retained observations
// inside [start, end). Samples are ordered by timestamp, then canonical
// series key, then commit order; counter values stay write increments.
func (s *metricStore) diagnosticMetricsLocked(now, start, end time.Time) diagnosticMetricsJSON {
	cutoff := now.Add(-metricRetention)

	current := make([]seriesOutput, 0, len(s.series))
	for _, cur := range s.series {
		current = append(current, seriesWireOutput(cur))
	}
	sort.Slice(current, func(i, j int) bool {
		if current[i].Name != current[j].Name {
			return current[i].Name < current[j].Name
		}
		return compareLabels(current[i].Labels, current[j].Labels) < 0
	})

	type sampleRow struct {
		ts     time.Time
		key    string
		sample diagnosticSampleJSON
	}
	rows := make([]sampleRow, 0)
	for key, cur := range s.series {
		for _, p := range cur.history {
			if p.ts.Before(cutoff) || p.ts.Before(start) || !p.ts.Before(end) {
				continue
			}
			item := diagnosticSampleJSON{
				Name:      cur.name,
				Type:      cur.typ,
				Labels:    copyLabels(cur.labels),
				Value:     p.value,
				Timestamp: p.ts.UTC().Format(time.RFC3339Nano),
			}
			if cur.typ == "histogram" {
				item.Buckets = append([]float64(nil), cur.bucketBounds...)
			}
			rows = append(rows, sampleRow{ts: p.ts, key: key, sample: item})
		}
	}
	// Per-series history is appended in commit order, so the stable sort keeps
	// commit order as the final tie-break within one (timestamp, key).
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].ts.Equal(rows[j].ts) {
			return rows[i].ts.Before(rows[j].ts)
		}
		return rows[i].key < rows[j].key
	})
	samples := make([]diagnosticSampleJSON, 0, len(rows))
	for _, row := range rows {
		samples = append(samples, row.sample)
	}

	return diagnosticMetricsJSON{Current: current, Samples: samples}
}

// diagnosticLogsLocked returns every retained log entry whose timestamp falls
// into [start, end), in the log read order: timestamp descending, id
// ascending within one instant.
func (s *metricStore) diagnosticLogsLocked(start, end time.Time) diagnosticLogsJSON {
	matched := make([]*logEntry, 0)
	for _, e := range s.logs {
		if e.timestamp.Before(start) || !e.timestamp.Before(end) {
			continue
		}
		matched = append(matched, e)
	}
	sort.Slice(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if !a.timestamp.Equal(b.timestamp) {
			return a.timestamp.After(b.timestamp)
		}
		return a.id < b.id
	})

	entries := make([]logEntryJSON, 0, len(matched))
	for _, e := range matched {
		entries = append(entries, e.wireJSON())
	}
	return diagnosticLogsJSON{Entries: entries}
}

// diagnosticTracesLocked returns every retained span whose start_time falls
// into [start, end), ordered by start time, then trace id, then span id.
func (s *metricStore) diagnosticTracesLocked(start, end time.Time) diagnosticTracesJSON {
	matched := make([]*span, 0)
	for _, sp := range s.spans {
		if sp.startTime.Before(start) || !sp.startTime.Before(end) {
			continue
		}
		matched = append(matched, sp)
	}
	sort.Slice(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if !a.startTime.Equal(b.startTime) {
			return a.startTime.Before(b.startTime)
		}
		if a.traceID != b.traceID {
			return a.traceID < b.traceID
		}
		return a.spanID < b.spanID
	})

	spans := make([]spanJSON, 0, len(matched))
	for _, sp := range matched {
		spans = append(spans, sp.wireJSON())
	}
	return diagnosticTracesJSON{Spans: spans}
}

// diagnosticConfigurationLocked renders every configuration document in its
// existing wire shape, each collection in its documented id-sorted order.
// Silence states are classified against the snapshot instant.
func (s *metricStore) diagnosticConfigurationLocked(now time.Time) diagnosticConfigurationJSON {
	rules := s.snapshotRulesLocked()
	alertRules := make([]any, 0, len(rules))
	for _, r := range rules {
		alertRules = append(alertRules, ruleWireJSON(r))
	}

	silences := s.snapshotSilencesLocked()
	silenceList := make([]silenceJSON, 0, len(silences))
	for _, sil := range silences {
		silenceList = append(silenceList, silenceWireJSON(sil, now))
	}

	inhibitions := s.snapshotInhibitRulesLocked()
	inhibitRules := make([]inhibitRuleJSON, 0, len(inhibitions))
	for _, ir := range inhibitions {
		inhibitRules = append(inhibitRules, inhibitWireJSON(ir))
	}

	routes := s.snapshotNotificationRoutesLocked()
	routeList := make([]notificationRouteJSON, 0, len(routes))
	for _, rt := range routes {
		routeList = append(routeList, notificationRouteWireJSON(rt))
	}

	slos := s.snapshotSLOsLocked()
	sloList := make([]sloJSON, 0, len(slos))
	for _, d := range slos {
		sloList = append(sloList, sloWireJSON(d))
	}

	return diagnosticConfigurationJSON{
		AlertRules:         alertRules,
		Silences:           silenceList,
		InhibitRules:       inhibitRules,
		NotificationRoutes: routeList,
		SLOs:               sloList,
		SamplingPolicy:     s.sampling,
		DiscoveryTargets:   discoverySnapshotWireJSON(s.discovery),
	}
}

// ---- HTTP handler ----------------------------------------------------------

func registerDiagnosticExportHandler(mux *http.ServeMux) {
	mux.HandleFunc(diagnosticExportPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		handleDiagnosticExport(w, r)
	})
}

func handleDiagnosticExport(w http.ResponseWriter, r *http.Request) {
	// The request-start instant anchors generated_at and every "now"-dependent
	// computation inside the snapshot.
	generatedAt := time.Now()
	invalid := func() {
		writeAPIError(w, "invalid_diagnostic_export", http.StatusBadRequest)
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		invalid()
		return
	}

	start, end, sections, ok := decodeDiagnosticExportRequest(body)
	if !ok {
		invalid()
		return
	}

	bundle := tenantStore(r).diagnosticBundle(tenantName(r), generatedAt, start, end, sections)

	// The bundle is encoded before anything is written: a non-finite number
	// anywhere fails the whole export with 422, and an oversized result fails
	// with 413 — neither sends partial content.
	payload, err := json.Marshal(bundle)
	if err != nil {
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
}
