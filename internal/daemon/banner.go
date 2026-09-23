package daemon

import (
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"

	"dboss/internal/config"
	"dboss/internal/logx"
	"dboss/internal/supervisor"
	"dboss/internal/version"
)

// printBanner writes the startup summary of a hand-run session. It is terminal-only, the same
// signal that drives the output echo and the privileged-port fallback, so a service start still
// logs nothing extra and never mints a console link.
func (d *Daemon) printBanner() {
	if d.echo == nil || len(d.listen) == 0 {
		return
	}
	scheme, port := d.appAddress()
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
	d.echo.Print(bannerIntro(d.cfg))
	for _, line := range banner(d.manager.Snapshots(), console, note, scheme, port, secure, d.echo) {
		d.echo.Print(line)
	}
}

// appAddress is the scheme and port a browser reaches the apps on through the proxy. A dev
// session keeps plain http on proxy.listen; its HTTPS gets its own banner row. The scheme is
// empty when no proxy is listening.
func (d *Daemon) appAddress() (scheme, port string) {
	if len(d.listen) == 0 {
		return "", ""
	}
	if d.cfg.Proxy.TLS.Enabled() && !d.cfg.Dev() {
		return "https", bannerPort(d.cfg.Proxy.TLS.Listen, "https")
	}
	return "http", bannerPort(d.listen[0], "http")
}

// bannerIntro names the build and what this session runs, above the rows.
func bannerIntro(cfg config.Config) string {
	if cfg.Dev() {
		return fmt.Sprintf("dboss %s - dev session for %s:", version.String(), filepath.Base(cfg.Dir))
	}
	return fmt.Sprintf("dboss %s - host %s:", version.String(), cfg.Dir)
}

// banner is the startup summary a hand-run session prints before any app starts: one row per
// process, keyed by the same colored prefix that process logs under, so the address and its later
// output line up. Web processes carry a clickable URL and workers say so.
func banner(snapshots []supervisor.Snapshot, console, consoleNote string, scheme, port string, secure devHTTPS, echo *supervisor.Echo) []string {
	rows := make([]bannerRow, 0, len(snapshots))
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Name < snapshots[j].Name })
	secureURL := ""
	for _, app := range snapshots {
		web := map[string]bool{}
		for _, process := range app.WebProcesses {
			web[process.Name] = true
			rows = append(rows, newRow(app.Name, process.Name, webURL(process, scheme, port), bannerNote(app), true))
			if secureURL == "" && config.PrimaryHost(process.CanonicalHost, process.Hosts) != "" {
				secureURL = webURL(process, "https", secure.port)
			}
		}
		for _, process := range app.Processes {
			if web[process.Name] {
				continue
			}
			rows = append(rows, newRow(app.Name, process.Name, "worker", bannerNote(app), true))
		}
	}
	if secure.note != "" && secureURL != "" {
		rows = append(rows, newRow("dboss", "https", secureURL, secure.note, true))
	}
	if console != "" {
		// A sign-in link carries a token, so it is far longer than any hostname. Keeping it out
		// of the column width stops one row from stretching every other one.
		rows = append(rows, newRow("dboss", "console", console, consoleNote, false))
	}
	// Register every name before rendering any key, so all rows pad to the widest one.
	for _, row := range rows {
		echo.Key(row.app, row.proc)
	}
	addressWidth := 0
	for _, row := range rows {
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
		lines = append(lines, strings.TrimRight(echo.Key(row.app, row.proc)+address+"  "+row.note, " "))
	}
	return lines
}

// devHTTPS is the dev session's HTTPS listener as the banner shows it: the port to print and
// whether the browser will trust it. A zero value prints nothing.
type devHTTPS struct {
	port string
	note string
}

// bannerRow is one line: the process it names, its address and a note. pad lines the address up
// with the other rows.
type bannerRow struct {
	app, proc string
	address   string
	note      string
	pad       bool
}

func newRow(app, proc, address, note string, pad bool) bannerRow {
	return bannerRow{app: app, proc: proc, address: address, note: note, pad: pad}
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

// bannerNote is what a process row adds after its address. The banner prints before any app
// starts, so the state would always read stopped; only maintenance changes what a click shows.
func bannerNote(app supervisor.Snapshot) string {
	if app.Maintenance {
		return "maintenance"
	}
	return ""
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
