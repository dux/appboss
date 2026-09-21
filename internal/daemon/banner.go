package daemon

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"dboss/internal/logx"
	"dboss/internal/super"
)

// printBanner writes the startup summary of a hand-run session. It is terminal-only, the same
// signal that drives the output echo and the privileged-port fallback, so a service start still
// logs nothing extra and never mints a console link.
func (d *Daemon) printBanner() {
	if d.echo == nil || len(d.listen) == 0 {
		return
	}
	scheme := "http"
	if d.cfg.Proxy.TLS.Enabled() {
		scheme = "https"
	}
	console := ""
	if d.management != nil {
		link, err := d.management.DevLoginURL()
		if err != nil {
			logx.Warnf("console link: %v", err)
		} else {
			console = link
		}
	}
	for _, line := range banner(d.manager.Snapshots(), console, scheme, bannerPort(d.listen[0], scheme), d.echo) {
		d.echo.Print(line)
	}
}

// banner is the startup summary a hand-run session prints: one row per process, keyed by the
// same colored prefix that process logs under, so the address and its later output line up.
// Web processes carry a clickable URL, workers say so, and every row ends in the app's state,
// which is how an app that has not started yet is still visible.
func banner(snapshots []super.Snapshot, console string, scheme, port string, echo *super.Echo) []string {
	rows := make([]bannerRow, 0, len(snapshots))
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Name < snapshots[j].Name })
	for _, app := range snapshots {
		web := map[string]bool{}
		for _, process := range app.WebProcesses {
			web[process.Name] = true
			rows = append(rows, newRow(echo, app.Name, process.Name, webURL(process, scheme, port), stateLabel(app, true), true))
		}
		for _, process := range app.Processes {
			if web[process.Name] {
				continue
			}
			rows = append(rows, newRow(echo, app.Name, process.Name, "worker", stateLabel(app, false), true))
		}
	}
	if console != "" {
		// The sign-in link carries a token, so it is far longer than any hostname. Keeping it
		// out of the column width stops one row from stretching every other one.
		rows = append(rows, newRow(echo, "dboss", "console", console, "signed in for an hour", false))
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

// bannerRow is one line. key carries the color escapes, so its printed width has to be tracked
// separately or every column after it is misaligned.
type bannerRow struct {
	key      string
	keyWidth int
	address  string
	note     string
	pad      bool
}

func newRow(echo *super.Echo, app, proc, address, note string, pad bool) bannerRow {
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
func webURL(web super.WebProcessSnapshot, scheme, port string) string {
	host := displayHost(web)
	if host == "" {
		return strings.Join(web.Hosts, ", ")
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host
}

// displayHost picks the one hostname worth printing: the canonical host when the process
// declares one (every other host redirects to it anyway), then the first concrete host, then a
// leading-dot pattern, which matches its own apex.
func displayHost(web super.WebProcessSnapshot) string {
	if web.CanonicalHost != "" {
		return web.CanonicalHost
	}
	for _, host := range web.Hosts {
		if !strings.HasPrefix(host, "*.") && !strings.HasPrefix(host, ".") {
			return host
		}
	}
	for _, host := range web.Hosts {
		if strings.HasPrefix(host, ".") {
			return strings.TrimPrefix(host, ".")
		}
	}
	return ""
}

// stateLabel says what the app is doing and, on a web row, what would start it. Only a web row
// gets the hint: a request is what wakes an app, and nobody sends one to a worker.
func stateLabel(app super.Snapshot, web bool) string {
	if app.Maintenance {
		return "maintenance"
	}
	if app.State != super.Stopped {
		return string(app.State)
	}
	if !web {
		return string(super.Stopped)
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
