package apps

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitSource(t *testing.T) {
	checkout := t.TempDir()
	gitDir := filepath.Join(checkout, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "worktrees", "wt"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/feature/x\n")
	writeTestFile(t, filepath.Join(gitDir, "config"), "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = git@github.com:other/repo.git\n[remote \"origin\"]\n\turl = git@github.com:dux/dboss.git\n")

	// a linked worktree: its own HEAD, the shared config through commondir
	worktree := t.TempDir()
	wtGitDir := filepath.Join(gitDir, "worktrees", "wt")
	writeTestFile(t, filepath.Join(worktree, ".git"), "gitdir: "+wtGitDir+"\n")
	writeTestFile(t, filepath.Join(wtGitDir, "HEAD"), "ref: refs/heads/main\n")
	writeTestFile(t, filepath.Join(wtGitDir, "commondir"), "../..\n")

	detached := t.TempDir()
	if err := os.MkdirAll(filepath.Join(detached, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(detached, ".git", "HEAD"), "4f2a9c0d\n")

	release := map[string]string{"GIT_BRANCH": "master", "GIT_REPO": "https://github.com/dux/authcog.git"}
	for _, tc := range []struct {
		name, dir         string
		env               map[string]string
		branch, branchURL string
	}{
		{"checkout wins over env", checkout, release, "feature/x", "https://github.com/dux/dboss/tree/feature/x"},
		{"worktree", worktree, nil, "main", "https://github.com/dux/dboss/tree/main"},
		{"detached head", detached, nil, "", ""},
		{"packed release reads env", t.TempDir(), release, "master", "https://github.com/dux/authcog/tree/master"},
		{"branch without a repo", t.TempDir(), map[string]string{"GIT_BRANCH": "main"}, "main", ""},
		{"nothing known", t.TempDir(), nil, "", ""},
	} {
		branch, branchURL := gitSource(tc.dir, tc.env)
		if branch != tc.branch || branchURL != tc.branchURL {
			t.Errorf("%s: got %q %q, want %q %q", tc.name, branch, branchURL, tc.branch, tc.branchURL)
		}
	}
}

func TestRepoWebURL(t *testing.T) {
	for remote, want := range map[string]string{
		"git@github.com:dux/dboss.git":                   "https://github.com/dux/dboss",
		"ssh://git@gitlab.example.com:2222/team/app.git": "https://gitlab.example.com/team/app",
		"https://user:token@github.com/dux/dboss.git":    "https://github.com/dux/dboss",
		"https://github.com/dux/dboss/":                  "https://github.com/dux/dboss",
		"/srv/git/app.git":                               "",
		"file:///srv/git/app.git":                        "",
		"":                                               "",
	} {
		if got := repoWebURL(remote); got != want {
			t.Errorf("repoWebURL(%q) = %q, want %q", remote, got, want)
		}
	}
}
