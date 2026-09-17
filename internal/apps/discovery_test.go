package apps

import (
	"os"
	"testing"
)

func TestParseProcfile(t *testing.T) {
	commands, err := ParseProcfile(map[string]string{"web": "./server --port x", "worker": "./jobs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands["web"].Argv[0] != "./server" {
		t.Fatalf("unexpected commands: %#v", commands)
	}
}

func TestLoadEnv(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/env"
	writeTestFile(t, path, "A=one\nexport B=two\nC=\"three words\"\nD='${A}'\n")
	values, err := LoadEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["C"] != "three words" || values["D"] != "${A}" {
		t.Fatalf("unexpected env: %#v", values)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
