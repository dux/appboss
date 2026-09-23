package console

import (
	"testing"

	"dboss/internal/apps"
	"dboss/internal/config"
)

// A broken host file met while checking an app file must not mark a line in the app editor.
func TestYAMLLineSkipsHostFileErrors(t *testing.T) {
	err := &config.Error{Path: "/srv/dboss.local.yaml", Line: 3, Key: "management.url", Message: "was removed"}
	if line := yamlLine(err); line != 3 {
		t.Fatalf("own file error line = %d, want 3", line)
	}
	if line := yamlLine(&apps.HostFileError{Err: err}); line != 0 {
		t.Fatalf("host file error line = %d, want 0", line)
	}
}
