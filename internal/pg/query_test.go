package pg

import (
	"context"
	"strings"
	"testing"
	"time"

	"dboss/internal/config"
)

func TestQueryRejectsEmptyInput(t *testing.T) {
	service := New(config.Config{StateDir: t.TempDir(), Postgres: config.Postgres{Enabled: true}}, nil)
	if _, err := service.Query(context.Background(), "", "select 1"); err == nil {
		t.Fatal("a run without a database should fail")
	}
	if _, err := service.Query(context.Background(), "postgres", "  \n "); err == nil {
		t.Fatal("a run without SQL should fail")
	}
}

func TestQueryAuditDetailIsOneCappedLine(t *testing.T) {
	if got := QueryAuditDetail("select *\n  from users\nwhere id = 1"); got != "select * from users where id = 1" {
		t.Fatalf("detail = %q", got)
	}
	long := QueryAuditDetail("select " + strings.Repeat("x", 500))
	if len(long) != queryAuditLimit+3 || !strings.HasSuffix(long, "...") {
		t.Fatalf("detail should be capped at %d chars plus an ellipsis, got %d", queryAuditLimit, len(long))
	}
}

func TestTextRowCopiesAndCapsCells(t *testing.T) {
	buffer := []byte("first")
	row := textRow([][]byte{buffer, nil, []byte(strings.Repeat("y", queryCellLimit+50))})
	copy(buffer, "SECON")
	if row[0] != "first" {
		t.Fatalf("cell must be copied out of the reader's buffer, got %v", row[0])
	}
	if row[1] != nil {
		t.Fatalf("NULL must stay nil, got %v", row[1])
	}
	if text := row[2].(string); len(text) != queryCellLimit+3 || !strings.HasSuffix(text, "...") {
		t.Fatalf("wide cell = %d chars, want %d plus an ellipsis", len(text), queryCellLimit)
	}
}

// TestQueryAgainstLiveServer runs the real path: several statements in one call, the last result
// set wins, and the row cap reports the full count. It skips unless a server is reachable.
func TestQueryAgainstLiveServer(t *testing.T) {
	service := New(config.Config{StateDir: t.TempDir(), Postgres: config.Postgres{Enabled: true}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	service.detect(ctx)
	if service.connConfig == nil {
		t.Skip("no reachable PostgreSQL server")
	}

	result, err := service.Query(ctx, "postgres", "select 1 as n, null::text as missing")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Columns) != 2 || result.Columns[0] != "n" || result.Columns[1] != "missing" {
		t.Fatalf("columns = %v", result.Columns)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != "1" || result.Rows[0][1] != nil {
		t.Fatalf("rows = %v", result.Rows)
	}
	if result.RowCount != 1 || result.Statements != 1 {
		t.Fatalf("row_count = %d, statements = %d", result.RowCount, result.Statements)
	}

	batch, err := service.Query(ctx, "postgres", "select 1; select 'a' as letter")
	if err != nil {
		t.Fatal(err)
	}
	if batch.Statements != 2 || len(batch.Columns) != 1 || batch.Columns[0] != "letter" {
		t.Fatalf("the last result set should win, got %d statements and columns %v", batch.Statements, batch.Columns)
	}

	capped, err := service.Query(ctx, "postgres", "select generate_series(1, 700)")
	if err != nil {
		t.Fatal(err)
	}
	if !capped.Truncated || len(capped.Rows) != queryRowLimit || capped.RowCount != 700 {
		t.Fatalf("truncated = %v, rows = %d, row_count = %d", capped.Truncated, len(capped.Rows), capped.RowCount)
	}

	if _, err := service.Query(ctx, "postgres", "select from nothing_here"); err == nil {
		t.Fatal("a bad statement should return the server's error")
	}
}
