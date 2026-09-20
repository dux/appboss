package apps

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dboss/internal/config"
	"dboss/internal/schedule"
)

var processName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
var hostName = regexp.MustCompile(`^(\*\.|\.)?([a-zA-Z0-9-]+\.)*[a-zA-Z0-9-]+$`)

type Command struct {
	Name string   `json:"name"`
	Line string   `json:"line"`
	Argv []string `json:"argv"`
}

type App struct {
	Name     string             `json:"name"`
	Dir      string             `json:"dir"`
	Commands map[string]Command `json:"commands"`
	Cron     map[string]CronJob `json:"cron"`
	Hooks    map[string]Hook    `json:"hooks"`
	// Env is the daemon environment plus mise; FileEnv is .env overlaid by .env.local. Config
	// env sits between them at process start, so it is applied when the process env is built.
	Env     map[string]string `json:"-"`
	FileEnv map[string]string `json:"-"`
	Config  config.App        `json:"config"`
}

// CronJob is one scheduled command with its schedule parsed once at load time.
type CronJob struct {
	Command  Command           `json:"command"`
	Schedule schedule.Schedule `json:"-"`
	Timeout  time.Duration     `json:"timeout"`
	Overlap  bool              `json:"overlap"`
	Disabled bool              `json:"disabled"`
}

// Hook is one named one-shot command with its command line parsed once at load time.
type Hook struct {
	Command  Command       `json:"command"`
	Timeout  time.Duration `json:"timeout"`
	Restart  bool          `json:"restart"`
	Overlap  bool          `json:"overlap"`
	Disabled bool          `json:"disabled"`
}

type ScanError struct {
	Name string `json:"name"`
	Err  error  `json:"-"`
}

func (e ScanError) Error() string { return e.Name + ": " + e.Err.Error() }

// Discover loads every app the root config describes: the config's own folder in single mode,
// otherwise each entry of the apps directory. Entries are walked in name order, which decides
// port assignment and host-conflict precedence.
func Discover(cfg config.Config) ([]*App, []error, error) {
	var found []*App
	var invalid []error
	if cfg.App != nil {
		name := filepath.Base(cfg.Dir)
		app, err := loadRootApp(cfg, name)
		if err != nil {
			invalid = append(invalid, ScanError{Name: name, Err: err})
		} else {
			found = append(found, app)
		}
		return resolveHosts(found, invalid)
	}
	entries, err := appNames(cfg.Apps)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range entries {
		app, err := loadChildApp(cfg, name, filepath.Join(cfg.Apps, name))
		if err != nil {
			invalid = append(invalid, ScanError{Name: name, Err: err})
			continue
		}
		found = append(found, app)
	}
	return resolveHosts(found, invalid)
}

