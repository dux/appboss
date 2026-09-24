package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"dboss/internal/config"
	"dboss/internal/fsutil"
	"dboss/internal/supervisor"
)

// syncManifestName is the file in the remote app folder listing what the last sync shipped. It is
// what lets apply delete a file the app dropped without ever touching one that only lives on the box.
const syncManifestName = ".dboss-sync"

// deployHook is the app hook `deploy git` pings; `hooks: {deploy: true}` is the pull-and-restart.
const deployHook = "deploy"

// deployWait bounds how long `deploy git` waits for the pull and the restart.
const deployWait = 15 * time.Minute

// deployPoll is how often `deploy git` asks for the hook state; a var so tests can poll faster.
var deployPoll = time.Second

var deployClient = &http.Client{Timeout: 30 * time.Second}

// syncProtected are never removed by apply, even when an earlier sync shipped them: they are
// server-only by convention and may have been placed on the box since.
var syncProtected = []string{syncManifestName, config.LocalFileName, ".env.local"}

// remoteSafe limits what goes into the ssh command line, which the remote shell parses.
var remoteSafe = regexp.MustCompile(`^[A-Za-z0-9._/@+=:,-]+$`)

func (c CLI) deploy(args []string) error {
	if len(args) == 0 {
		return c.help(c.Out, "deploy")
	}
	switch args[0] {
	case "sync":
		return c.deploySync(args[1:])
	case "git":
		return c.deployGit(args[1:])
	case "apply":
		return c.deployApply(args[1:])
	case "help":
		return c.help(c.Out, "deploy")
	default:
		return fmt.Errorf("unknown deploy subcommand %q (use sync or git)", args[0])
	}
}

// deploySync ships the tracked files of the app folder with rsync, then runs `deploy apply` on the
// box over ssh, which drops what the app no longer ships and restarts it.
func (c CLI) deploySync(args []string) error {
	set := flag.NewFlagSet("deploy sync", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := configFlag(set)
	app := set.String("app", "", "app name on the box (default: the remote folder name)")
	var dryRun bool
	set.BoolVar(&dryRun, "n", false, "show what would change")
	set.BoolVar(&dryRun, "dry-run", false, "show what would change")
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return err
	}
	if len(operands) != 1 {
		return errors.New("usage: dboss deploy sync <user@host:/path> [--app name] [-n]")
	}
	host, remotePath, err := splitTarget(operands[0])
	if err != nil {
		return err
	}
	if *app == "" {
		*app = path.Base(remotePath)
	}
	if !remoteSafe.MatchString(*app) {
		return fmt.Errorf("app name %q has characters the remote shell would read", *app)
	}
	here := &workdir{explicit: *configPath}
	configFile, cfg, err := here.load()
	if err != nil {
		return err
	}
	if cfg.App == nil {
		return fmt.Errorf("%s is a host config; run deploy sync inside an app folder", configFile)
	}
	files, untracked, err := syncManifest(cfg.Dir)
	if err != nil {
		return err
	}
	if untracked > 0 {
		fmt.Fprintf(c.Err, "skipped %d untracked %s (git add to ship them)\n", untracked, plural(untracked, "file"))
	}
	manifest := strings.Join(files, "\n") + "\n"

	rsync := exec.Command("rsync", rsyncArgs(host, remotePath, dryRun)...)
	rsync.Dir = cfg.Dir
	rsync.Stdin = strings.NewReader(manifest)
	rsync.Stdout, rsync.Stderr = c.Out, c.Err
	if err := rsync.Run(); err != nil {
		return fmt.Errorf("rsync: %w", err)
	}
	ssh := exec.Command("ssh", applyArgs(host, remotePath, *app, dryRun)...)
	ssh.Stdin = strings.NewReader(manifest)
	ssh.Stdout, ssh.Stderr = c.Out, c.Err
	if err := ssh.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return &exitError{code: exit.ExitCode()}
		}
		return fmt.Errorf("ssh: %w", err)
	}
	if !dryRun {
		fmt.Fprintf(c.Out, "deployed %s: %d files synced to %s:%s\n", *app, len(files), host, remotePath)
	}
	return nil
}

// splitTarget reads rsync's host:path form. The path must be absolute, and both parts are kept
// to characters the remote shell passes through untouched.
func splitTarget(target string) (string, string, error) {
	host, remotePath, ok := strings.Cut(target, ":")
	if !ok || host == "" || strings.HasPrefix(host, "-") {
		return "", "", fmt.Errorf("target %q is not user@host:/path", target)
	}
	if !path.IsAbs(remotePath) {
		return "", "", fmt.Errorf("remote path %q must be absolute", remotePath)
	}
	remotePath = path.Clean(remotePath)
	if remotePath == "/" || !remoteSafe.MatchString(host) || !remoteSafe.MatchString(remotePath) {
		return "", "", fmt.Errorf("target %q has characters the remote shell would read", target)
	}
	return host, remotePath, nil
}

