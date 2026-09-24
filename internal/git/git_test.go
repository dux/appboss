package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthEnvCarriesTokenInEnvironment(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin"}
	AuthEnv(env, "s3cret")
	if env["GITHUB_TOKEN"] != "s3cret" || env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("env = %v", env)
	}
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "credential.https://github.com.helper" {
		t.Fatalf("git config env = %v", env)
	}
	if strings.Contains(env["GIT_CONFIG_VALUE_0"], "s3cret") {
		t.Fatalf("token leaked into the helper: %q", env["GIT_CONFIG_VALUE_0"])
	}
}

func TestNormalizeRepo(t *testing.T) {
	cases := []struct{ input, repo, name string }{
		{"https://github.com/acme/Shop.git", "https://github.com/acme/Shop.git", "shop"},
		{" https://gitlab.com/acme/group/my.site/ ", "https://gitlab.com/acme/group/my.site/", "my-site"},
		{"ssh://git@example.com/acme/api.git", "ssh://git@example.com/acme/api.git", "api"},
		{"git@github.com:acme/billing.git", "git@github.com:acme/billing.git", "billing"},
		{"github.com/acme/blog", "https://github.com/acme/blog", "blog"},
		{"acme/blog", "https://github.com/acme/blog.git", "blog"},
		{"acme/2fa_app", "https://github.com/acme/2fa_app.git", "fa_app"},
	}
	for _, tc := range cases {
		repo, name, err := NormalizeRepo(tc.input)
		if err != nil || repo != tc.repo || name != tc.name {
			t.Errorf("NormalizeRepo(%q) = %q, %q, %v; want %q, %q", tc.input, repo, name, err, tc.repo, tc.name)
		}
	}
}

func TestNormalizeRepoRejects(t *testing.T) {
	for _, input := range []string{"", "-uhack", "/srv/app", "./app", "~/app", "file:///srv/app", "ext::sh -c x", "acme", "a b/c", "https://github.com/", "ftp://host/x"} {
		if repo, _, err := NormalizeRepo(input); err == nil {
			t.Errorf("NormalizeRepo(%q) = %q, want an error", input, repo)
		}
	}
}

func TestCloneDefaultBranchAndRefusesExisting(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	for _, args := range [][]string{
		{"init", "-q", "-b", "trunk", source},
		{"-C", source, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	target := filepath.Join(root, "apps", "shop")
	if err := Clone(target, source, "", ""); err != nil {
		t.Fatal(err)
	}
	head, err := os.ReadFile(filepath.Join(target, ".git", "HEAD"))
	if err != nil || !strings.Contains(string(head), "trunk") {
		t.Fatalf("HEAD = %q, %v", head, err)
	}
	if err := Clone(target, source, "", ""); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second clone err = %v", err)
	}
}
