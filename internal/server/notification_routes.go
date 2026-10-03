package server

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
)

const (
	notificationRoutesPath   = "/api/v1/notification-routes"
	notificationRoutesPrefix = notificationRoutesPath + "/"
	notificationPlanPath     = "/api/v1/notification-plan"
)

// notificationRoute is an immutable routing document. It maps a set of alert
// rule ids to a receiver with a priority used when several routes cover the
// same alert. Replacements swap the pointer atomically, so readers never
// observe a half-updated route.
type notificationRoute struct {
	id       string
	ruleIDs  []string
	receiver string
	priority int

	// comment is a nullable string: nil echoes back as JSON null.
	comment *string
}

// ---- JSON wire shapes ------------------------------------------------------

type notificationRouteJSON struct {
	ID       string   `json:"id"`
	RuleIDs  []string `json:"rule_ids"`
	Receiver string   `json:"receiver"`
	Priority int      `json:"priority"`
	Comment  *string  `json:"comment"`
}

func notificationRouteWireJSON(r *notificationRoute) notificationRouteJSON {
	return notificationRouteJSON{
		ID:       r.id,
		RuleIDs:  append([]string(nil), r.ruleIDs...),
		Receiver: r.receiver,
		Priority: r.priority,
		Comment:  r.comment,
	}
}

// ---- request decoding ------------------------------------------------------

type rawNotificationRoute struct {
	RuleIDs  *[]string       `json:"rule_ids"`
	Receiver *string         `json:"receiver"`
	Priority *int            `json:"priority"`
	Comment  json.RawMessage `json:"comment"`
}

// decodeNotificationRoute parses and fully validates a route body. It never
// returns a partially validated route.
func decodeNotificationRoute(body []byte) (*notificationRoute, bool) {
	var raw rawNotificationRoute
	if !strictDecode(body, &raw) {
		return nil, false
	}
	if raw.RuleIDs == nil || raw.Receiver == nil || raw.Priority == nil || raw.Comment == nil {
		return nil, false
	}

	// rule_ids may reference rules that do not exist yet; the ids themselves
	// still must be non-empty, distinct, valid identifiers.
	ruleIDs, ok := validateIDList(*raw.RuleIDs)
	if !ok {
		return nil, false
	}
	receiver := *raw.Receiver
	if !identPattern.MatchString(receiver) {
		return nil, false
	}
	priority := *raw.Priority
	if priority < 0 || priority > 1000 {
		return nil, false
	}
	// comment is a nullable string: an explicit null is accepted and echoes
	// back as null, but any other JSON type is rejected. A missing field was
	// already rejected above (raw.Comment == nil).
	var comment *string
	if err := json.Unmarshal(raw.Comment, &comment); err != nil {
		return nil, false
	}

	return &notificationRoute{
		ruleIDs:  ruleIDs,
		receiver: receiver,
		priority: priority,
		comment:  comment,
	}, true
}

// ---- store operations ------------------------------------------------------

func (s *metricStore) putNotificationRoute(r *notificationRoute) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.notificationRoutes[r.id]
	s.notificationRoutes[r.id] = r
	return !existed
}

func (s *metricStore) getNotificationRoute(id string) (*notificationRoute, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.notificationRoutes[id]
	return r, ok
}

func (s *metricStore) deleteNotificationRoute(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.notificationRoutes[id]; !ok {
		return false
	}
	delete(s.notificationRoutes, id)
	return true
}

// snapshotNotificationRoutes returns all routes sorted by id. Route objects
// are immutable, so the pointers stay safe after the lock is released.
func (s *metricStore) snapshotNotificationRoutes() []*notificationRoute {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotNotificationRoutesLocked()
}

