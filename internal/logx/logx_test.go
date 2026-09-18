package logx

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestLevelFiltering(t *testing.T) {
	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buffer)
	defer log.SetOutput(previous)

	SetLevel("warn")
	Infof("info line")
	Warnf("warn line")
	Errorf("error line")
	Debugf("debug line")
	output := buffer.String()
	if strings.Contains(output, "info line") || strings.Contains(output, "debug line") {
		t.Fatalf("warn level printed lower levels: %q", output)
	}
	if !strings.Contains(output, "warn line") || !strings.Contains(output, "error line") {
		t.Fatalf("warn level dropped higher levels: %q", output)
	}

	buffer.Reset()
	SetLevel("debug")
	Debugf("debug line")
	if !strings.Contains(buffer.String(), "debug line") {
		t.Fatal("debug level should print debug lines")
	}
	if !Enabled("debug") || !Enabled("warn") {
		t.Fatal("debug level should enable every level")
	}
	SetLevel("error")
	if Enabled("warn") || !Enabled("error") {
		t.Fatal("error level should only enable error")
	}
}
