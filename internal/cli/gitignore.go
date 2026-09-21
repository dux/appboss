package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"dboss/internal/config"
)

// warnUnignoredRuntime tells a developer when the runtime folder dboss is about to write, which
// is `.dboss` unless the config moves it, would be committed. It is a hand-run convenience only:
// a server has no checkout, and a repository that keeps no .gitignore is not asking to be
// advised. Anything unexpected (no git, no repository, a path outside the checkout) stays quiet,
// so this can never get between the operator and a start.
func warnUnignoredRuntime(out io.Writer, cfg config.Config) {
	if _, err := os.Stat(filepath.Join(cfg.Dir, ".gitignore")); err != nil {
		return
	}
	paths := runtimeRoots(cfg)
	if len(paths) == 0 {
		return
	}
	unignored := make([]string, 0, len(paths))
	for _, path := range paths {
		ignored, err := gitIgnores(cfg.Dir, path)
		if err != nil {
			return
		}
		if !ignored {
			unignored = append(unignored, path)
		}
	}
	if len(unignored) == 0 {
		return
	}
	fmt.Fprintf(out, "dboss: %s holds this host's state, logs and secrets and is not gitignored\n", strings.Join(unignored, ", "))
	fmt.Fprintf(out, "       add it: echo %s >> %s\n", unignored[0]+"/", filepath.Join(cfg.Dir, ".gitignore"))
}

// runtimeRoots is the set of top-level entries dboss creates inside the config directory, so a
// state_dir and a log_dir under the same `.dboss` are reported once. A path configured outside
// the directory is not in the checkout and is left alone.
func runtimeRoots(cfg config.Config) []string {
	seen := map[string]bool{}
	for _, path := range []string{cfg.StateDir, cfg.LogDir, filepath.Dir(cfg.Socket)} {
		relative, err := filepath.Rel(cfg.Dir, path)
		if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
			continue
		}
		seen[strings.Split(relative, string(filepath.Separator))[0]] = true
	}
	roots := make([]string, 0, len(seen))
	for root := range seen {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots
}

// gitIgnores asks git itself rather than parsing .gitignore, so a rule in a parent directory,
// in .git/info/exclude or in the user's global excludes counts too. An error means the question
// could not be answered here, which is not the same as a no.
//
// The path is asked for with a trailing slash. On a first start the folder does not exist yet,
// and git reads a bare name as a file, which a `.dboss/` rule deliberately does not match; the
// slash says directory and every rule style then matches.
func gitIgnores(dir, path string) (bool, error) {
	command := exec.Command("git", "check-ignore", "-q", "--", path+"/")
	command.Dir = dir
	err := command.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}