func (s *metricStore) snapshotNotificationRoutesLocked() []*notificationRoute {
	out := make([]*notificationRoute, 0, len(s.notificationRoutes))
	for _, r := range s.notificationRoutes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// ---- HTTP handlers ---------------------------------------------------------

func registerNotificationRouteHandlers(mux *http.ServeMux) {
	mux.HandleFunc(notificationRoutesPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		routes := tenantStore(r).snapshotNotificationRoutes()
		body := make([]notificationRouteJSON, 0, len(routes))
		for _, route := range routes {
			body = append(body, notificationRouteWireJSON(route))
		}
		writeJSON(w, http.StatusOK, map[string]any{"routes": body})
	})

	mux.HandleFunc(notificationRoutesPrefix, func(w http.ResponseWriter, r *http.Request) {
		store := tenantStore(r)
		id := strings.TrimPrefix(r.URL.Path, notificationRoutesPrefix)
		switch r.Method {
		case http.MethodGet, http.MethodDelete:
			// Reads and deletions can only address an existing, syntactically
			// valid route id; anything else is simply not found.
			if id == "" || strings.Contains(id, "/") || !identPattern.MatchString(id) {
				writeAPIError(w, "notification_route_not_found", http.StatusNotFound)
				return
			}
			if r.Method == http.MethodGet {
				handleNotificationRouteGet(w, id, store)
			} else {
				handleNotificationRouteDelete(w, id, store)
			}
		case http.MethodPut:
			// A structurally nested path is outside this resource subtree;
			// an identifier-shaped but invalid id is a bad identifier.
			if id == "" || strings.Contains(id, "/") {
				writeAPIError(w, "notification_route_not_found", http.StatusNotFound)
				return
			}
			if !identPattern.MatchString(id) {
				writeAPIError(w, "invalid_notification_route", http.StatusBadRequest)
				return
			}
			handleNotificationRoutePut(w, r, id, store)
		default:
			w.Header().Set("Allow", "GET, PUT, DELETE")
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(notificationPlanPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		deliveries, unrouted := tenantStore(r).notificationPlan()
		writeJSON(w, http.StatusOK, map[string]any{
			"deliveries":         deliveries,
			"unrouted_alert_ids": unrouted,
		})
	})
}

func handleNotificationRouteGet(w http.ResponseWriter, id string, store *metricStore) {
	route, ok := store.getNotificationRoute(id)
	if !ok {
		writeAPIError(w, "notification_route_not_found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, notificationRouteWireJSON(route))
}

func handleNotificationRoutePut(w http.ResponseWriter, r *http.Request, id string, store *metricStore) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, "unsupported_media_type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, "invalid_notification_route", http.StatusBadRequest)
		return
	}

	route, ok := decodeNotificationRoute(body)
	if !ok {
		writeAPIError(w, "invalid_notification_route", http.StatusBadRequest)
		return
	}
	route.id = id

	created := store.putNotificationRoute(route)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, notificationRouteWireJSON(route))
}

func handleNotificationRouteDelete(w http.ResponseWriter, id string, store *metricStore) {
	if !store.deleteNotificationRoute(id) {
		writeAPIError(w, "notification_route_not_found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- notification plan -----------------------------------------------------

// deliveryOutput is one planned notification: the alert to send, the receiver
// resolved from the winning route, and the route that won.
type deliveryOutput struct {
	AlertID  string `json:"alert_id"`
	Receiver string `json:"receiver"`
	RouteID  string `json:"route_id"`
}

// notificationPlan evaluates alerts and routes against one consistent snapshot
// shared with metrics, alert rules, silences and inhibit rules. Only alerts
// that are firing and neither silenced nor inhibited participate. Each
// participating alert is delivered through the covering route with the
// smallest priority, ties broken by the smallest route id; participating
// alerts with no covering route are reported as unrouted.
func (s *metricStore) notificationPlan() ([]deliveryOutput, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	alerts := s.evalAlertsLocked()
	// Routes are sorted by id, so scanning in order and replacing the winner
	// only on a strictly smaller priority also resolves priority ties toward
	// the smallest id.
	routes := s.snapshotNotificationRoutesLocked()

	deliveries := make([]deliveryOutput, 0)
	unrouted := make([]string, 0)
	for _, al := range alerts {
		if al.State != "firing" || al.Silenced || al.Inhibited {
			continue
		}
		var best *notificationRoute
		for _, rt := range routes {
			if !containsString(rt.ruleIDs, al.ID) {
				continue
			}
			if best == nil || rt.priority < best.priority {
				best = rt
			}
		}
		if best == nil {
			unrouted = append(unrouted, al.ID)
			continue
		}
		deliveries = append(deliveries, deliveryOutput{
			AlertID:  al.ID,
			Receiver: best.receiver,
			RouteID:  best.id,
		})
	}

	// Alert evaluation already returns alerts sorted by id, but enforce the
	// documented output order explicitly.
	sort.Slice(deliveries, func(i, j int) bool { return deliveries[i].AlertID < deliveries[j].AlertID })
	sort.Strings(unrouted)
	return deliveries, unrouted
}
