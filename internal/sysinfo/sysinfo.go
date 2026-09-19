// Package sysinfo inspects the host the daemon runs on: OS and kernel facts, live resource
// samples and the toolchains that are installed. It is read-only: it never changes dboss or
// app state, runs no mutating command and writes no audit row. A Module keeps a snapshot warm
// for the console's Sys tab.
package sysinfo

import (
	"context"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"dboss/internal/version"
)

const (
	// defaultInterval is how often host and disk facts are re-sampled. Probing every installed
	// tool is slower, so it runs at most every toolInterval.
	defaultInterval    = 30 * time.Second
	defaultToolRefresh = 10 * time.Minute
	probeTimeout       = 2 * time.Second
)

// toolEnv is the non-secret environment dboss forwards to the console for context.
var toolEnv = []string{"PATH", "HOME", "USER", "SHELL", "LANG", "TZ"}

// Tool is one probed executable: where it is, what version it reports, or that it is missing.
// Home is the project's public page, so the console can link the name.
type Tool struct {
	Name    string `json:"name"`
	Found   bool   `json:"found"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Home    string `json:"home,omitempty"`
	Error   string `json:"error,omitempty"`
}

// DirSpec names a directory dboss uses, so the inspector can report its disk usage.
type DirSpec struct {
	Name string
	Path string
}

// Dir is one directory dboss uses plus the filesystem it lives on.
type Dir struct {
	Name       string  `json:"name"`
	Path       string  `json:"path"`
	TotalBytes int64   `json:"total_bytes"`
	FreeBytes  int64   `json:"free_bytes"`
	UsedBytes  int64   `json:"used_bytes"`
	Percent    float64 `json:"percent"`
	Error      string  `json:"error,omitempty"`
}

// Host is the machine dboss runs on.
type Host struct {
	Hostname  string  `json:"hostname"`
	OS        string  `json:"os"`
	OSName    string  `json:"os_name,omitempty"`
	Arch      string  `json:"arch"`
	Kernel    string  `json:"kernel,omitempty"`
	UptimeSec int64   `json:"uptime_seconds,omitempty"`
	CPUs      int     `json:"cpus"`
	Load1     float64 `json:"load1,omitempty"`
	Load5     float64 `json:"load5,omitempty"`
	Load15    float64 `json:"load15,omitempty"`
	MemTotal  int64   `json:"mem_total,omitempty"`
	MemFree   int64   `json:"mem_free,omitempty"`
	MemUsed   int64   `json:"mem_used,omitempty"`
	SwapTotal int64   `json:"swap_total,omitempty"`
	SwapFree  int64   `json:"swap_free,omitempty"`
}

// Runtime is the running dboss process itself.
type Runtime struct {
	Dboss      string            `json:"dboss"`
	GoVersion  string            `json:"go_version"`
	PID        int               `json:"pid"`
	Goroutines int               `json:"goroutines"`
	GOMAXPROCS int               `json:"gomaxprocs"`
	Env        map[string]string `json:"env,omitempty"`
}

// Snapshot is one inspection result.
type Snapshot struct {
	CollectedAt time.Time `json:"collected_at"`
	Host        Host      `json:"host"`
	Runtime     Runtime   `json:"runtime"`
	Tools       []Tool    `json:"tools"`
	Dirs        []Dir     `json:"dirs"`
}

type probe struct {
	name string
	args []string
	home string
}

// defaultProbes is the curated toolchain list in display order. A tool that is missing is shown
// as missing rather than omitted, so the tab doubles as an inventory. home is the project page
// the console links the tool name to.
var defaultProbes = []probe{
	{"go", []string{"version"}, "https://go.dev"},
	{"git", []string{"--version"}, "https://git-scm.com"},
	{"node", []string{"--version"}, "https://nodejs.org"},
	{"npm", []string{"--version"}, "https://www.npmjs.com"},
	{"npx", []string{"--version"}, "https://www.npmjs.com"},
	{"bun", []string{"--version"}, "https://bun.sh"},
	{"deno", []string{"--version"}, "https://deno.com"},
	{"yarn", []string{"--version"}, "https://yarnpkg.com"},
	{"pnpm", []string{"--version"}, "https://pnpm.io"},
	{"ruby", []string{"--version"}, "https://www.ruby-lang.org"},
	{"gem", []string{"--version"}, "https://rubygems.org"},
	{"bundle", []string{"--version"}, "https://bundler.io"},
	{"python3", []string{"--version"}, "https://www.python.org"},
	{"python", []string{"--version"}, "https://www.python.org"},
	{"pip3", []string{"--version"}, "https://pip.pypa.io"},
	{"uv", []string{"--version"}, "https://docs.astral.sh/uv"},
	{"php", []string{"--version"}, "https://www.php.net"},
	{"composer", []string{"--version"}, "https://getcomposer.org"},
	{"java", []string{"-version"}, "https://openjdk.org"},
	{"sqlite3", []string{"--version"}, "https://sqlite.org"},
	{"psql", []string{"--version"}, "https://www.postgresql.org"},
	{"redis-cli", []string{"--version"}, "https://redis.io"},
	{"lsof", []string{"-v"}, "https://github.com/lsof-org/lsof"},
	{"rsync", []string{"--version"}, "https://rsync.samba.org"},
	{"curl", []string{"--version"}, "https://curl.se"},
	{"docker", []string{"--version"}, "https://www.docker.com"},
	{"podman", []string{"--version"}, "https://podman.io"},
	{"systemctl", []string{"--version"}, "https://www.freedesktop.org/wiki/Software/systemd"},
	{"make", []string{"--version"}, "https://www.gnu.org/software/make"},
	{"gcc", []string{"--version"}, "https://gcc.gnu.org"},
	{"jq", []string{"--version"}, "https://jqlang.github.io/jq"},
	{"mise", []string{"--version"}, "https://mise.jdx.dev"},
	{"asdf", []string{"--version"}, "https://asdf-vm.com"},
	{"tar", []string{"--version"}, "https://www.gnu.org/software/tar"},
}

// Inspector collects and caches one Snapshot. Snapshot is cheap and safe for any goroutine;
// Refresh re-samples the host and re-probes the tools when the tool interval has elapsed.
type Inspector struct {
	dirs         []DirSpec
	probes       []probe
	toolInterval time.Duration
	lookPath     func(string) (string, error)
	run          func(context.Context, string, ...string) (string, error)

	mu        sync.RWMutex
	snapshot  Snapshot
	lastTools time.Time
}

func NewInspector(dirs []DirSpec) *Inspector {
	return &Inspector{
		dirs:         dirs,
		probes:       defaultProbes,
		toolInterval: defaultToolRefresh,
		lookPath:     exec.LookPath,
		run:          runCommand,
	}
}

// Snapshot returns the last collected inspection without touching the host.
func (i *Inspector) Snapshot() Snapshot {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.snapshot
}

// Refresh re-samples the host and directories and, when the tool interval has elapsed, re-probes
// every toolchain. It stores and returns the new snapshot.
func (i *Inspector) Refresh(ctx context.Context) Snapshot {
	now := time.Now()
	i.mu.RLock()
	previous, lastTools := i.snapshot, i.lastTools
	i.mu.RUnlock()

	tools := previous.Tools
	if len(tools) == 0 || now.Sub(lastTools) >= i.toolInterval {
		tools = i.collectTools(ctx)
		lastTools = now
	}
	snapshot := Snapshot{
		CollectedAt: now,
		Host:        collectHost(),
		Runtime:     collectRuntime(),
		Tools:       tools,
		Dirs:        collectDirs(i.dirs),
	}
	i.mu.Lock()
	i.snapshot, i.lastTools = snapshot, lastTools
	i.mu.Unlock()
	return snapshot
}

func (i *Inspector) collectTools(ctx context.Context) []Tool {
	tools := make([]Tool, 0, len(i.probes))
	for _, p := range i.probes {
		tools = append(tools, i.probe(ctx, p))
	}
	return tools
}

// probe finds one tool and records what it prints. A tool that is absent is data, not an error;
// a probe that exits non-zero (lsof -v does) still keeps any version text it printed.
func (i *Inspector) probe(ctx context.Context, p probe) Tool {
	tool := Tool{Name: p.name, Home: p.home}
	path, err := i.lookPath(p.name)
	if err != nil {
		return tool
	}
	tool.Found, tool.Path = true, path
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	output, runErr := i.run(probeCtx, p.name, p.args...)
	tool.Version = firstLine(output)
	if runErr != nil && tool.Version == "" {
		tool.Error = runErr.Error()
	}
	return tool
}

func collectRuntime() Runtime {
	env := map[string]string{}
	for _, name := range toolEnv {
		if value := os.Getenv(name); value != "" {
			env[name] = value
		}
	}
	return Runtime{
		Dboss:      version.String(),
		GoVersion:  runtime.Version(),
		PID:        os.Getpid(),
		Goroutines: runtime.NumGoroutine(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		Env:        env,
	}
}

func collectDirs(refs []DirSpec) []Dir {
	dirs := make([]Dir, 0, len(refs))
	for _, ref := range refs {
		dir := Dir{Name: ref.Name, Path: ref.Path}
		if err := diskUsage(ref.Path, &dir); err != nil {
			dir.Error = err.Error()
		}
		dirs = append(dirs, dir)
	}
	return dirs
}

// diskUsage fills total, free and used bytes from statfs. Used counts reserved blocks, so it can
// differ from total minus the free space a process may use.
func diskUsage(path string, dir *Dir) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return err
	}
	block := uint64(stat.Bsize)
	dir.TotalBytes = int64(stat.Blocks * block)
	dir.FreeBytes = int64(stat.Bavail * block)
	dir.UsedBytes = dir.TotalBytes - int64(stat.Bfree*block)
	if dir.TotalBytes > 0 {
		dir.Percent = math.Round(float64(dir.UsedBytes)/float64(dir.TotalBytes)*1000) / 10
	}
	return nil
}

// hostBasics is the portable part of the host facts; each platform adds uptime, load and memory.
func hostBasics() Host {
	host := Host{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU()}
	host.Hostname, _ = os.Hostname()
	host.Kernel = firstLine(runOutput("uname", "-r"))
	host.OSName = osName()
	return host
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(output), err
}

// runOutput runs a command for host facts where a failure just leaves the fact empty.
func runOutput(name string, args ...string) string {
	output, _ := exec.Command(name, args...).Output()
	return string(output)
}

func firstLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// Module keeps an Inspector warm on a timer so the console's first load already has data.
type Module struct {
	inspector *Inspector
	interval  time.Duration
	cancel    context.CancelFunc
	done      chan struct{}
}

func New(dirs []DirSpec) *Module {
	return &Module{inspector: NewInspector(dirs), interval: defaultInterval, done: make(chan struct{})}
}

func (m *Module) Name() string          { return "sysinfo" }
func (m *Module) Inspector() *Inspector { return m.inspector }

func (m *Module) Start(ctx context.Context) error {
	// Probing every tool can take a moment, so collection runs off the daemon's start path.
	ctx, m.cancel = context.WithCancel(ctx)
	go m.loop(ctx)
	return nil
}

func (m *Module) Close() error {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
	return nil
}

func (m *Module) loop(ctx context.Context) {
	defer close(m.done)
	m.inspector.Refresh(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.inspector.Refresh(ctx)
		}
	}
}
