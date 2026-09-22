package daemon

import (
	"io"
	"strings"
	"testing"

	"dboss/internal/super"
)

func TestDisplayHostPicksOneAddress(t *testing.T) {
	for _, test := range []struct {
		name string
		web  super.WebProcessSnapshot
		want string
	}{
		{"canonical wins", super.WebProcessSnapshot{Hosts: []string{"a.example.com", "b.example.com"}, CanonicalHost: "b.example.com"}, "b.example.com"},
		{"first concrete host", super.WebProcessSnapshot{Hosts: []string{"a.example.com", "b.example.com"}}, "a.example.com"},
		{"leading dot matches its apex", super.WebProcessSnapshot{Hosts: []string{".sinatra.lvh.me"}}, "sinatra.lvh.me"},
		{"concrete beats a pattern", super.WebProcessSnapshot{Hosts: []string{"*.example.com", "app.example.com"}}, "app.example.com"},
		{"subdomains only has no apex", super.WebProcessSnapshot{Hosts: []string{"*.example.com"}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := displayHost(test.web); got != test.want {
				t.Fatalf("displayHost = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWebURLCarriesSchemeAndPort(t *testing.T) {
	web := super.WebProcessSnapshot{Hosts: []string{"app.example.com"}}
	if got := webURL(web, "http", ""); got != "http://app.example.com" {
		t.Fatalf("default port should be dropped, got %q", got)
	}
	if got := webURL(web, "http", "3101"); got != "http://app.example.com:3101" {
		t.Fatalf("fallback port missing, got %q", got)
	}
	if got := webURL(web, "https", ""); got != "https://app.example.com" {
		t.Fatalf("tls scheme missing, got %q", got)
	}
	// A pattern with no apex cannot be linked, so it is shown as written rather than as a URL
	// that would not resolve.
	wildcard := super.WebProcessSnapshot{Hosts: []string{"*.example.com"}}
	if got := webURL(wildcard, "http", ""); got != "*.example.com" {
		t.Fatalf("wildcard should print as written, got %q", got)
	}
}

func TestBannerPortDropsTheSchemeDefault(t *testing.T) {
	for _, test := range []struct{ address, scheme, want string }{
		{":80", "http", ""},
		{":443", "https", ""},
		{":443", "http", "443"},
		{"127.0.0.1:3101", "http", "3101"},
		{"not-an-address", "http", ""},
	} {
		if got := bannerPort(test.address, test.scheme); got != test.want {
			t.Fatalf("bannerPort(%q, %q) = %q, want %q", test.address, test.scheme, got, test.want)
		}
	}
}

func TestStateLabelHintsOnlyOnWebRows(t *testing.T) {
	stopped := super.Snapshot{State: super.Stopped}
	if got := stateLabel(stopped, true); got != "stopped, wakes on the first request" {
		t.Fatalf("web row = %q", got)
	}
	// Nobody sends a request to a worker, so the wake hint would be a lie there.
	if got := stateLabel(stopped, false); got != "stopped" {
		t.Fatalf("worker row = %q", got)
	}
	if got := stateLabel(super.Snapshot{State: super.Stopped, WakeButton: true}, true); got != "stopped, needs the start button" {
		t.Fatalf("button app = %q", got)
	}
	if got := stateLabel(super.Snapshot{State: super.Running, Maintenance: true}, true); got != "maintenance" {
		t.Fatalf("maintenance should win, got %q", got)
	}
	if got := stateLabel(super.Snapshot{State: super.Crashed}, true); got != "crashed" {
		t.Fatalf("crashed = %q", got)
	}
}

func TestBannerRowsEveryProcessAndAligns(t *testing.T) {
	echo := super.NewEcho(io.Discard)
	snapshots := []super.Snapshot{
		{
			Name:         "sinatra",
			State:        super.Stopped,
			WebProcesses: []super.WebProcessSnapshot{{Name: "web", Hosts: []string{".sinatra.lvh.me"}, CanonicalHost: "sinatra.lvh.me"}},
			Processes:    []super.ProcessSnapshot{{Name: "web"}, {Name: "job"}},
		},
		{
			Name:         "bun",
			State:        super.Running,
			WebProcesses: []super.WebProcessSnapshot{{Name: "web", Hosts: []string{"bun.lvh.me"}}},
			Processes:    []super.ProcessSnapshot{{Name: "web"}},
		},
	}
	lines := banner(snapshots, "http://127.0.0.1:3100/login?token=x", "signed in for an hour", "http", "", devHTTPS{}, echo)
	if len(lines) != 4 {
		t.Fatalf("want a row per process plus the console, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	// Apps are sorted, and a web process comes before the app's workers.
	for i, want := range []string{"bun/web", "sinatra/web", "sinatra/job", "dboss/console"} {
		if !strings.Contains(lines[i], want) {
			t.Fatalf("line %d = %q, want %s", i, lines[i], want)
		}
	}
	if !strings.Contains(lines[2], "worker") {
		t.Fatalf("worker row missing its marker: %q", lines[2])
	}
	// The escape codes in a key have no width, so the address column has to line up on the
	// visible text instead.
	first := strings.Index(stripANSI(lines[0]), "http://")
	second := strings.Index(stripANSI(lines[1]), "http://")
	if first != second {
		t.Fatalf("addresses not aligned: %d vs %d\n%s", first, second, strings.Join(lines, "\n"))
	}
}

// stripANSI removes the color escapes so a test can measure what the terminal actually shows.
func stripANSI(line string) string {
	var out strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '\x1b' {
			for i < len(line) && line[i] != 'm' {
				i++
			}
			continue
		}
		out.WriteByte(line[i])
	}
	return out.String()
}

// A dev session admits this machine without a session, so its console row is the plain address.
func TestBannerConsoleRowCarriesItsNote(t *testing.T) {
	echo := super.NewEcho(io.Discard)
	lines := banner(nil, "http://127.0.0.1:3100", "open on this machine", "http", "", devHTTPS{}, echo)
	if len(lines) != 1 || !strings.Contains(lines[0], "http://127.0.0.1:3100") || !strings.Contains(lines[0], "open on this machine") {
		t.Fatalf("console row = %v", lines)
	}
	if strings.Contains(lines[0], "token=") {
		t.Fatalf("a dev console link must carry no token: %q", lines[0])
	}
}

// A dev session's HTTPS listener gets one row with the first web host and the trust hint.
func TestBannerShowsDevHTTPSRow(t *testing.T) {
	echo := super.NewEcho(io.Discard)
	snapshots := []super.Snapshot{{Name: "shop", State: super.Running, WebProcesses: []super.WebProcessSnapshot{{Name: "web", Hosts: []string{"shop.lvh.me"}}}}}
	lines := banner(snapshots, "", "", "http", "", devHTTPS{port: "3101", note: "run `dboss trust` once"}, echo)
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	row := stripANSI(lines[1])
	if !strings.Contains(row, "https://shop.lvh.me:3101") || !strings.Contains(row, "dboss trust") {
		t.Fatalf("https row = %q", row)
	}
	if lines := banner(snapshots, "", "", "http", "", devHTTPS{}, echo); len(lines) != 1 {
		t.Fatalf("no dev https should print no https row: %v", lines)
	}
}
