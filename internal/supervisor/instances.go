package supervisor

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// A procfile entry runs as one or more instances: "web" when count is 1, else "web.1".."web.N".
// The instance name keys the runtime maps, the log file and the console row. Each live instance
// sits in a slot ("web", "web#1", ...), which owns its port, cgroup and pid file; a web process
// has one slot more than instances, so a rolling restart can start a copy next to the old one.

func instanceName(proc string, index, count int) string {
	if count <= 1 {
		return proc
	}
	return proc + "." + strconv.Itoa(index)
}

// splitInstance returns the procfile name and 1-based index of an instance. Procfile names never
// contain a dot, so the split is unambiguous.
func splitInstance(name string) (string, int) {
	proc, index, ok := strings.Cut(name, ".")
	if !ok {
		return name, 1
	}
	n, err := strconv.Atoi(index)
	if err != nil || n < 1 {
		return name, 1
	}
	return proc, n
}

func slotName(proc string, k int) string {
	if k == 0 {
		return proc
	}
	return proc + "#" + strconv.Itoa(k)
}

// slotCount is the number of slots assigned up front: one per instance, plus the spare a web
// process rolls into.
func slotCount(count int, web bool) int {
	if web {
		return count + 1
	}
	return count
}

// instancesOf lists the instances of one procfile entry the current spec asks for.
func (a *appRuntime) instancesOf(proc string) []string {
	count := a.spec.Config.Procfile[proc].Instances()
	names := make([]string, count)
	for i := range names {
		names[i] = instanceName(proc, i+1, count)
	}
	return names
}

// knownInstances is every instance the spec asks for plus any live one it no longer does, in
// procfile then index order.
func (a *appRuntime) knownInstances() []string {
	seen := map[string]bool{}
	for _, proc := range slices.Sorted(maps.Keys(a.spec.Commands)) {
		for _, name := range a.instancesOf(proc) {
			seen[name] = true
		}
	}
	for name := range a.processes {
		seen[name] = true
	}
	return slices.SortedFunc(maps.Keys(seen), compareInstances)
}

func compareInstances(x, y string) int {
	xp, xi := splitInstance(x)
	yp, yi := splitInstance(y)
	return cmp.Or(cmp.Compare(xp, yp), cmp.Compare(xi, yi))
}

// wanted reports whether the spec still asks for the instance, so a scale-down does not bring a
// dropped copy back through the restart policy.
func (a *appRuntime) wanted(name string) bool {
	proc, _ := splitInstance(name)
	return slices.Contains(a.instancesOf(proc), name)
}

// resolve turns a procfile name or one instance name into the instances an action applies to.
func (a *appRuntime) resolve(name string) ([]string, error) {
	if _, ok := a.spec.Commands[name]; ok {
		return a.instancesOf(name), nil
	}
	proc, _ := splitInstance(name)
	if _, ok := a.spec.Commands[proc]; ok && slices.Contains(a.knownInstances(), name) {
		return []string{name}, nil
	}
	return nil, fmt.Errorf("unknown process %q", name)
}

// tracked lists every process the runtime owns: live, retiring and a rolling replacement.
func (a *appRuntime) tracked() []*process {
	list := slices.Collect(maps.Values(a.processes))
	list = append(list, slices.Collect(maps.Keys(a.retiring))...)
	if a.roll != nil && a.roll.current != nil {
		list = append(list, a.roll.current)
	}
	return list
}

// freeSlot is the first slot of proc no tracked process holds.
func (a *appRuntime) freeSlot(proc string) string {
	used := map[string]bool{}
	for _, p := range a.tracked() {
		used[p.slot] = true
	}
	for k := 0; ; k++ {
		if slot := slotName(proc, k); !used[slot] {
			return slot
		}
	}
}

// webServing reports whether any instance of a web process is ready for traffic.
func (a *appRuntime) webServing(proc string) bool {
	for name, p := range a.processes {
		if p.proc == proc && a.ready[name] {
			return true
		}
	}
	return false
}

// publishRoutes hands the proxy the ports of every ready web instance.
func (a *appRuntime) publishRoutes() {
	if a.routes == nil {
		return
	}
	routes := map[string][]int{}
	for _, web := range a.spec.Config.WebProcesses {
		var ports []int
		for _, name := range a.liveInstances(web.Name) {
			if p := a.processes[name]; p != nil && a.ready[name] {
				ports = append(ports, p.port)
			}
		}
		if len(ports) > 0 {
			routes[web.Name] = ports
		}
	}
	a.routes.setApp(a.spec.Name, routes)
}

// liveInstances lists the live instances of proc in index order.
func (a *appRuntime) liveInstances(proc string) []string {
	var names []string
	for name, p := range a.processes {
		if p.proc == proc {
			names = append(names, name)
		}
	}
	slices.SortFunc(names, compareInstances)
	return names
}
