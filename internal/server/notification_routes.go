package server

import (
	"io"
	"math"
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

// notificationRoute is an immutable routing document mapping alert rule ids
// to a receiver. Replacements swap the pointer atomically, so readers never
// observe a half-updated route.
type notificationRoute struct {
	id       string
	ruleIDs  []string
	receiver string
	priority int
	comment  *string // nil echoes JSON null
}

// ---- JSON wire shapes ------------------------------------------------------

type notificationRouteJSON struct {
	ID       string   `json:"id"`
	RuleIDs  []string `json:"rule_ids"`
	Receiver string   `json:"receiver"`
	Priority int      `json:"priority"`
	Comment  *string  `json:"comment"`
}

func routeWireJSON(r *notificationRoute) notificationRouteJSON {
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
	RuleIDs  *[]string `json:"rule_ids"`
	Receiver *string   `json:"receiver"`
	Priority *float64  `json:"priority"`
	Comment  *string   `json:"comment"`
}

// decodeNotificationRoute parses and fully validates a route body. It never
// returns a partially validated route.
func decodeNotificationRoute(body []byte) (*notificationRoute, bool) {
	var raw rawNotificationRoute
	if !strictDecode(body, &raw) {
		return nil, false
	}
	if raw.RuleIDs == nil || raw.Receiver == nil || raw.Priority == nil {
		return nil, false
	}

	// Referenced rule ids need not exist; they only have to be a non-empty
	// list of distinct valid identifiers.
	ruleIDs, ok := validateIDList(*raw.RuleIDs)
	if !ok {
		return nil, false
	}
	if !identPattern.MatchString(*raw.Receiver) {
		return nil, false
	}
	priority := *raw.Priority
	if math.IsNaN(priority) || math.IsInf(priority, 0) ||
		priority != math.Trunc(priority) || priority < 0 || priority > 1000 {
		return nil, false
	}

	return &notificationRoute{
		ruleIDs:  ruleIDs,
		receiver: *raw.Receiver,
		priority: int(priority),
		comment:  raw.Comment,
	}, true
}

// ---- store operations ------------------------------------------------------

func (s *metricStore) putRoute(r *notificationRoute) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.routes[r.id]
	s.routes[r.id] = r
	return !existed
}

func (s *metricStore) getRoute(id string) (*notificationRoute, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.routes[id]
	return r, ok
}

func (s *metricStore) deleteRoute(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.routes[id]; !ok {
		return false
	}
	delete(s.routes, id)
	return true
}

// snapshotRoutes returns all routes sorted by id. Route objects are
// immutable, so the pointers stay safe after the lock is released.
func (s *metricStore) snapshotRoutes() []*notificationRoute {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*notificationRoute, 0, len(s.routes))
	for _, r := range s.routes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// ---- notification plan -----------------------------------------------------

type deliveryJSON struct {
	AlertID  string `json:"alert_id"`
	Receiver string `json:"receiver"`
	RouteID  string `json:"route_id"`
}

// notificationPlan computes the pending deliveries from one consistent
// snapshot of metrics, rules, silences, inhibit rules and routes. Only
// firing, neither silenced nor inhibited alerts participate; each is routed
// to the lowest-priority route covering its rule id, ties broken by route id.
func (s *metricStore) notificationPlan() ([]deliveryJSON, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	alerts := s.evalAlertsLocked()
	deliveries := make([]deliveryJSON, 0)
	unrouted := make([]string, 0)
	for _, al := range alerts {
		if al.State != "firing" || al.Silenced || al.Inhibited {
			continue
		}
		var best *notificationRoute
		for _, rt := range s.routes {
			if !containsString(rt.ruleIDs, al.ID) {
				continue
			}
			if best == nil || rt.priority < best.priority ||
				(rt.priority == best.priority && rt.id < best.id) {
				best = rt
			}
		}
		if best == nil {
			unrouted = append(unrouted, al.ID)
			continue
		}
		deliveries = append(deliveries, deliveryJSON{
			AlertID:  al.ID,
			Receiver: best.receiver,
			RouteID:  best.id,
		})
	}
	sort.Slice(deliveries, func(i, j int) bool { return deliveries[i].AlertID < deliveries[j].AlertID })
	sort.Strings(unrouted)
	return deliveries, unrouted
}

// ---- HTTP handlers ---------------------------------------------------------

func registerNotificationHandlers(mux *http.ServeMux, store *metricStore) {
	mux.HandleFunc(notificationRoutesPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, "method_not_allowed", http.StatusMethodNotAllowed)
			return
		}
		routes := store.snapshotRoutes()
		body := make([]notificationRouteJSON, 0, len(routes))
		for _, rt := range routes {
			body = append(body, routeWireJSON(rt))
		}
		writeJSON(w, http.StatusOK, map[string]any{"routes": body})
	})

	mux.HandleFunc(notificationRoutesPrefix, func(w http.ResponseWriter, r *http.Request) {
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
				handleRouteGet(w, id, store)
			} else {
				handleRouteDelete(w, id, store)
			}
		case http.MethodPut:
			// A structurally nested path is outside this resource subtree;
			// an identifier-shaped but invalid id is a bad route identifier.
			if id == "" || strings.Contains(id, "/") {
				writeAPIError(w, "notification_route_not_found", http.StatusNotFound)
				return
			}
			if !identPattern.MatchString(id) {
				writeAPIError(w, "invalid_notification_route", http.StatusBadRequest)
				return
			}
			handleRoutePut(w, r, id, store)
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
		deliveries, unrouted := store.notificationPlan()
		writeJSON(w, http.StatusOK, map[string]any{
			"deliveries":         deliveries,
			"unrouted_alert_ids": unrouted,
		})
	})
}

func handleRouteGet(w http.ResponseWriter, id string, store *metricStore) {
	rt, ok := store.getRoute(id)
	if !ok {
		writeAPIError(w, "notification_route_not_found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, routeWireJSON(rt))
}

func handleRoutePut(w http.ResponseWriter, r *http.Request, id string, store *metricStore) {
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

	rt, ok := decodeNotificationRoute(body)
	if !ok {
		writeAPIError(w, "invalid_notification_route", http.StatusBadRequest)
		return
	}
	rt.id = id

	created := store.putRoute(rt)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, routeWireJSON(rt))
}

func handleRouteDelete(w http.ResponseWriter, id string, store *metricStore) {
	if !store.deleteRoute(id) {
		writeAPIError(w, "notification_route_not_found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
