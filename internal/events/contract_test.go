package events

import (
	"slices"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func TestParsePullsFixedKeysOutOfData(t *testing.T) {
	line := `{"msg":"Checkout completed","tags":["plan:pro","beta"," plan : team ","beta"],
		"data":{"event":"checkout_completed","ts":"2026-09-24T10:00:00.123Z","user_id":42,"anon_id":"a_9",
		"tenant_id":"s_7","request_id":"abc","value":49.9,"coupon":"X","items":3}}`
	row, err := Parse([]byte(line), now)
	if err != nil {
		t.Fatal(err)
	}
	if row.Event != "checkout_completed" || row.UserID != "42" || row.AnonID != "a_9" || row.TenantID != "s_7" || row.RequestID != "abc" {
		t.Fatalf("fixed columns = %+v", row)
	}
	if row.Value == nil || *row.Value != 49.9 {
		t.Fatalf("value = %v", row.Value)
	}
	if !row.TS.Equal(time.Date(2026, 9, 24, 10, 0, 0, 123e6, time.UTC)) {
		t.Fatalf("ts = %v", row.TS)
	}
	if row.Msg != "Checkout completed" {
		t.Fatalf("msg = %q", row.Msg)
	}
	if want := []string{"beta", "plan:pro", "plan:team"}; !slices.Equal(row.Tags, want) {
		t.Fatalf("tags = %v, want %v", row.Tags, want)
	}
	if row.Data != `{"coupon":"X","items":3}` {
		t.Fatalf("data = %s", row.Data)
	}
}

func TestParseTopLevelKeys(t *testing.T) {
	row, err := Parse([]byte(`{"event":"signup","ts":1727172000000,"source":"ad","data":{"source":"inner"}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if row.Event != "signup" || row.TS.UnixMilli() != 1727172000000 {
		t.Fatalf("row = %+v", row)
	}
	// data wins over a top-level key of the same name.
	if row.Data != `{"source":"inner"}` {
		t.Fatalf("data = %s", row.Data)
	}
}

func TestParseDefaultsAndLimits(t *testing.T) {
	long := strings.Repeat("ž", MaxMsg+10)
	row, err := Parse([]byte(`{"msg":"`+long+`","data":{"event":"x","value":0}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if !row.TS.Equal(now) {
		t.Fatalf("missing ts = %v, want now", row.TS)
	}
	if got := len([]rune(row.Msg)); got != MaxMsg {
		t.Fatalf("msg runes = %d", got)
	}
	if row.Value == nil || *row.Value != 0 {
		t.Fatalf("a zero value must stay a value, got %v", row.Value)
	}
	if row.Data != "" {
		t.Fatalf("data = %q, want empty", row.Data)
	}
}

func TestParseRejects(t *testing.T) {
	for name, line := range map[string]string{
		"not json":      `hello`,
		"array":         `[1,2]`,
		"no event":      `{"msg":"x","data":{"a":1}}`,
		"empty event":   `{"event":"  "}`,
		"bad ts":        `{"event":"x","ts":"yesterday"}`,
		"bad value":     `{"event":"x","value":"12"}`,
		"bad tags":      `{"event":"x","tags":"a"}`,
		"bad data":      `{"event":"x","data":[1]}`,
		"object id":     `{"event":"x","user_id":{"a":1}}`,
		"trailing text": `{"event":"x"} extra`,
	} {
		if _, err := Parse([]byte(line), now); err == nil {
			t.Errorf("%s: %s parsed without error", name, line)
		}
	}
}

func TestNormalizeTagsCapsCount(t *testing.T) {
	var tags []string
	for i := range MaxTags + 5 {
		tags = append(tags, "t"+strings.Repeat("x", i))
	}
	if got := NormalizeTags(tags); len(got) != MaxTags {
		t.Fatalf("len = %d", len(got))
	}
}

func TestNamespace(t *testing.T) {
	for rel, want := range map[string]string{
		"checkout.json.log":        "checkout",
		"billing/invoice.json.log": "billing.invoice",
		"a-b_c.json.log":           "a-b_c",
	} {
		if got, err := Namespace(rel); err != nil || got != want {
			t.Errorf("Namespace(%q) = %q, %v; want %q", rel, got, err, want)
		}
	}
	for _, rel := range []string{"Checkout.json.log", "a b.json.log", "x.log", ".json.log"} {
		if _, err := Namespace(rel); err == nil {
			t.Errorf("Namespace(%q) accepted", rel)
		}
	}
}

func TestSplitTag(t *testing.T) {
	if k, v := SplitTag("plan:pro"); k != "plan" || v != "pro" {
		t.Fatal(k, v)
	}
	if k, v := SplitTag("url:http://x"); k != "url" || v != "http://x" {
		t.Fatal(k, v)
	}
	if k, v := SplitTag("beta"); k != "#" || v != "beta" {
		t.Fatal(k, v)
	}
}
