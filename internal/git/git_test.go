package git

import (
	"strings"
	"testing"
)

func TestAuthEnvCarriesTokenInEnvironment(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin"}
	AuthEnv(env, "s3cret")
	if env["GITHUB_TOKEN"] != "s3cret" || env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("env = %v", env)
	}
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "credential.helper" {
		t.Fatalf("git config env = %v", env)
	}
	if strings.Contains(env["GIT_CONFIG_VALUE_0"], "s3cret") {
		t.Fatalf("token leaked into the helper: %q", env["GIT_CONFIG_VALUE_0"])
	}
}
