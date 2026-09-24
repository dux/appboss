package ops

import (
	"encoding/json"
	"testing"
	"time"

	"dboss/internal/events"
)

type eventApps []events.AppConfig

func (e eventApps) EventApps() []events.AppConfig { return e }

func TestEventsSaveAndDeleteAreAudited(t *testing.T) {
	store := &auditStore{}
	service := New(&fakeRuntime{}, store, nil, nil, nil, nil)
	apps := eventApps{{Name: "shop", Retention: time.Hour, Views: []events.View{{Name: "paid", Filter: "checkout value>0"}}}}
	service.SetEvents(events.NewService(events.NewStore(t.TempDir()), events.NewSavedStore(t.TempDir()), apps))

	entry, _ := json.Marshal(events.View{Name: "pro", Filter: "plan:pro"})
	if _, err := service.Do(Request{Method: ActionEventsSave, App: "shop", Kind: "view", Data: entry, Actor: "ana@example.com"}); err != nil {
		t.Fatal(err)
	}
	// A console view that a dboss.yaml view hides is reported, not listed twice.
	shadow, _ := json.Marshal(events.View{Name: "paid", Filter: "checkout"})
	if _, err := service.Do(Request{Method: ActionEventsSave, App: "shop", Kind: "view", Data: shadow, Actor: "ana@example.com"}); err != nil {
		t.Fatal(err)
	}
	result, err := service.EventViews("shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Views) != 2 || result.Views[0].Source != events.SourceYAML || result.Views[1].Name != "pro" || len(result.Shadowed) != 1 {
		t.Fatalf("catalog = %+v", result.Catalog)
	}
	if _, err := service.Do(Request{Method: ActionEventsDelete, App: "shop", Kind: "view", Name: "pro", Actor: "ana@example.com"}); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 3 || store.rows[0].Action != ActionEventsSave || store.rows[0].Detail != "view pro" || store.rows[2].Detail != "view pro" {
		t.Fatalf("audit = %+v", store.rows)
	}
	// Reads are not audited; a bad filter is an error.
	if _, err := service.Do(Request{Method: ActionEvents, App: "shop", Query: "checkout"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Do(Request{Method: ActionEvents, App: "shop", Query: "a b"}); err == nil {
		t.Fatal("two event names must fail")
	}
	if len(store.rows) != 3 {
		t.Fatalf("read was audited: %+v", store.rows)
	}
	if _, err := service.Do(Request{Method: ActionEvents, App: "nope"}); err == nil {
		t.Fatal("unknown app must fail")
	}
}
