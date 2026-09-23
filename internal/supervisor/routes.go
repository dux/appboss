package supervisor

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// upstream is one ready copy of a web process the proxy may send a request to.
type upstream struct {
	port     int
	inflight atomic.Int64
}

type routeSet struct {
	next      atomic.Uint64
	upstreams []*upstream
}

// routeTable maps app/web to the ports of its ready copies. A pick counts the request on its
// upstream under the same lock a change takes, so once set drops a port no request can still
// land on it and the old copy's counter only goes down.
type routeTable struct {
	mu   sync.RWMutex
	sets map[string]*routeSet
}

func newRouteTable() *routeTable { return &routeTable{sets: map[string]*routeSet{}} }

func routeKey(app, web string) string { return app + "/" + web }

// setApp replaces every route of one app. An upstream that stays keeps its object, so its
// in-flight count carries over.
func (t *routeTable) setApp(app string, routes map[string][]int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.sets {
		if web, ok := strings.CutPrefix(key, app+"/"); ok && routes[web] == nil {
			delete(t.sets, key)
		}
	}
	for _, web := range slices.Sorted(maps.Keys(routes)) {
		ports := routes[web]
		key := routeKey(app, web)
		current := t.sets[key]
		if current != nil && samePorts(current.upstreams, ports) {
			continue
		}
		set := &routeSet{}
		for _, port := range ports {
			var kept *upstream
			if current != nil {
				kept = find(current.upstreams, port)
			}
			if kept == nil {
				kept = &upstream{port: port}
			}
			set.upstreams = append(set.upstreams, kept)
		}
		t.sets[key] = set
	}
}

// upstream returns the live upstream on port, nil when the port is not routed.
func (t *routeTable) upstream(app, web string, port int) *upstream {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if set := t.sets[routeKey(app, web)]; set != nil {
		return find(set.upstreams, port)
	}
	return nil
}

// pick chooses the upstream with the fewest requests in flight, starting the scan one further
// each call so equal ones take turns, and counts the request on it.
func (t *routeTable) pick(app, web string) *upstream {
	t.mu.RLock()
	defer t.mu.RUnlock()
	set := t.sets[routeKey(app, web)]
	if set == nil || len(set.upstreams) == 0 {
		return nil
	}
	count := len(set.upstreams)
	start := int(set.next.Add(1) % uint64(count))
	best := set.upstreams[start]
	for i := 1; i < count; i++ {
		if candidate := set.upstreams[(start+i)%count]; candidate.inflight.Load() < best.inflight.Load() {
			best = candidate
		}
	}
	best.inflight.Add(1)
	return best
}

func find(upstreams []*upstream, port int) *upstream {
	for _, u := range upstreams {
		if u.port == port {
			return u
		}
	}
	return nil
}

func samePorts(upstreams []*upstream, ports []int) bool {
	if len(upstreams) != len(ports) {
		return false
	}
	for i, u := range upstreams {
		if u.port != ports[i] {
			return false
		}
	}
	return true
}
