package supervisor

import (
	"fmt"
	"maps"
	"slices"
	"syscall"
	"time"

	"dboss/internal/notify"
	"dboss/internal/res"
)

// rollState is one rolling restart of a running app. Web instances are replaced one at a time: a
// new copy starts in a free slot next to the old one, and only once it passes its readiness check
// does the route move to it and the old copy drain and stop. Workers are stopped and started
// again, never doubled. Everything runs on the app goroutine and waits on events, so snapshots
// and the proxy keep answering while a copy boots or drains.
type rollState struct {
	// only limits the roll to one procfile process or one instance; "" rolls the whole app.
	only  string
	steps []rollStep
	// current is the new copy waiting for its readiness check, failed why it was killed first.
	current *process
	failed  error
	// waiting is the worker whose exit the next step waits for.
	waiting *process
	// step is the lifecycle start step running ahead of the copies.
	step *jobState
	done []chan error
	// again is set by a restart asked for while this one runs; it rolls once more afterwards.
	again     bool
	againOnly string
	againDone []chan error
}

type rollStepKind int

const (
	stepReplace rollStepKind = iota
	stepWorker
	stepRetire
)

type rollStep struct {
	instance string
	kind     rollStepKind
}

// beginRoll restarts a running app, or one of its web processes, without dropping a request. It
// reports false when a rolling restart does not apply (the app is not running, has no web process
// or the target is a worker) and the caller falls back to a plain restart. done receives the
// outcome.
func (a *appRuntime) beginRoll(only string, done chan error) (bool, error) {
	if a.state != Running || len(a.spec.Config.WebProcesses) == 0 {
		return false, nil
	}
	if only != "" {
		names, err := a.resolve(only)
		if err != nil {
			return false, err
		}
		if proc, _ := splitInstance(names[0]); !a.spec.Config.IsWeb(proc) {
			return false, nil
		}
	}
	if r := a.roll; r != nil {
		if !r.again {
			r.againOnly = only
		} else if r.againOnly != only {
			r.againOnly = ""
		}
		r.again = true
		r.againDone = appendDone(r.againDone, done)
		return true, nil
	}
	a.runRoll(&rollState{only: only, done: appendDone(nil, done)})
	return true, nil
}

func appendDone(list []chan error, done chan error) []chan error {
	if done == nil {
		return list
	}
	return append(list, done)
}

// runRoll starts a roll: the lifecycle start step first when the whole app rolls, then the copies.
func (a *appRuntime) runRoll(r *rollState) {
	a.roll = r
	a.lastError, a.lastErrorProcess = "", ""
	if step := a.lifecycle["start"]; step != nil && r.only == "" {
		if err := a.startJob(step, time.Now(), true); err != nil {
			a.abortRoll(fmt.Errorf("lifecycle start: %w", err))
			return
		}
		r.step = step
		return
	}
	a.planRoll()
	a.advanceRoll()
}

// rollStepDone moves the roll on once its lifecycle start step exits.
func (a *appRuntime) rollStepDone(state *jobState, exitCode int) {
	a.roll.step = nil
	if exitCode != 0 {
		a.abortRoll(fmt.Errorf("lifecycle start: %s", state.lastError))
		return
	}
	a.planRoll()
	a.advanceRoll()
}

// planRoll lists the steps from the current spec, so a changed count takes effect: missing copies
// are started, surplus ones drained.
func (a *appRuntime) planRoll() {
	r := a.roll
	match := func(name string) bool {
		proc, _ := splitInstance(name)
		return r.only == "" || name == r.only || proc == r.only
	}
	// A restart brings back what the operator stopped, as a start does.
	for _, web := range a.spec.Config.WebProcesses {
		for _, name := range a.instancesOf(web.Name) {
			if match(name) {
				delete(a.held, name)
				r.steps = append(r.steps, rollStep{instance: name, kind: stepReplace})
			}
		}
	}
	if r.only == "" {
		for _, proc := range slices.Sorted(maps.Keys(a.spec.Commands)) {
			if a.spec.Config.IsWeb(proc) {
				continue
			}
			for _, name := range a.instancesOf(proc) {
				delete(a.held, name)
				r.steps = append(r.steps, rollStep{instance: name, kind: stepWorker})
			}
		}
	}
	for _, name := range a.knownInstances() {
		if a.processes[name] != nil && !a.wanted(name) && match(name) {
			r.steps = append(r.steps, rollStep{instance: name, kind: stepRetire})
		}
	}
}

