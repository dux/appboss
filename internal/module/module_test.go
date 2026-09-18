package module

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeModule struct {
	name      string
	started   *[]string
	closed    *[]string
	failStart bool
}

func (f fakeModule) Name() string { return f.name }

func (f fakeModule) Start(context.Context) error {
	if f.failStart {
		return errors.New("boom")
	}
	if f.started != nil {
		*f.started = append(*f.started, f.name)
	}
	return nil
}

func (f fakeModule) Close() error {
	if f.closed != nil {
		*f.closed = append(*f.closed, f.name)
	}
	return nil
}

func TestManagerStartsInOrderAndClosesInReverse(t *testing.T) {
	var started, closed []string
	manager := NewManager(
		fakeModule{name: "a", started: &started, closed: &closed},
		fakeModule{name: "b", started: &started, closed: &closed},
	)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(started, ",") != "a,b" || strings.Join(closed, ",") != "b,a" {
		t.Fatalf("started=%v closed=%v", started, closed)
	}
}

func TestManagerClosesStartedModulesOnFailure(t *testing.T) {
	var closed []string
	manager := NewManager(
		fakeModule{name: "a", closed: &closed},
		fakeModule{name: "bad", closed: &closed, failStart: true},
	)
	if err := manager.Start(context.Background()); err == nil {
		t.Fatal("expected the failing module to abort Start")
	}
	if strings.Join(closed, ",") != "a" {
		t.Fatalf("closed=%v, want only the started module", closed)
	}
}
