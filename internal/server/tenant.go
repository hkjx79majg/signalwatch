package server

import (
	"context"
	"net/http"
	"regexp"
	"sync"
)

// Tenant isolation. Every tenant owns an independent metricStore; resources of
// the same name or id may coexist across tenants and never resolve across
// tenant boundaries. A tenant is selected with the X-SignalWatch-Tenant header;
// requests without the header are fixed to the default tenant.
const (
	tenantHeader  = "X-SignalWatch-Tenant"
	defaultTenant = "default"
)

// tenantPattern pins the exact wire spelling of a tenant name: no case folding
// and no trimming are applied to the header value.
var tenantPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]{0,63}$`)

// storeCtxKey carries the resolved tenant store through the request context.
type storeCtxKey struct{}

// tenantRegistry hands out one independent metricStore per tenant name. Each
// store keeps its own lock, so concurrent requests for different tenants never
// block each other; the registry lock only guards store creation.
type tenantRegistry struct {
	mu     sync.Mutex
	stores map[string]*metricStore
}

func newTenantRegistry() *tenantRegistry {
	return &tenantRegistry{stores: make(map[string]*metricStore)}
}

// get returns the store for name, creating the empty tenant state on first
// use. First access to a legal tenant therefore behaves exactly like the
// baseline empty state.
func (r *tenantRegistry) get(name string) *metricStore {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.stores[name]
	if !ok {
		st = newMetricStore()
		r.stores[name] = st
	}
	return st
}

// requestStore returns the tenant store resolved by withTenant.
func requestStore(r *http.Request) *metricStore {
	return r.Context().Value(storeCtxKey{}).(*metricStore)
}

// withTenant wraps a registered /api/v1 handler. The tenant header is resolved
// before the handler examines method, media type, body or resource id. An
// invalid header (multiple values, an empty value, or a value outside the
// tenant name pattern) is rejected with 400 invalid_tenant and no tenant state
// changes. An absent header selects the default tenant. The resolved tenant
// store is attached to the request context.
func withTenant(reg *tenantRegistry, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(tenantHeader)
		if len(values) != 0 {
			if len(values) != 1 || values[0] == "" || !tenantPattern.MatchString(values[0]) {
				writeAPIError(w, "invalid_tenant", http.StatusBadRequest)
				return
			}
		}
		name := defaultTenant
		if len(values) == 1 {
			name = values[0]
		}
		ctx := context.WithValue(r.Context(), storeCtxKey{}, reg.get(name))
		h(w, r.WithContext(ctx))
	}
}
