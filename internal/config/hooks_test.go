package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAppReadsHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	app, err := ParseApp([]byte(`procfile:
  web: ./server
hooks:
  deploy:
    command: ./deploy.sh
    timeout: 10m
    restart: true
    secret: s3cret
  notify:
    command: ./notify.sh
    disabled: true
`), path, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Hooks) != 2 {
		t.Fatalf("hooks = %+v", app.Hooks)
	}
	deploy := app.Hooks["deploy"]
	if deploy.Command != "./deploy.sh" || deploy.Timeout.Value() != 10*time.Minute || !deploy.Restart || deploy.Secret != "s3cret" {
		t.Fatalf("deploy = %+v", deploy)
	}
	if notify := app.Hooks["notify"]; !notify.Disabled {
		t.Fatalf("notify = %+v", notify)
	}
}

func TestHookPullShorthand(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	app, err := ParseApp([]byte("procfile:\n  web: ./server\nhooks:\n  deploy: true\n"), path, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if deploy := app.Hooks["deploy"]; deploy.Command != pullCommand || !deploy.Restart || !deploy.Pull {
		t.Fatalf("deploy = %+v", deploy)
	}
	if _, err := ParseApp([]byte("procfile:\n  web: ./server\nhooks:\n  deploy: false\n"), path, Default().Defaults); err == nil {
		t.Fatal("scalar false was accepted")
	}
}

func TestGithubTokenDefaultsAndOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	defaults := Default().Defaults
	defaults.GithubToken = "host-token"
	app, err := ParseApp([]byte("procfile:\n  web: ./server\n"), path, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.GithubToken != "host-token" {
		t.Fatalf("inherited token = %q", app.GithubToken)
	}
	app, err = ParseApp([]byte("procfile:\n  web: ./server\ngithub_token: app-token\n"), path, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if app.GithubToken != "app-token" {
		t.Fatalf("override token = %q", app.GithubToken)
	}
}

func TestHostGithubPRHook(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	base := "apps: ./apps\n"
	valid := base + `hooks:
  github_pr:
    repo: https://github.com/example/repo.git
    template:
      name: $QS_BRANCH
      hosts: [pr-$QS_BRANCH.example.com]
      procfile:
        web: ./start.sh
`
	cfg, err := Parse([]byte(valid), path)
	if err != nil {
		t.Fatal(err)
	}
	hook := cfg.HostHooks["github_pr"]
	if hook.Repo == "" || hook.Template == nil {
		t.Fatalf("github_pr = %+v", hook)
	}
	for name, body := range map[string]string{
		"unknown hook": "hooks:\n  deploy:\n    command: ./run\n",
		"command set":  "hooks:\n  github_pr:\n    command: ./run\n    template:\n      name: x\n",
		"no template":  "hooks:\n  github_pr:\n    repo: x\n",
		"no name":      "hooks:\n  github_pr:\n    template:\n      procfile:\n        web: ./start\n",
		"setup":        "hooks:\n  github_pr:\n    setup: {command: ./x}\n    template:\n      name: x\n",
	} {
		if _, err := Parse([]byte(base+body), path); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestHookValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	base := "procfile:\n  web: ./server\nhooks:\n  deploy:\n"
	for name, body := range map[string]string{
		"empty command":  "    command: \"  \"\n",
		"negative time":  "    command: ./run\n    timeout: -1m\n",
		"unknown option": "    command: ./run\n    retries: 3\n",
	} {
		if _, err := ParseApp([]byte(base+body), path, Default().Defaults); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := ParseApp([]byte("procfile:\n  web: ./server\nhooks:\n  Bad Name:\n    command: ./run\n"), path, Default().Defaults); err == nil || !strings.Contains(err.Error(), "invalid hook name") {
		t.Fatalf("bad hook name err = %v", err)
	}
	if _, err := ParseApp([]byte(base+"    command: ./run\n"), path, Default().Defaults); err != nil {
		t.Fatalf("valid hook rejected: %v", err)
	}
}

func TestLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	base := "procfile:\n  web: ./server\n"
	app, err := ParseApp([]byte(base+"lifecycle:\n  create: bin/setup\n  start: {command: bin/migrate, timeout: 10m}\n  destroy: bin/cleanup\n"), path, Default().Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if got := app.Lifecycle["create"]; got.Command != "bin/setup" || got.Timeout != 0 {
		t.Errorf("create = %+v", got)
	}
	if got := app.Lifecycle["start"]; got.Command != "bin/migrate" || got.Timeout.Value() != 10*time.Minute {
		t.Errorf("start = %+v", got)
	}
	for name, body := range map[string]string{
		"unknown step":  "lifecycle:\n  deploy: ./x\n",
		"empty command": "lifecycle:\n  start: {timeout: 1m}\n",
		"unknown key":   "lifecycle:\n  start: {command: ./x, restart: true}\n",
		"negative":      "lifecycle:\n  start: {command: ./x, timeout: -1s}\n",
	} {
		if _, err := ParseApp([]byte(base+body), path, Default().Defaults); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