// appNames lists the entries of an apps directory, in name order, skipping dotfiles.
func appNames(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("apps directory: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// Lookup discovers every app and returns the one called name, or its scan error when it is
// invalid, so the CLI and the console print the same thing for the same app.
func Lookup(cfg config.Config, name string) (*App, error) {
	found, invalid, err := Discover(cfg)
	if err != nil {
		return nil, err
	}
	for _, scanErr := range invalid {
		if scanError, ok := scanErr.(ScanError); ok && scanError.Name == name {
			return nil, scanErr
		}
	}
	for _, app := range found {
		if app.Name == name {
			return app, nil
		}
	}
	return nil, fmt.Errorf("unknown app %q", name)
}

func resolveHosts(found []*App, invalid []error) ([]*App, []error, error) {
	owners := map[string]string{}
	valid := found[:0]
	for _, app := range found {
		conflict := ""
		for _, host := range app.Config.Hosts {
			normalized := strings.ToLower(strings.TrimSuffix(host, "."))
			if owner := owners[normalized]; owner != "" {
				conflict = fmt.Sprintf("host pattern %q is already owned by %s", host, owner)
				break
			}
		}
		if conflict != "" {
			invalid = append(invalid, ScanError{Name: app.Name, Err: errors.New(conflict)})
			continue
		}
		for _, host := range app.Config.Hosts {
			owners[strings.ToLower(strings.TrimSuffix(host, "."))] = app.Name
		}
		valid = append(valid, app)
	}
	return valid, invalid, nil
}

// loadRootApp re-reads the root file so a rescan in single mode picks up edits to it.
func loadRootApp(cfg config.Config, name string) (*App, error) {
	loaded, err := config.Load(cfg.SourcePath)
	if err != nil {
		return nil, err
	}
	if loaded.App == nil {
		return nil, fmt.Errorf("%s no longer describes an app", cfg.SourcePath)
	}
	return buildApp(name, cfg.Dir, *loaded.App)
}

// loadChildApp loads one entry of the apps directory. The entry path, not its symlink target,
// is the process working directory so a target that is itself a release symlink keeps working.
func loadChildApp(cfg config.Config, name, dir string) (*App, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("app path %q is not a directory", dir)
	}
	path, err := config.FindInDir(dir)
	if err != nil {
		return nil, err
	}
	appCfg, err := config.LoadApp(path, cfg.Defaults)
	if err != nil {
		return nil, err
	}
	return buildApp(name, dir, appCfg)
}

// validateApp checks what the config loader cannot see on its own: the procfile parses, every
// referenced process exists and host patterns are well formed. Host conflicts need the whole
// fleet and stay in resolveHosts.
func validateApp(appCfg config.App) (map[string]Command, error) {
	commands, err := ParseProcfile(appCfg.Procfile)
	if err != nil {
		return nil, err
	}
	for _, web := range appCfg.WebProcesses {
		if _, ok := commands[web.Name]; !ok {
			return nil, fmt.Errorf("web process %q is not in procfile", web.Name)
		}
	}
	for process := range appCfg.Processes {
		if _, ok := commands[process]; !ok {
			return nil, fmt.Errorf("processes.%s is not in procfile", process)
		}
	}
	for _, host := range appCfg.Hosts {
		if !hostName.MatchString(strings.ToLower(strings.TrimSuffix(host, "."))) {
			return nil, fmt.Errorf("invalid host pattern %q", host)
		}
	}
	return commands, nil
}

func buildApp(name, dir string, appCfg config.App) (*App, error) {
	commands, err := validateApp(appCfg)
	if err != nil {
		return nil, err
	}
	cron, err := buildCron(appCfg.Cron)
	if err != nil {
		return nil, err
	}
	hooks, err := buildHooks(appCfg.Hooks)
	if err != nil {
		return nil, err
	}
	env := minimalEnvironment()
	if _, err := os.Stat(filepath.Join(dir, "mise.toml")); err == nil {
		mise, miseErr := miseEnvironment(dir)
		if miseErr != nil {
			return nil, miseErr
		}
		merge(env, mise)
	}
	// .env and .env.local stay separate so they can outrank the config env when the process
	// environment is assembled (see supervisor.environment).
	fileEnv := map[string]string{}
	for _, filename := range []string{".env", ".env.local"} {
		values, envErr := LoadEnv(filepath.Join(dir, filename))
		if envErr != nil {
			return nil, envErr
		}
		merge(fileEnv, values)
	}
	return &App{Name: name, Dir: dir, Commands: commands, Cron: cron, Hooks: hooks, Env: env, FileEnv: fileEnv, Config: appCfg}, nil
}

// buildCron parses every schedule once so the supervisor only has to work with next run times.
func buildCron(jobs map[string]config.CronJob) (map[string]CronJob, error) {
	result := make(map[string]CronJob, len(jobs))
	for name, job := range jobs {
		parsed, err := schedule.Parse(job.Schedule)
		if err != nil {
			return nil, fmt.Errorf("cron.%s.schedule: %w", name, err)
		}
		result[name] = CronJob{
			Command:  Command{Name: name, Line: job.Command, Argv: strings.Fields(job.Command)},
			Schedule: parsed,
			Timeout:  job.Timeout.Value(),
			Overlap:  job.Overlap,
			Disabled: job.Disabled,
		}
	}
	return result, nil
}

// buildHooks parses every hook command once; hooks have no schedule, they only fire on a ping.
func buildHooks(hooks map[string]config.Hook) (map[string]Hook, error) {
	result := make(map[string]Hook, len(hooks))
	for name, hook := range hooks {
		result[name] = Hook{
			Command:  Command{Name: name, Line: hook.Command, Argv: strings.Fields(hook.Command)},
			Timeout:  hook.Timeout.Value(),
			Restart:  hook.Restart,
			Overlap:  hook.Overlap,
			Disabled: hook.Disabled,
		}
	}
	return result, nil
}

func ParseProcfile(procfile map[string]config.ProcessSpec) (map[string]Command, error) {
	commands := map[string]Command{}
	for name, process := range procfile {
		line := strings.TrimSpace(process.Command)
		if !processName.MatchString(name) {
			return nil, fmt.Errorf("procfile has invalid process name %q", name)
		}
		if line == "" {
			return nil, fmt.Errorf("procfile.%s command is empty", name)
		}
		commands[name] = Command{Name: name, Line: line, Argv: strings.Fields(line)}
	}
	return commands, nil
}

func LoadEnv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !validEnvKey(key) {
			return nil, fmt.Errorf("%s:%d: expected KEY=value", path, lineNumber)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			if value[0] == '\'' {
				value = value[1 : len(value)-1]
			} else {
				decoded, decodeErr := strconv.Unquote(value)
				if decodeErr != nil {
					return nil, fmt.Errorf("%s:%d: %w", path, lineNumber, decodeErr)
				}
				value = decoded
			}
		}
		result[key] = value
	}
	return result, scanner.Err()
}

func validEnvKey(key string) bool {
	if key == "" || !(key[0] == '_' || key[0] >= 'A' && key[0] <= 'Z' || key[0] >= 'a' && key[0] <= 'z') {
		return false
	}
	for i := 1; i < len(key); i++ {
		character := key[i]
		if !(character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

func minimalEnvironment() map[string]string {
	result := map[string]string{}
	for _, key := range []string{"HOME", "LANG", "LC_ALL", "TZ", "PATH", "USER"} {
		if value, ok := os.LookupEnv(key); ok {
			result[key] = value
		}
	}
	return result
}

func miseEnvironment(dir string) (map[string]string, error) {
	cmd := exec.Command("mise", "env", "--json")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("mise env: %w", err)
	}
	var values map[string]string
	if err := json.Unmarshal(output, &values); err != nil {
		return nil, fmt.Errorf("mise env JSON: %w", err)
	}
	return values, nil
}

func merge(target, source map[string]string) {
	for key, value := range source {
		target[key] = value
	}
}
