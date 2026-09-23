// Package git runs the few git commands dboss needs, authenticated with a GitHub token that
// never reaches argv or a repository's config.
package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// AuthEnv points git at tokens.github through a credential helper carried in the environment, so
// the token reaches neither argv nor the repository's config. The helper answers only the
// credential "get"; GIT_TERMINAL_PROMPT=0 makes a bad token fail instead of hanging.
func AuthEnv(env map[string]string, token string) {
	env["GITHUB_TOKEN"] = token
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GIT_CONFIG_COUNT"] = "1"
	env["GIT_CONFIG_KEY_0"] = "credential.helper"
	env["GIT_CONFIG_VALUE_0"] = `!f() { if [ "$1" = get ]; then printf 'username=x-access-token\npassword=%s\n' "$GITHUB_TOKEN"; fi; }; f`
}

// Checkout clones branch into dir on first use and resets dir to it afterwards, so a repeat call
// lands on the same checkout. The remote is re-pointed every time, since a fork PR changes it. An
// empty token runs git unauthenticated.
func Checkout(dir, repo, branch, token string) error {
	env := os.Environ()
	if token != "" {
		auth := map[string]string{}
		AuthEnv(auth, token)
		for key, value := range auth {
			env = append(env, key+"="+value)
		}
	}
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Env = env
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
		}
		return nil
	}
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
