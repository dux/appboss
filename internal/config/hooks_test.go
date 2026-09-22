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