// syncManifest lists the files git tracks in dir, relative to it, and counts the untracked files
// git does not ignore, so a missing `git add` is visible. Untracked files are never shipped: a file
// the box needs that git does not carry is placed there on purpose.
func syncManifest(dir string) ([]string, int, error) {
	tracked, err := gitList(dir, "ls-files", "-z", "--cached", "--recurse-submodules")
	if err != nil {
		return nil, 0, err
	}
	files := make([]string, 0, len(tracked))
	for _, name := range tracked {
		if name == syncManifestName {
			continue
		}
		if strings.ContainsAny(name, "\n\r") {
			return nil, 0, fmt.Errorf("cannot ship %q: the name has a line break", name)
		}
		// A tracked file deleted from the working tree is not shipped, and apply then removes it.
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, 0, err
		}
		files = append(files, name)
	}
	untracked, err := gitList(dir, "ls-files", "-z", "--others", "--exclude-standard", "--directory", "--no-empty-directory")
	if err != nil {
		return nil, 0, err
	}
	return files, len(untracked), nil
}

func gitList(dir string, args ...string) ([]string, error) {
	command := exec.Command("git", args...)
	command.Dir = dir
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "not a git repository") {
			return nil, fmt.Errorf("deploy sync ships the files git tracks, and %s is not in a git repository", dir)
		}
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	var names []string
	for name := range strings.SplitSeq(string(out), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// rsyncArgs copies exactly the listed files. rsync's own --delete is never used: openrsync, the
// macOS default, removes gitignored files on the receiver with it. apply deletes instead.
func rsyncArgs(host, remotePath string, dryRun bool) []string {
	args := []string{"-az", "--files-from=-"}
	if dryRun {
		args = append(args, "-n", "-v")
	}
	return append(args, "./", host+":"+remotePath+"/")
}

func applyArgs(host, remotePath, app string, dryRun bool) []string {
	args := []string{host, "dboss", "deploy", "apply", remotePath, "--app", app}
	if dryRun {
		args = append(args, "-n")
	}
	return args
}

// deployApply is the box's half of `deploy sync`: it reads the new manifest on stdin, removes the
// files the previous manifest listed and this one does not, records the new manifest and restarts
// the app through the control socket.
func (c CLI) deployApply(args []string) error {
	set := flag.NewFlagSet("deploy apply", flag.ContinueOnError)
	set.SetOutput(c.Err)
	app := set.String("app", "", "app name (default: the folder name)")
	socket := set.String("socket", "", "control socket")
	var dryRun bool
	set.BoolVar(&dryRun, "n", false, "list what would be removed")
	set.BoolVar(&dryRun, "dry-run", false, "list what would be removed")
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return err
	}
	if len(operands) != 1 || !filepath.IsAbs(operands[0]) {
		return errors.New("usage: dboss deploy apply </absolute/app/path> [--app name] [-n]")
	}
	root := filepath.Clean(operands[0])
	if *app == "" {
		*app = filepath.Base(root)
	}
	// A mistyped path must delete nothing, so the folder has to be an app.
	if configFile, err := config.FindInDir(root); err != nil {
		return fmt.Errorf("%s holds no app config: %w", root, err)
	} else if cfg, err := config.Load(configFile); err != nil {
		return err
	} else if cfg.App == nil {
		return fmt.Errorf("%s is a host config, not an app", configFile)
	}
	next, err := readManifest(c.In)
	if err != nil {
		return err
	}
	var previous []string
	if data, err := os.ReadFile(filepath.Join(root, syncManifestName)); err == nil {
		if previous, err = readManifest(bytes.NewReader(data)); err != nil {
			return fmt.Errorf("%s: %w", syncManifestName, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	removed := staleFiles(previous, next)
	if dryRun {
		for _, name := range removed {
			fmt.Fprintf(c.Out, "would remove %s\n", name)
		}
		return nil
	}
	for _, name := range removed {
		if err := removeShipped(root, name); err != nil {
			fmt.Fprintf(c.Err, "keep %s: %v\n", name, err)
		}
	}
	if len(removed) > 0 {
		fmt.Fprintf(c.Out, "removed %d %s the app no longer ships\n", len(removed), plural(len(removed), "file"))
	}
	if err := fsutil.WriteFile(filepath.Join(root, syncManifestName), []byte(strings.Join(next, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	restart := []string{*app}
	if *socket != "" {
		restart = append(restart, "--socket", *socket)
	}
	if err := c.remote("restart", restart); err != nil {
		return fmt.Errorf("restart %s: %w", *app, err)
	}
	return nil
}

// readManifest reads one relative path per line and refuses anything that would leave the app
// folder, so a manifest can never name a file outside it.
func readManifest(in io.Reader) ([]string, error) {
	var names []string
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		name := scanner.Text()
		if name == "" {
			continue
		}
		if !filepath.IsLocal(name) {
			return nil, fmt.Errorf("manifest entry %q leaves the app folder", name)
		}
		names = append(names, filepath.Clean(name))
	}
	return names, scanner.Err()
}

// staleFiles is what previous listed and next no longer does, minus the protected names, sorted.
func staleFiles(previous, next []string) []string {
	keep := make(map[string]bool, len(next))
	for _, name := range next {
		keep[name] = true
	}
	var stale []string
	for _, name := range previous {
		if !keep[name] && !slices.Contains(syncProtected, filepath.Base(name)) {
			stale = append(stale, name)
		}
	}
	slices.Sort(stale)
	return slices.Compact(stale)
}

// removeShipped removes one file under root without following a symlink anywhere on its path,
// then every parent directory the removal left empty.
func removeShipped(root, name string) error {
	parts := strings.Split(name, string(filepath.Separator))
	current := root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("a parent is not a directory")
		}
	}
	if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for dir := filepath.Dir(name); dir != "."; dir = filepath.Dir(dir) {
		if os.Remove(filepath.Join(root, dir)) != nil {
			break
		}
	}
	return nil
}

// deployGit pings the app's deploy hook on the box with tokens.dboss and waits until the pull and
// the restart are done, printing the hook's output.
func (c CLI) deployGit(args []string) error {
	set := flag.NewFlagSet("deploy git", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := configFlag(set)
	app := set.String("app", "", "app name on the box (default: the current folder's app)")
	token := set.String("token", "", "tokens.dboss of the box (default: $DBOSS_TOKEN)")
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return err
	}
	if len(operands) != 1 {
		return errors.New("usage: dboss deploy git <https://management.host> [--app name] [--token t]")
	}
	base, err := url.Parse(strings.TrimRight(operands[0], "/"))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return fmt.Errorf("%q is not an http(s) URL of the box's management host", operands[0])
	}
	if *token == "" {
		*token = os.Getenv("DBOSS_TOKEN")
	}
	if *token == "" {
		return errors.New("pass --token or set DBOSS_TOKEN to the box's tokens.dboss")
	}
	if *app == "" {
		here := &workdir{explicit: *configPath}
		if *app, err = here.app(nil); err != nil {
			return err
		}
	}
	hookURL := base.String() + "/hooks/" + url.PathEscape(*app) + "/" + deployHook

	status, body, err := hookRequest(http.MethodPost, hookURL, *token)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusAccepted:
	case http.StatusNotFound:
		return fmt.Errorf("%s has no %s hook on %s: add `hooks: {deploy: true}` to its dboss.yaml", *app, deployHook, base.Host)
	case http.StatusUnauthorized:
		return fmt.Errorf("%s rejected the token", base.Host)
	default:
		return fmt.Errorf("%s answered %d: %s", base.Host, status, hookErrorText(body))
	}
	fmt.Fprintf(c.Err, "deploying %s on %s\n", *app, base.Host)

	deadline := time.Now().Add(deployWait)
	for {
		time.Sleep(deployPoll)
		status, body, err := hookRequest(http.MethodGet, hookURL, *token)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("%s answered %d: %s", base.Host, status, hookErrorText(body))
		}
		var info supervisor.HookInfo
		if err := json.Unmarshal(body, &info); err != nil {
			return fmt.Errorf("%s: %w", base.Host, err)
		}
		if info.Running || info.Restarting {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s is still deploying after %s; see dboss hooks %s on the box", *app, deployWait, *app)
			}
			continue
		}
		if output := strings.TrimRight(info.Output, "\n"); output != "" {
			fmt.Fprintln(c.Out, output)
		}
		if info.LastExit != 0 || info.LastError != "" {
			fmt.Fprintf(c.Err, "dboss: deploy of %s failed: %s\n", *app, orElse(info.LastError, fmt.Sprintf("exit code %d", info.LastExit)))
			return &exitError{code: max(info.LastExit, 1)}
		}
		fmt.Fprintf(c.Out, "deployed %s: pulled and restarted\n", *app)
		return nil
	}
}

func hookRequest(method, target, token string) (int, []byte, error) {
	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := deployClient.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, body, err
}

// hookErrorText is the error of a JSON answer, else the trimmed body.
func hookErrorText(body []byte) string {
	var answer struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &answer) == nil && answer.Error != "" {
		return answer.Error
	}
	return strings.TrimSpace(string(body))
}

func plural(count int, word string) string {
	if count == 1 {
		return word
	}
	return word + "s"
}

func orElse(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
