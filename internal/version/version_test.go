package version

import "testing"

func TestFormatRendersDotted(t *testing.T) {
	for raw, want := range map[string]string{
		"v123":   "v1.2.3",
		"v1123":  "v11.2.3",
		"v81":    "v0.8.1",
		"v5":     "v0.0.5",
		"v100":   "v1.0.0",
		"v1234":  "v12.3.4",
		"dev":    "dev",
		"":       "",
		"v1.2.3": "v1.2.3",
	} {
		if got := Format(raw); got != want {
			t.Errorf("Format(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestStringUsesVersion(t *testing.T) {
	previous := Version
	defer func() { Version = previous }()
	Version = "v1123"
	if got := String(); got != "v11.2.3" {
		t.Fatalf("String() = %q, want v11.2.3", got)
	}
}
