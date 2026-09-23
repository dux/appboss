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
	"strconv"
	"strings"
	"time"

	"dboss/internal/config"
	"dboss/internal/schedule"
)

type Command struct {
	Name string   `json:"name"`
	Line string   `json:"line"`
	Argv []string `json:"argv,omitempty"` // dboss exec only
}

type App struct {
	Name     string             `json:"name"`
	Dir      string             `json:"dir"`
	Commands map[string]Command `json:"commands"`
	Cron     map[string]CronJob `json:"cron"`
	Hooks    map[string]Hook    `json:"hooks"`
	// Lifecycle holds the create, start and destroy steps that are set.
	Lifecycle map[string]Step `json:"lifecycle"`
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

// Hook is one named one-shot command with its command line parsed once at load time. Pull marks
// the `deploy: true` shorthand, whose git command authenticates with tokens.github.
type Hook struct {
	Command  Command       `json:"command"`
	Timeout  time.Duration `json:"timeout"`
	Restart  bool          `json:"restart"`
	Overlap  bool          `json:"overlap"`
	Disabled bool          `json:"disabled"`
	Pull     bool          `json:"pull,omitempty"`
}

// Step is one lifecycle command with its timeout resolved.
type Step struct {
	Command Command       `json:"command"`
	Timeout time.Duration `json:"timeout"`
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
	if cfg.Dev() {
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
			normalized := config.NormalizePattern(host)
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
			owners[config.NormalizePattern(host)] = app.Name
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

func buildApp(name, dir string, appCfg config.App) (*App, error) {
	commands := make(map[string]Command, len(appCfg.Procfile))
	for name, process := range appCfg.Procfile {
		commands[name] = newCommand(name, process.Command)
	}
	cron, err := buildCron(appCfg.Cron)
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
		values, envErr := loadEnv(filepath.Join(dir, filename))
		if envErr != nil {
			return nil, envErr
		}
		merge(fileEnv, values)
	}
	return &App{Name: name, Dir: dir, Commands: commands, Cron: cron, Hooks: buildHooks(appCfg.Hooks), Lifecycle: buildLifecycle(appCfg.Lifecycle), Env: env, FileEnv: fileEnv, Config: appCfg}, nil
}

// newCommand is one config command line, run through /bin/sh -c; config has already refused an
// empty one.
func newCommand(name, line string) Command {
	return Command{Name: name, Line: strings.TrimSpace(line)}
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
			Command:  newCommand(name, job.Command),
			Schedule: parsed,
			Timeout:  job.Timeout.Value(),
			Overlap:  job.Overlap,
			Disabled: job.Disabled,
		}
	}
	return result, nil
}

// buildHooks parses every hook command once; hooks have no schedule, they only fire on a ping.
func buildHooks(hooks map[string]config.Hook) map[string]Hook {
	result := make(map[string]Hook, len(hooks))
	for name, hook := range hooks {
		result[name] = Hook{
			Command:  newCommand(name, hook.Command),
			Timeout:  hook.Timeout.Value(),
			Restart:  hook.Restart,
			Overlap:  hook.Overlap,
			Disabled: hook.Disabled,
			Pull:     hook.Pull,
		}
	}
	return result
}

// buildLifecycle parses each step once and applies the default timeout.
func buildLifecycle(steps map[string]config.LifecycleCommand) map[string]Step {
	result := make(map[string]Step, len(steps))
	for name, step := range steps {
		timeout := step.Timeout.Value()
		if timeout == 0 {
			timeout = config.DefaultLifecycleTimeout
		}
		result[name] = Step{Command: newCommand(name, step.Command), Timeout: timeout}
	}
	return result
}

func loadEnv(path string) (map[string]string, error) {
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
