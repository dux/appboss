package ingest

import (
	"strings"
	"testing"

	"dboss/internal/logstore"
)

func group(lines ...string) []logstore.LogEntry {
	g := &grouper{source: "file", name: "test.log"}
	for _, line := range lines {
		g.add(line)
	}
	g.flush()
	return g.entries
}

func TestGrouperFoldsIndentedLines(t *testing.T) {
	entries := group(
		"Traceback (most recent call last):",
		`  File "app.py", line 3, in <module>`,
		"\tboom()",
		"ValueError: bad",
	)
	if len(entries) != 2 {
		t.Fatalf("want 2 rows, got %+v", entries)
	}
	want := "Traceback (most recent call last):\n  File \"app.py\", line 3, in <module>\n\tboom()"
	if entries[0].Message != want || entries[0].Raw != want || entries[1].Message != "ValueError: bad" {
		t.Fatalf("unexpected rows: %+v", entries)
	}
}

func TestGrouperLevelComesFromTheFirstLine(t *testing.T) {
	entries := group("Processing by HomeController#index as HTML", `  SELECT * FROM "errors"`)
	if len(entries) != 1 || entries[0].Level != "info" {
		t.Fatalf("detail lines must not set the level: %+v", entries)
	}
}

func TestGrouperOrphanContinuationIsItsOwnRow(t *testing.T) {
	entries := group("  tail of a cut row", "  more", "next")
	if len(entries) != 2 || entries[0].Message != "  tail of a cut row\n  more" || entries[1].Message != "next" {
		t.Fatalf("unexpected rows: %+v", entries)
	}
}

func TestGrouperDropsBlankLines(t *testing.T) {
	if entries := group("", "   ", "\x1b[0m"); len(entries) != 0 {
		t.Fatalf("blank input must give no rows: %+v", entries)
	}
	entries := group("head", "", "  detail", "", "", "next", "")
	if len(entries) != 2 || entries[0].Message != "head\n\n  detail" || entries[1].Message != "next" {
		t.Fatalf("unexpected rows: %+v", entries)
	}
}

func TestGrouperRailsRequestIsOneRow(t *testing.T) {
	entries := group(
		"",
		"",
		`Started GET "/posts" for 127.0.0.1 at 2026-09-20 10:00:00 +0200`,
		"Processing by PostsController#index as HTML",
		`  Parameters: {"page"=>"2"}`,
		"  \x1b[1m\x1b[36mPost Load (0.4ms)\x1b[0m  \x1b[1m\x1b[34mSELECT \"posts\".* FROM \"posts\"\x1b[0m",
		"  Rendered posts/index.html.erb (Duration: 1.2ms)",
		"Completed 200 OK in 12ms (Views: 8.1ms | ActiveRecord: 0.4ms)",
		"",
		"",
		`Started POST "/posts" for 127.0.0.1 at 2026-09-20 10:00:01 +0200`,
		"Processing by PostsController#create as HTML",
		"Completed 500 Internal Server Error in 3ms",
		"worker heartbeat",
	)
	if len(entries) != 3 {
		t.Fatalf("want 3 rows, got %d: %+v", len(entries), entries)
	}
	first := entries[0]
	if !strings.HasPrefix(first.Message, "Started GET") || !strings.HasSuffix(first.Message, "ActiveRecord: 0.4ms)") || strings.Count(first.Message, "\n") != 5 {
		t.Fatalf("request is not one row: %q", first.Message)
	}
	if strings.Contains(first.Message, "\x1b") || !strings.Contains(first.Message, `  Post Load (0.4ms)  SELECT "posts".*`) {
		t.Fatalf("colors must be stripped from the message: %q", first.Message)
	}
	if !strings.Contains(first.Raw, "\x1b[1m") {
		t.Fatalf("raw must keep the line as written: %q", first.Raw)
	}
	if first.Level != "info" || entries[1].Level != "error" || entries[2].Message != "worker heartbeat" {
		t.Fatalf("unexpected levels or rows: %+v", entries)
	}
}

func TestGrouperUnfinishedRailsRequestEndsAtTheNextOne(t *testing.T) {
	entries := group(
		`Started GET "/nope" for 127.0.0.1`,
		`ActionController::RoutingError (No route matches [GET] "/nope"):`,
		`Started GET "/" for 127.0.0.1`,
		"Completed 404 Not Found in 1ms",
	)
	if len(entries) != 2 || strings.Count(entries[0].Message, "\n") != 1 || entries[1].Level != "warn" {
		t.Fatalf("unexpected rows: %+v", entries)
	}
}

func TestGrouperKeepsInterleavedTaggedRequestsApart(t *testing.T) {
	a, b := "[8a1b2c3d4e5f6789-FRA] ", "[0f9e8d7c6b5a4321-FRA] "
	entries := group(
		a+`Started GET "/a" for 127.0.0.1`,
		a+"Processing by AController#show as HTML",
		b+`Started GET "/b" for 127.0.0.1`,
		a+"  Parameters: {}",
		a+"Completed 200 OK in 1ms",
		b+"Completed 200 OK in 1ms",
	)
	// The cut request continues as fragments: they never mix, and the request id ties them.
	if len(entries) != 5 {
		t.Fatalf("want 5 rows, got %d: %+v", len(entries), entries)
	}
	for index, id := range []string{"8a1b2c3d4e5f6789-FRA", "0f9e8d7c6b5a4321-FRA", "8a1b2c3d4e5f6789-FRA", "8a1b2c3d4e5f6789-FRA", "0f9e8d7c6b5a4321-FRA"} {
		if entries[index].RequestID != id {
			t.Fatalf("row %d has request id %q, want %q", index, entries[index].RequestID, id)
		}
	}
	if entries[0].Message != "Started GET \"/a\" for 127.0.0.1\nProcessing by AController#show as HTML" {
		t.Fatalf("tag must leave the message: %q", entries[0].Message)
	}
	if entries[2].Message != "  Parameters: {}" || entries[2].Raw != a+"  Parameters: {}" {
		t.Fatalf("unexpected cut row: %+v", entries[2])
	}
}

func TestSplitTagNeedsAnID(t *testing.T) {
	for _, line := range []string{"[ActiveJob] [Mailer] Performing", "[ERROR] boom", "[2026-09-20 10:00:00] started", "[abc1] short"} {
		if id, text := splitTag(line); id != "" || text != line {
			t.Fatalf("%q must not carry a request id, got %q", line, id)
		}
	}
	if id, text := splitTag("[3f2a9c1e-7b4d-4e8a-9c1e-7b4d4e8a9c1e]   indented"); id != "3f2a9c1e-7b4d-4e8a-9c1e-7b4d4e8a9c1e" || text != "  indented" {
		t.Fatalf("unexpected split: %q %q", id, text)
	}
}

func TestParseLineKeepsRequestIDCase(t *testing.T) {
	entry := ParseLine("stdout", "web", `{"msg":"hi","request_id":"8a1b2c3d-FRA"}`)
	if entry.RequestID != "8a1b2c3d-FRA" {
		t.Fatalf("request id = %q", entry.RequestID)
	}
}

func TestGrouperCapsARow(t *testing.T) {
	lines := []string{"head"}
	for range maxBlockLines + 10 {
		lines = append(lines, "  detail")
	}
	entries := group(lines...)
	if len(entries) != 2 || strings.Count(entries[0].Message, "\n") != maxBlockLines-1 || strings.Count(entries[1].Message, "\n") != 10 {
		t.Fatalf("want a full row and the overflow, got %d rows", len(entries))
	}
}
