package daemon

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"dboss/internal/config"
	"dboss/internal/logx"
	"dboss/internal/supervisor"
)

// printBanner writes the startup summary of a hand-run session. It is terminal-only, the same
// signal that drives the output echo and the privileged-port fallback, so a service start still
// logs nothing extra and never mints a console link.
func (d *Daemon) printBanner() {
	if d.echo == nil || len(d.listen) == 0 {
		return
	}
	scheme := "http"
	// A dev session keeps plain http on proxy.listen; its HTTPS gets its own row below.
	if d.cfg.Proxy.TLS.Enabled() && !d.cfg.Dev() {
		scheme = "https"
	}
	console, note := "", ""
	switch {
	case d.management == nil:
	case d.cfg.Dev():
		// A dev session admits this machine without a session, so the link needs no token.
		console, note = d.cfg.ConsoleURL(), "open on this machine"
	default:
		link, err := d.management.DevLoginURL()
		if err != nil {
			logx.Warnf("console link: %v", err)
		} else {
			console, note = link, "signed in for an hour"
		}
	}
	secure := devHTTPS{}
	if d.devHTTPS != "" {
		secure = devHTTPS{port: bannerPort(d.devHTTPS, "https"), note: "run `dboss trust` once so the browser accepts it"}
		if d.devTLS.Trusted() {
			secure.note = "trusted local certificate"
		}
	}
	for _, line := range banner(d.manager.Snapshots(), console, note, scheme, bannerPort(d.listen[0], scheme), secure, d.echo) {
		d.echo.Print(line)
	}
}

// banner is the startup summary a hand-run session prints: one row per process, keyed by the
// same colored prefix that process logs under, so the address and its later output line up.
// Web processes carry a clickable URL, workers say so, and every row ends in the app's state,
// which is how an app that has not started yet is still visible.
func banner(snapshots []supervisor.Snapshot, console, consoleNote string, scheme, port string, secure devHTTPS, echo *supervisor.Echo) []string {
	rows := make([]bannerRow, 0, len(snapshots))
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Name < snapshots[j].Name })
	secureURL := ""
	for _, app := range snapshots {
		web := map[string]bool{}
		for _, process := range app.WebProcesses {
			web[process.Name] = true
			rows = append(rows, newRow(echo, app.Name, process.Name, webURL(process, scheme, port), stateLabel(app, true), true))
			if secureURL == "" && config.PrimaryHost(process.CanonicalHost, process.Hosts) != "" {
				secureURL = webURL(process, "https", secure.port)
			}
		}
		for _, process := range app.Processes {
			if web[process.Name] {
				continue
			}
			rows = append(rows, newRow(echo, app.Name, process.Name, "worker", stateLabel(app, false), true))
		}
	}
	if secure.note != "" && secureURL != "" {
		rows = append(rows, newRow(echo, "dboss", "https", secureURL, secure.note, true))
	}
	if console != "" {
		// A sign-in link carries a token, so it is far longer than any hostname. Keeping it out
		// of the column width stops one row from stretching every other one.
		rows = append(rows, newRow(echo, "dboss", "console", console, consoleNote, false))
	}
	keyWidth, addressWidth := 0, 0
	for _, row := range rows {
		keyWidth = max(keyWidth, row.keyWidth)
		if row.pad {
			addressWidth = max(addressWidth, len(row.address))
		}
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		address := row.address
		if row.pad {
			address = fmt.Sprintf("%-*s", addressWidth, address)
		}
		lines = append(lines, row.key+strings.Repeat(" ", keyWidth-row.keyWidth)+address+"  "+row.note)
	}
	return lines
}

// devHTTPS is the dev session's HTTPS listener as the banner shows it: the port to print and
// whether the browser will trust it. A zero value prints nothing.
type devHTTPS struct {
	port string
	note string
}

// bannerRow is one line. key carries the color escapes, so its printed width has to be tracked
// separately or every column after it is misaligned.
type bannerRow struct {
	key      string
	keyWidth int
	address  string
	note     string
	pad      bool
}

func newRow(echo *supervisor.Echo, app, proc, address, note string, pad bool) bannerRow {
	return bannerRow{
		key:      echo.Key(app, proc),
		keyWidth: len(echo.Name(app, proc)) + len(" | "),
		address:  address,
		note:     note,
		pad:      pad,
	}
}

// webURL is the address to open for one web process. A pattern that matches only subdomains has
// no address of its own, so it is printed as written instead of being turned into a link that
// would not resolve.
func webURL(web supervisor.WebProcessSnapshot, scheme, port string) string {
	host := config.PrimaryHost(web.CanonicalHost, web.Hosts)
	if host == "" {
		return strings.Join(web.Hosts, ", ")
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host
}

// stateLabel says what the app is doing and, on a web row, what would start it. Only a web row
// gets the hint: a request is what wakes an app, and nobody sends one to a worker.
func stateLabel(app supervisor.Snapshot, web bool) string {
	if app.Maintenance {
		return "maintenance"
	}
	if app.State != supervisor.Stopped {
		return string(app.State)
	}
	if !web {
		return string(supervisor.Stopped)
	}
	if app.WakeButton {
		return "stopped, needs the start button"
	}
	return "stopped, wakes on the first request"
}

// bannerPort is the port to put in a printed URL: the one actually bound, dropped when it is
// the default for the scheme and the browser would add it back.
func bannerPort(address, scheme string) string {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		return ""
	}
	return port
}
