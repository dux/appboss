package supervisor

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"dboss/internal/config"
)

// healthInterval is the readiness poll period while a web process starts, and healthDialTimeout
// bounds one check's connect. healthInterval is a var so tests can poll faster.
var healthInterval = 500 * time.Millisecond

const healthDialTimeout = 2 * time.Second

// monitor walks the web process from spawn to exit. It first polls every healthInterval until
// the process answers (readiness, bounded by health_timeout), then slows to liveness_interval
// and reports a health failure after unhealthy_threshold consecutive failures. The runtime handles that exactly like a crash, so
// restart policy, backoff and max_restarts apply. unhealthy_threshold: 0 stops after readiness.
// It is given the process defaults, host and poll interval instead of reading the app spec or the
// package var, which a rescan or a test may replace while it runs.
func (a *appRuntime) monitor(p *process, defaults config.Process, host string, interval time.Duration) {
	deadline := time.Now().Add(defaults.HealthTimeout.Value())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	ready := false
	failures := 0
	lastError := errors.New("healthcheck timed out")
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-p.done:
			return
		case <-ticker.C:
			ok, err := healthCheck(defaults.Health, p.port, host, healthDialTimeout)
			if ok {
				if !ready {
					ready = true
					a.sendEvent(processEvent{kind: "ready", proc: p})
					if defaults.UnhealthyThreshold <= 0 {
						return
					}
					// Readiness is decided; from here the check only has to notice a process
					// that died quietly, so it drops to the slower liveness cadence instead of
					// hitting the app's health path twice a second for its whole life.
					ticker.Reset(a.livenessInterval(defaults))
				}
				failures = 0
				continue
			}
			if err != nil {
				lastError = err
			}
			if !ready {
				if time.Now().After(deadline) {
					a.sendEvent(processEvent{kind: "health-failed", proc: p, err: lastError})
					return
				}
				continue
			}
			failures++
			if failures >= defaults.UnhealthyThreshold {
				a.sendEvent(processEvent{kind: "health-failed", proc: p, err: fmt.Errorf("unhealthy after %d failed checks: %w", failures, lastError)})
				return
			}
		}
	}
}

// devLivenessInterval is how often a hand-run session re-checks a process that is already up.
// A developer watching one app does not need it polled every ten seconds, and every poll shows
// up in their own request log; a server keeps the configured cadence so a hung process is
// noticed quickly.
const devLivenessInterval = 5 * time.Minute

// livenessInterval is the ongoing check's period. A terminal session (the same signal behind the
// output echo, the startup banner and the privileged-port fallback) is relaxed to
// devLivenessInterval. It only ever slows the check down, so an app that asks for something
// longer still gets it.
func (a *appRuntime) livenessInterval(defaults config.Process) time.Duration {
	interval := defaults.LivenessInterval.Value()
	if a.echo != nil && interval < devLivenessInterval {
		return devLivenessInterval
	}
	return interval
}

// healthCheck talks to the process the way the proxy does: loopback address, app hostname in Host.
// An empty or "tcp" check only dials the port; anything else is a path GET expecting 2xx.
func healthCheck(check string, port int, host string, timeout time.Duration) (bool, error) {
	address := fmt.Sprintf("127.0.0.1:%d", port)
	if check == "" || check == "tcp" {
		connection, err := net.DialTimeout("tcp", address, timeout)
		if err == nil {
			_ = connection.Close()
			return true, nil
		}
		return false, fmt.Errorf("healthcheck on tcp failed: %w", err)
	}
	path := check
	client := &http.Client{Timeout: timeout}
	request, err := http.NewRequest(http.MethodGet, "http://"+address+path, nil)
	if err != nil {
		return false, err
	}
	if host != "" {
		request.Host = host
	}
	response, err := client.Do(request)
	if err != nil {
		return false, fmt.Errorf("healthcheck on %s failed: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("healthcheck on %s returned %d", path, response.StatusCode)
	}
	return true, nil
}
