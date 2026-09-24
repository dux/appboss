package events

import (
	"os"
	"strings"
	"testing"
	"time"
)

type staticApps []AppConfig

func (s staticApps) EventApps() []AppConfig { return s }

func TestMaintainCompactsPrunesAndWritesViews(t *testing.T) {
	store := NewStore(t.TempDir())
	saved := NewSavedStore(t.TempDir())
	old := now.Add(-100 * 24 * time.Hour)
	store.Append("shop", "web", []Row{event(1, now.Add(-24*time.Hour), "a", "t"), event(2, old, "a", "t")})
	// An app no longer in the config falls under the default retention.
	store.Append("gone", "web", []Row{event(3, old, "a", "t")})
	if err := saved.SaveView("shop", View{Name: "mine", Filter: "a"}); err != nil {
		t.Fatal(err)
	}
	apps := staticApps{{Name: "shop", Retention: 365 * 24 * time.Hour, Funnels: []Funnel{{Name: "f", Steps: []Step{{Filter: "a"}, {Filter: "b"}}}}}}
	module := NewModule(store, saved, apps, "", 30*24*time.Hour)
	module.Maintain(now)

	partitions, _ := store.Partitions("shop")
	if len(partitions) != 2 || !partitions[0].Compact() || !partitions[1].Compact() {
		t.Fatalf("shop partitions = %+v", partitions)
	}
	if gone, _ := store.Partitions("gone"); len(gone) != 0 {
		t.Fatalf("removed app kept %+v", gone)
	}
	views, err := os.ReadFile(store.ViewsFile("shop"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CREATE OR REPLACE VIEW events", "VIEW mine AS", "VIEW funnel_f AS", "read_parquet("} {
		if !strings.Contains(string(views), want) {
			t.Errorf("views.sql misses %q", want)
		}
	}
}
