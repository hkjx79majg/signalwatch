package server

import (
	"context"
	"net/http"
	"regexp"
	"sync"
)

const (
	tenantHeader  = "X-SignalWatch-Tenant"
	defaultTenant = "default"
	healthzPath   = "/healthz"
)

// tenantPattern is the exact, case-sensitive tenant identifier grammar:
// [a-zA-Z_][a-zA-Z0-9_-]{0,63}. Values are never case-folded or trimmed.
var tenantPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]{0,63}$`)

// tenantRegistry owns one independent metricStore per tenant. Tenant creation
// is serialized by creationMu, but the lock is never held while tenant data is
// read or written: each tenant has its own store mutex, so concurrent requests
// for different tenants never block each other.
type tenantRegistry struct {
	creationMu sync.Mutex
	stores     map[string]*metricStore
}

func newTenantRegistry() *tenantRegistry {
	return &tenantRegistry{stores: map[string]*metricStore{
		defaultTenant: newMetricStore(),
	}}
}

// storeFor returns the tenant's store, creating an empty one on first use so
// collection and computation endpoints respond with the baseline empty state.
func (r *tenantRegistry) storeFor(name string) *metricStore {
	r.creationMu.Lock()
	defer r.creationMu.Unlock()
	st, ok := r.stores[name]
	if !ok {
		st = newMetricStore()
		r.stores[name] = st
	}
	return st
}

type tenantStoreKey struct{}
type tenantNameKey struct{}

// withTenants isolates every registered /api/v1 endpoint by the
// X-SignalWatch-Tenant header. A missing header selects the default tenant; a
// missing, empty, duplicated or malformed value is rejected with
// invalid_tenant before the request reaches any handler, so no media type,
// body or resource id is ever parsed and no tenant state changes. /healthz and
// unknown paths do not participate and pass through untouched.
func withTenants(next *http.ServeMux, registry *tenantRegistry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An empty pattern means the mux would serve its built-in 404 (or
		// redirect) handler; /healthz explicitly opts out of tenant isolation.
		if _, pattern := next.Handler(r); pattern == "" || pattern == healthzPath {
			next.ServeHTTP(w, r)
			return
		}

		values := r.Header.Values(tenantHeader)
		tenant := defaultTenant
		if len(values) > 0 {
			// Exactly one non-empty, well-formed value is accepted; a single
			// header line containing a comma is still one value and simply
			// fails the pattern.
			if len(values) != 1 || !tenantPattern.MatchString(values[0]) {
				writeAPIError(w, "invalid_tenant", http.StatusBadRequest)
				return
			}
			tenant = values[0]
		}

		store := registry.storeFor(tenant)
		ctx := context.WithValue(r.Context(), tenantStoreKey{}, store)
		ctx = context.WithValue(ctx, tenantNameKey{}, tenant)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// tenantStore returns the resolved per-tenant store. It is present for every
// registered /api/v1 endpoint because withTenants runs first.
func tenantStore(r *http.Request) *metricStore {
	return r.Context().Value(tenantStoreKey{}).(*metricStore)
}

// tenantName returns the resolved tenant identifier for the request.
func tenantName(r *http.Request) string {
	return r.Context().Value(tenantNameKey{}).(string)
}
