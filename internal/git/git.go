// Package git runs the few git commands dboss needs, authenticated with a GitHub token that
// never reaches argv or a repository's config.
package git

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// AuthEnv points git at tokens.github through a credential helper carried in the environment, so
// the token reaches neither argv nor the repository's config. The helper is scoped to
// https://github.com, so a remote on another host is never offered the token, and answers only the
// credential "get"; GIT_TERMINAL_PROMPT=0 makes a bad token fail instead of hanging.
func AuthEnv(env map[string]string, token string) {
	env["GITHUB_TOKEN"] = token
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GIT_CONFIG_COUNT"] = "1"
	env["GIT_CONFIG_KEY_0"] = "credential.https://github.com.helper"
	env["GIT_CONFIG_VALUE_0"] = `!f() { if [ "$1" = get ]; then printf 'username=x-access-token\npassword=%s\n' "$GITHUB_TOKEN"; fi; }; f`
}

// runner returns a git invocation with the token helper in its environment. An ssh remote never
// prompts: an unknown host key is accepted once and a missing key fails instead of asking.
func runner(token string) func(args ...string) error {
	env := os.Environ()
	if token != "" {
		auth := map[string]string{}
		AuthEnv(auth, token)
		for key, value := range auth {
			env = append(env, key+"="+value)
		}
	}
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new")
	}
	return func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Env = env
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
		}
		return nil
	}
}

// Checkout clones branch into dir on first use and resets dir to it afterwards, so a repeat call
// lands on the same checkout. The remote is re-pointed every time, since a fork PR changes it. An
// empty token runs git unauthenticated.
func Checkout(dir, repo, branch, token string) error {
	run := runner(token)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if err := run("-C", dir, "remote", "set-url", "origin", repo); err != nil {
			return err
		}
		if err := run("-C", dir, "fetch", "--prune", "origin"); err != nil {
			return err
		}
		return run("-C", dir, "reset", "--hard", "origin/"+branch)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return run("clone", "--branch", branch, repo, dir)
}

// Clone clones repo into dir, which must not exist yet. An empty branch takes the remote's
// default branch.
func Clone(dir, repo, branch, token string) error {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	}
	args := []string{"clone"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	return runner(token)(append(args, "--", repo, dir)...)
}

var (
	// scpRemote is git's user@host:path form, e.g. git@github.com:owner/repo.git.
	scpRemote   = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^/\s][^\s]*$`)
	nameInvalid = regexp.MustCompile(`[^a-z0-9_-]+`)
)

// NormalizeRepo turns what an operator pastes into a clone URL and a suggested app name. https,
// http and ssh URLs and the user@host:path form are kept; host/owner/repo gets https:// and
// owner/repo is a GitHub repository. Local paths and other schemes are refused, so the input can
// never be read by git as a flag or a file on the box.
func NormalizeRepo(input string) (repo, name string, err error) {
	repo = strings.TrimSpace(input)
	switch {
	case repo == "" || strings.ContainsAny(repo, " \t\r\n") || strings.HasPrefix(repo, "-"):
		return "", "", fmt.Errorf("invalid git URL %q", input)
	case strings.HasPrefix(repo, "https://"), strings.HasPrefix(repo, "http://"), strings.HasPrefix(repo, "ssh://"):
		parsed, perr := url.Parse(repo)
		if perr != nil || parsed.Host == "" || strings.Trim(parsed.Path, "/") == "" {
			return "", "", fmt.Errorf("invalid git URL %q", input)
		}
	case scpRemote.MatchString(repo):
	case strings.Contains(repo, "://"):
		return "", "", fmt.Errorf("unsupported git URL %q: use https://, ssh:// or git@host:path", input)
	case strings.HasPrefix(repo, "/"), strings.HasPrefix(repo, "."), strings.HasPrefix(repo, "~"):
		return "", "", fmt.Errorf("a local path is not a git URL: %q", input)
	default:
		parts := strings.Split(strings.Trim(repo, "/"), "/")
		switch {
		case len(parts) == 2 && !strings.Contains(parts[0], ".") && parts[0] != "" && parts[1] != "":
			repo = "https://github.com/" + parts[0] + "/" + parts[1]
			if !strings.HasSuffix(repo, ".git") {
				repo += ".git"
			}
		case len(parts) >= 3 && strings.Contains(parts[0], "."):
			repo = "https://" + strings.Trim(repo, "/")
		default:
			return "", "", fmt.Errorf("invalid git URL %q: use https://host/owner/repo, git@host:owner/repo or owner/repo", input)
		}
	}
	name, err = RepoName(repo)
	return repo, name, err
}

// RepoName suggests an app name from a clone URL: its last path segment without .git, lowercased,
// with every run of other characters turned into a dash.
func RepoName(repo string) (string, error) {
	trimmed := strings.TrimRight(repo, "/")
	base := strings.TrimSuffix(trimmed[strings.LastIndexAny(trimmed, "/:")+1:], ".git")
	name := strings.Trim(nameInvalid.ReplaceAllString(strings.ToLower(base), "-"), "-_")
	name = strings.TrimLeft(name, "0123456789-_")
	if name == "" {
		return "", errors.New("cannot derive an app name from " + repo)
	}
	return name, nil
}
