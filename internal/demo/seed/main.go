// Command seed recreates the demo's SQLite databases with dummy traffic, logs, audit rows and
// blocked counters, so a fresh console has something to show. It is the body of `make seed`; stop
// the running demo first, because it deletes the databases it seeds. It writes through
// ./internal/logstore, so the schema stays single-sourced with the daemon.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dboss/internal/events"
	"dboss/internal/logstore"
)

func main() {
	dir := flag.String("dir", "./demo/.dboss/log", "log directory that holds one database per app")
	socket := flag.String("socket", "", "control socket to refuse when a daemon is live (default: <dir>/../dboss.sock)")
	flag.Parse()
	apps := flag.Args()
	if len(apps) == 0 {
		apps = []string{"bun", "sinatra", "button", "scratch"}
	}
	if *socket == "" {
		*socket = filepath.Join(filepath.Dir(*dir), "dboss.sock")
	}
	if daemonRunning(*socket) {
		fmt.Fprintf(os.Stderr, "seed: a daemon is live on %s; stop the demo before seeding (%s deletes databases)\n", *socket, *dir)
		os.Exit(1)
	}
	if err := seed(*dir, apps, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
	fmt.Printf("seeded %d app(s) and the host database under %s\n", len(apps), *dir)
}

// daemonRunning reports whether a live dboss answers on the control socket, so seeding never
// deletes the databases out from under a running daemon. A missing or stale socket is not running.
func daemonRunning(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// seed wipes every database and events directory under dir and writes fresh dummy data. now
// anchors the timestamps so the traffic range, the last hour and the audit view all have rows.
func seed(dir string, apps []string, now time.Time) error {
	for _, app := range append(append([]string{}, apps...), logstore.HostApp) {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(filepath.Join(dir, app, "dboss.sqlite"+suffix)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	for _, app := range apps {
		if err := os.RemoveAll(filepath.Join(dir, app, events.DirName)); err != nil {
			return err
		}
	}
	store := logstore.New(dir, 50*time.Millisecond, nil, "", time.Hour, 0)
	rng := rand.New(rand.NewSource(now.UnixNano()))
	for _, app := range apps {
		seedApp(store, rng, app, app+".lvh.me", now)
		if err := seedEvents(dir, rng, app, now); err != nil {
			_ = store.Close()
			return err
		}
	}
	seedHost(store, rng, now)
	if err := store.Close(); err != nil {
		return err
	}
	// Exceptions last, on a fresh store: the batched request/log writers above are stopped, so
	// the synchronous exception upsert never contends with them for SQLite's write lock.
	exceptions := logstore.New(dir, 50*time.Millisecond, nil, "", time.Hour, 0)
	if len(apps) > 0 {
		if err := seedExceptions(exceptions, rng, apps[0], now); err != nil {
			_ = exceptions.Close()
			return err
		}
	}
	return exceptions.Close()
}

var (
	seedMethods  = []string{"GET", "GET", "GET", "GET", "POST", "HEAD"}
	seedPaths    = []string{"/", "/up", "/about", "/shop", "/assets/app.css", "/api/items", "/favicon.ico", "/robots.txt", "/login", "/search?q=shoes"}
	seedAgents   = []string{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36", "curl/8.4.0", "Googlebot/2.1 (+http://www.google.com/bot.html)", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)"}
	seedCountry  = []string{"US", "DE", "HR", "GB", "FR", "", "NL", "JP"}
	seedLevels   = []string{"info", "info", "info", "warn", "error"}
	seedBlocked  = map[string]int{"/wp-login.php": 148, "/xmlrpc.php": 73, "/.env": 51, "/.git/config": 34, "/admin.php": 27, "/phpmyadmin": 19, "/shell.asp": 12, "/cgi-bin/test.cgi": 6}
	seedAuditRow = []struct{ actor, app, action, detail string }{
		{"ops@example.com", "bun", "start", "web"},
		{"ops@example.com", "sinatra", "restart", "rolling restart"},
		{"cli@localhost", "bun", "config-changed", "bun/dboss.yaml"},
		{"hook:bun/deploy", "bun", "deploy", "main@1a2b3c"},
		{"ops@example.com", "scratch", "destroy", "removed"},
	}
)

// seedApp writes a week of requests and a day of stdout logs for one app, so its Traffic and Logs
// views are populated and its error-rate alert has a subject.
func seedApp(store *logstore.Store, rng *rand.Rand, app, host string, now time.Time) {
	for i := 0; i < 600; i++ {
		status := 200
		switch n := rng.Intn(100); {
		case n >= 94:
			status = 500
		case n >= 89:
			status = 404
		case n >= 86:
			status = 302
		}
		_ = store.Record(app, 336*time.Hour, logstore.RequestEntry{
			Time:       now.Add(-time.Duration(rng.Intn(7*24*60)) * time.Minute),
			Method:     seedMethods[rng.Intn(len(seedMethods))],
			Host:       host,
			Path:       seedPaths[rng.Intn(len(seedPaths))],
			Status:     status,
			DurationMS: int64(4 + rng.Intn(400)),
			BytesOut:   int64(200 + rng.Intn(20000)),
			IP:         fmt.Sprintf("203.0.113.%d", rng.Intn(254)+1),
			UserAgent:  seedAgents[rng.Intn(len(seedAgents))],
			RequestID:  randomID(rng),
			Process:    "web",
			Country:    seedCountry[rng.Intn(len(seedCountry))],
		})
	}
	for i := 0; i < 120; i++ {
		level := seedLevels[rng.Intn(len(seedLevels))]
		path := seedPaths[rng.Intn(len(seedPaths))]
		message := fmt.Sprintf("GET %s completed 200 in %dms", path, 4+rng.Intn(400))
		switch level {
		case "warn":
			message = "slow request: " + path
		case "error":
			message = "ActiveRecord::ConnectionTimeoutError: could not obtain a connection"
		}
		_ = store.RecordLogs(app, []logstore.LogEntry{{
			Time:      now.Add(-time.Duration(rng.Intn(24*60)) * time.Minute),
			Source:    "stdout",
			Process:   "web",
			Stream:    "stdout",
			Level:     level,
			Message:   message,
			RequestID: randomID(rng),
			Raw:       message,
		}})
	}
}

// seedException is one fingerprint the demo can write: a realistic class, message and origin,
// plus the users and IPs it was reported for.
type seedException struct {
	klass, message, origin, description string
	users, ips                          []string
}

// seedExceptionPool is the two fingerprints written for the first seeded app. The other apps
// stay empty so the Exceptions tab is one app's list.
var seedExceptionPool = []seedException{
	{"PG::ConnectionBad", "PG::ConnectionBad: connection refused", "app/controllers/checkout_controller.rb:42:in `create'", "confirming an order", []string{"u_1024", "u_2048"}, []string{"203.0.113.7", "198.51.100.4"}},
	{"NoMethodError", "NoMethodError: undefined method `total' for nil", "app/services/report.rb:88:in `build'", "building the monthly report", []string{"u_7"}, []string{"192.0.2.55"}},
}

// seedExceptions writes two fingerprints for one app, each with a busy minute and a quiet one,
// so the Exceptions tab has groups and their per-minute breakdowns.
func seedExceptions(store *logstore.Store, rng *rand.Rand, app string, now time.Time) error {
	picked := seedExceptionPool
	groups := make([]logstore.ExceptionGroup, 0, len(picked))
	for i, fp := range picked {
		uid := exceptionUID(app, fp.klass, fp.origin)
		dump := fmt.Sprintf("%s: %s (%s)", fp.origin, fp.message, fp.klass)
		base := now.Add(-time.Duration(30*(i+1)) * time.Minute).Truncate(time.Minute)
		counts := []int{80 + rng.Intn(60), 3 + rng.Intn(6)}
		group := logstore.ExceptionGroup{ExpUID: uid, Dump: dump, FirstAt: base}
		for m, count := range counts {
			minute := base.Add(time.Duration(m) * time.Minute)
			group.Count += count
			group.LastAt = minute.Add(30 * time.Second)
			group.Minutes = append(group.Minutes, logstore.ExceptionMinute{
				MinuteAt:    minute,
				Count:       count,
				Message:     fp.message,
				Tags:        []string{strings.ToLower(strings.SplitN(fp.klass, "::", 2)[0])},
				Description: fp.description,
				Users:       fp.users,
				IPs:         fp.ips,
			})
		}
		groups = append(groups, group)
	}
	if err := store.AppendExceptions(app, logstore.ExceptionBatch{Groups: groups}); err != nil {
		return err
	}
	// One group ships resolved, so the console shows both states out of the box.
	return store.SetExceptionResolved(app, exceptionUID(app, picked[1].klass, picked[1].origin), true)
}

// exceptionUID is a stand-in for the Lux writer's SHA-256 of [file, line, class].
func exceptionUID(app, klass, origin string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join([]string{app, origin, klass}, "\x00"))))
}

// seedHost fills the reserved host database: audit rows, deny counters and a few daemon log lines.
func seedHost(store *logstore.Store, rng *rand.Rand, now time.Time) {
	for i, row := range seedAuditRow {
		_ = store.RecordAudit(logstore.AuditEntry{Time: now.Add(-time.Duration(i+1) * 17 * time.Minute), Actor: row.actor, App: row.app, Action: row.action, Detail: row.detail, Result: "ok"})
	}
	for path, count := range seedBlocked {
		for i := 0; i < count; i++ {
			_ = store.RecordBlocked(path)
		}
	}
	messages := []string{"dboss ready: listen=:80 management=dboss.lvh.me", "wake bun: starting web", "rescan: 4 apps", "notify: posted crash for scratch", "log prune and vacuum done"}
	for i := 0; i < 40; i++ {
		message := messages[rng.Intn(len(messages))]
		_ = store.RecordLogs(logstore.HostApp, []logstore.LogEntry{{
			Time:    now.Add(-time.Duration(rng.Intn(12*60)) * time.Minute),
			Source:  "dboss",
			Stream:  "stderr",
			Level:   "info",
			Message: message,
			Raw:     message,
		}})
	}
}

// seedEvents writes one Parquet batch for an app under the "app" namespace: a pool of anonymous
// visitors that move pricing -> signup -> paid, with page views and a plan tag, so the demo's
// 4-step funnel and its tag filters have real data. It matches `events:` in demo/apps/bun/dboss.yaml.
func seedEvents(logDir string, rng *rand.Rand, app string, now time.Time) error {
	plans := []string{"free", "pro", "team"}
	countries := []string{"US", "DE", "HR", "GB", "FR", "NL", "JP"}
	rows := make([]events.Row, 0, 600)
	var eid uint64
	add := func(ts time.Time, event, anon, user, country string, value *float64, tags ...string) {
		eid++
		rows = append(rows, events.Row{
			EID:     eid,
			TS:      ts.UTC().Truncate(time.Millisecond),
			Event:   event,
			AnonID:  anon,
			UserID:  user,
			Country: country,
			Value:   value,
			Tags:    events.NormalizeTags(tags),
		})
	}
	for i := 0; i < 150; i++ {
		anon := fmt.Sprintf("a_%04d", i)
		plan := plans[rng.Intn(len(plans))]
		country := countries[rng.Intn(len(countries))]
		start := now.Add(-time.Duration(rng.Intn(7*24*60)) * time.Minute)
		add(start, "page_view", anon, "", country, nil, "page:pricing", "plan:"+plan)
		for j := rng.Intn(3); j > 0; j-- {
			add(start.Add(-time.Duration(1+rng.Intn(120))*time.Minute), "page_view", anon, "", country, nil, "page:home", "plan:"+plan)
		}
		cursor := start
		if rng.Intn(100) < 70 {
			cursor = cursor.Add(time.Duration(1+rng.Intn(30)) * time.Minute)
			add(cursor, "signup_started", anon, "", country, nil, "plan:"+plan)
		}
		if rng.Intn(100) < 50 {
			cursor = cursor.Add(time.Duration(1+rng.Intn(60)) * time.Minute)
			add(cursor, "signup_completed", anon, "u_"+anon, country, nil, "plan:"+plan)
		}
		if rng.Intn(100) < 30 {
			cursor = cursor.Add(time.Duration(1+rng.Intn(120)) * time.Minute)
			price := map[string]float64{"free": 0, "pro": 29, "team": 99}[plan]
			add(cursor, "checkout_completed", anon, "u_"+anon, country, &price, "plan:"+plan)
		}
	}
	return events.NewStore(logDir).Append(app, "app", rows)
}

func randomID(rng *rand.Rand) string {
	const hex = "0123456789abcdef"
	id := make([]byte, 16)
	for i := range id {
		id[i] = hex[rng.Intn(len(hex))]
	}
	return string(id)
}