// advanceRoll runs steps until one has to wait for an event, and finishes the roll when none are
// left.
func (a *appRuntime) advanceRoll() {
	r := a.roll
	for len(r.steps) > 0 {
		step := r.steps[0]
		r.steps = r.steps[1:]
		old := a.processes[step.instance]
		switch step.kind {
		case stepRetire:
			if old != nil {
				a.retire(old)
			}
		case stepWorker:
			if old != nil {
				a.retire(old)
				r.waiting = old
				return
			}
			if err := a.spawnOne(step.instance); err != nil {
				a.finishRoll(err)
				return
			}
		case stepReplace:
			var log *logWriter
			if old != nil {
				log, _ = old.log.(*logWriter)
			}
			p, err := a.spawn(step.instance, log)
			if err != nil {
				a.abortRoll(err)
				return
			}
			r.current, r.failed = p, nil
			a.watch(p)
			return
		}
	}
	a.finishRoll(nil)
}

// rollEvent handles the new copy of the step in flight. Ready swaps it in; an exit before that
// aborts the roll and leaves the old copy serving.
func (a *appRuntime) rollEvent(event processEvent) {
	r, p := a.roll, event.proc
	switch event.kind {
	case "ready":
		r.current = nil
		old := a.processes[p.name]
		a.processes[p.name] = p
		a.ready[p.name] = true
		a.failures[p.name] = 0
		if old != nil {
			a.retire(old)
		}
		a.advanceRoll()
	case "health-failed":
		r.failed = event.err
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	case "exit":
		err := r.failed
		if err == nil {
			err = fmt.Errorf("exited with code %d before it was ready", event.exitCode)
		}
		a.release(p)
		a.abortRoll(fmt.Errorf("new %s %w", p.name, err))
	}
}

// retire takes a live process out of service: it leaves the route at once, in-flight requests
// get up to its stop_timeout to finish, then it is signalled and, past the same timeout, killed.
// The wait runs off the app goroutine; the retired event releases the process.
func (a *appRuntime) retire(p *process) {
	var drain *upstream
	if a.routes != nil && a.spec.Config.IsWeb(p.proc) {
		drain = a.routes.upstream(a.spec.Name, p.proc, p.port)
	}
	if a.processes[p.name] == p {
		delete(a.processes, p.name)
		delete(a.ready, p.name)
	}
	a.retiring[p] = true
	// The route drops p before the drain starts counting, so no new request can reach it.
	a.publishRoutes()
	defaults := a.spec.Config.Process(p.proc)
	signal, _ := res.Signal(defaults.StopSignal)
	timeout := defaults.StopTimeout.Value()
	go func() {
		deadline := time.Now().Add(timeout)
		for drain != nil && drain.inflight.Load() > 0 && time.Now().Before(deadline) {
			time.Sleep(drainPoll)
		}
		// A stop meanwhile may have reaped it already; its pid could belong to someone else now.
		select {
		case <-p.waited:
			a.sendEvent(processEvent{kind: "retired", proc: p})
			return
		default:
		}
		_ = syscall.Kill(-p.pid, signal)
		if !p.waitUntil(time.Now().Add(timeout)) {
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
			<-p.waited
		}
		a.sendEvent(processEvent{kind: "retired", proc: p})
	}()
}

// retired releases a process retire stopped, and starts the successor of a worker the roll waited
// on.
func (a *appRuntime) retired(p *process) {
	a.release(p)
	r := a.roll
	if r == nil || r.waiting != p {
		return
	}
	r.waiting = nil
	if a.processes[p.name] == nil && a.wanted(p.name) {
		if err := a.spawnOne(p.name); err != nil {
			a.finishRoll(err)
			return
		}
	}
	a.advanceRoll()
}

// abortRoll gives up on the roll in flight and kills its new copy. The old copies keep serving,
// which is the point: a release that never gets healthy does not take the app down.
func (a *appRuntime) abortRoll(err error) {
	r := a.roll
	if r == nil {
		return
	}
	if p := r.current; p != nil {
		r.current = nil
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		<-p.waited
		a.release(p)
	}
	if a.state == Running {
		a.lastError = fmt.Sprintf("restart aborted, the previous version keeps serving: %v", err)
		a.emit(notify.HealthTimeout, a.lastError)
	}
	r.again = false
	a.finishRoll(err)
}

// finishRoll reports the outcome and runs the restart that was asked for meanwhile.
func (a *appRuntime) finishRoll(err error) {
	r := a.roll
	a.roll = nil
	for _, done := range r.done {
		done <- err
	}
	if !r.again {
		for _, done := range r.againDone {
			done <- err
		}
		return
	}
	if a.state != Running {
		for _, done := range r.againDone {
			done <- fmt.Errorf("%s is %s", a.spec.Name, a.state)
		}
		return
	}
	a.runRoll(&rollState{only: r.againOnly, done: r.againDone})
}
