package zerotier

import (
	"net/netip"
	"sync"
)

// ZT_ROUTE_FLAG_ACTIVE from ZeroTierOne.h.
const RouteFlagActive = 0x0001

// RouteTable holds the current usable ZT managed routes. It is rebuilt from
// the ZT config snapshot on every change and queried per packet.
//
// Design decision (M1): a 0.0.0.0/0 (or ::/0) managed route would route ALL
// traffic to ZeroTier and starve mihomo. In this dual-exit app such a route
// is an operator misconfiguration: it is ignored with a one-time warning.
type RouteTable struct {
	mu            sync.RWMutex
	routes        []Route
	warnedDefault bool
}

// NewRouteTable returns an empty route table.
func NewRouteTable() *RouteTable { return &RouteTable{} }

// Set replaces the route set. Default routes are filtered out.
func (t *RouteTable) Set(routes []Route) {
	keep := make([]Route, 0, len(routes))
	for _, r := range routes {
		if r.Prefix.Bits() == 0 {
			if !t.warnedDefault {
				t.warnedDefault = true
				Warnf("[ZT] ignoring default managed route (0.0.0.0/0 or ::/0): routing everything via ZeroTier would starve mihomo")
			}
			continue
		}
		keep = append(keep, r)
	}
	t.mu.Lock()
	t.routes = keep
	t.mu.Unlock()
}

// Clear removes all routes (network down / destroyed / engine stopped).
func (t *RouteTable) Clear() {
	t.mu.Lock()
	t.routes = nil
	t.mu.Unlock()
}

// Count returns the number of usable routes.
func (t *RouteTable) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.routes)
}

// Match returns the longest-prefix route containing addr, or nil.
// Ties are broken by lower metric.
func (t *RouteTable) Match(addr netip.Addr) *Route {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var best *Route
	bestBits := -1
	for i := range t.routes {
		r := &t.routes[i]
		if !r.Prefix.Contains(addr) {
			continue
		}
		bits := r.Prefix.Bits()
		if bits < bestBits {
			continue
		}
		if bits == bestBits && best != nil && r.Metric >= best.Metric {
			continue
		}
		best = r
		bestBits = bits
	}
	return best
}

// Snapshot returns a copy of the current routes (diagnostics).
func (t *RouteTable) Snapshot() []Route {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]Route(nil), t.routes...)
}
