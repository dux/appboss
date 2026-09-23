package supervisor

import (
	"os"
	"strings"
	"testing"

	"dboss/internal/apps"
)

func TestNewCommandRunsConfigLinesThroughTheShell(t *testing.T) {
	env := map[string]string{"PATH": os.Getenv("PATH"), "NAME": "dboss"}
	cmd, err := newCommand(t.TempDir(), apps.Command{Line: `printf '%s\n' "a b" && echo $NAME`}, env)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil || string(out) != "a b\ndboss\n" {
		t.Fatalf("output = %q, err = %v", out, err)
	}
}

func TestNewCommandExecsArgvDirectly(t *testing.T) {
	cmd, err := newCommand(t.TempDir(), apps.Command{Argv: []string{"echo", "a && b"}}, map[string]string{"PATH": os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "a && b" {
		t.Fatalf("output = %q, err = %v", out, err)
	}
}
