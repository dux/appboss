package supervisor

import (
	"bytes"
	"strings"
	"testing"
)

// The banner asks for a key before the process exists, and the process writer must then log
// under that same key, or the startup row and the output it belongs to end up in different
// colors.
func TestEchoKeyIsStableAndSharedWithTheWriter(t *testing.T) {
	var out bytes.Buffer
	echo := NewEcho(&out)

	key := echo.Key("sinatra", "web")
	if !strings.Contains(key, "sinatra/web |") {
		t.Fatalf("key = %q", key)
	}
	if again := echo.Key("sinatra", "web"); again != key {
		t.Fatalf("key changed between asks: %q then %q", key, again)
	}

	writer := echo.writer("sinatra", "web")
	if prefix := echo.key(writer.name); prefix != key {
		t.Fatalf("writer prefix %q does not match the banner key %q", prefix, key)
	}

	other := echo.Key("bun", "web")
	if other == key {
		t.Fatal("two processes should not share a color")
	}
}

// Inside an app folder there is only one app, so repeating its name on every line says nothing.
func TestEchoSoloDropsTheAppSegment(t *testing.T) {
	var out bytes.Buffer
	echo := NewEcho(&out)
	echo.Solo()

	key := echo.Key("sinatra", "job")
	if !strings.Contains(key, "job |") || strings.Contains(key, "sinatra") {
		t.Fatalf("key = %q, want a bare job key", key)
	}
	if writer := echo.writer("sinatra", "job"); echo.key(writer.name) != key {
		t.Fatalf("writer prefix %q does not match the key %q", echo.key(writer.name), key)
	}
}

func TestEchoPrintGoesThroughTheSameLock(t *testing.T) {
	var out bytes.Buffer
	echo := NewEcho(&out)
	echo.Print(echo.Key("bun", "web") + "http://bun.lvh.me")
	writer := echo.writer("bun", "web")
	if _, err := writer.Write([]byte("listening\n")); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one banner line and one log line, got %q", out.String())
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, echo.Key("bun", "web")) {
			t.Fatalf("line %q does not start with the process key", line)
		}
	}
}

// The pipe is one column: a short name is padded to the widest one, and a line logged by a process
// named before a wider one still lines up with it.
func TestEchoPadsEveryNameToTheWidest(t *testing.T) {
	var out bytes.Buffer
	echo := NewEcho(&out)
	writer := echo.writer("bun", "web")
	wide := echo.Key("dboss", "console")
	if _, err := writer.Write([]byte("listening\n")); err != nil {
		t.Fatal(err)
	}
	short := echo.Key("bun", "web")
	if strings.Index(short, "|") != strings.Index(wide, "|") {
		t.Fatalf("pipes differ: %q vs %q", short, wide)
	}
	if !strings.HasPrefix(out.String(), short) {
		t.Fatalf("log line %q is not padded like %q", out.String(), short)
	}
}
