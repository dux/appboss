package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// queryTimeout bounds one run, on the server through statement_timeout and on this side
	// through the context, so a closed browser never leaves a query burning CPU.
	queryTimeout = 30 * time.Second
	// queryRowLimit is how many rows travel to the console. The full count is still reported.
	queryRowLimit = 500
	// queryCellLimit keeps one wide column (a bytea, a document) from filling the response.
	queryCellLimit = 10000
	// queryAuditLimit is how much of a statement an audit row keeps.
	queryAuditLimit = 200
)

// QueryResult is one run of the SQL runner. Columns and Rows come from the last statement that
// returned any, which is what a runner shows; Command is that statement's tag. Every value is the
// server's text form, so a column of any type renders without a type map on this side.
type QueryResult struct {
	Database   string   `json:"database"`
	Columns    []string `json:"columns"`
	Rows       [][]any  `json:"rows"`
	RowCount   int64    `json:"row_count"`
	Command    string   `json:"command"`
	Statements int      `json:"statements"`
	Truncated  bool     `json:"truncated"`
	DurationMS int64    `json:"duration_ms"`
}

// Query runs sql against one database and returns its last result set. It accepts several
// statements in one run, since that is how an operator pastes work in; they share the connection
// but not a transaction, exactly as psql would run them.
func (s *Service) Query(ctx context.Context, database, sql string) (QueryResult, error) {
	database = strings.TrimSpace(database)
	sql = strings.TrimSpace(sql)
	if database == "" {
		return QueryResult{}, errors.New("a database is required")
	}
	if sql == "" {
		return QueryResult{}, errors.New("there is no SQL to run")
	}
	if !s.Enabled() {
		return QueryResult{}, errors.New("postgres is not enabled")
	}
	connConfig := s.connection(ctx)
	if connConfig == nil {
		return QueryResult{}, errors.New("no reachable PostgreSQL server")
	}

	// The context outlives statement_timeout, so the server's own error wins whenever it can:
	// it names the statement, a cancelled context only says the run was too slow.
	ctx, cancel := context.WithTimeout(ctx, queryTimeout+connectTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, mustConfig(connConfig, database))
	if err != nil {
		return QueryResult{}, fmt.Errorf("connect to %s: %w", database, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = %d", queryTimeout.Milliseconds())); err != nil {
		return QueryResult{}, err
	}

	result := QueryResult{Database: database}
	start := time.Now()
	reader := conn.PgConn().Exec(ctx, sql)
	for reader.NextResult() {
		result.Statements++
		rows := reader.ResultReader()
		fields := rows.FieldDescriptions()
		// A statement that returns rows replaces the previous result set; one that does not
		// (an INSERT, a SET) leaves the last grid in place and only updates the tag.
		if len(fields) > 0 {
			result.Columns = make([]string, len(fields))
			for i, field := range fields {
				result.Columns[i] = string(field.Name)
			}
			result.Rows = nil
			result.RowCount = 0
			result.Truncated = false
		}
		for rows.NextRow() {
			result.RowCount++
			if len(result.Rows) >= queryRowLimit {
				result.Truncated = true
				continue
			}
			result.Rows = append(result.Rows, textRow(rows.Values()))
		}
		tag, err := rows.Close()
		if err != nil {
			_ = reader.Close()
			return QueryResult{}, err
		}
		result.Command = tag.String()
		if len(fields) == 0 {
			result.RowCount = tag.RowsAffected()
		}
	}
	if err := reader.Close(); err != nil {
		return QueryResult{}, err
	}
	result.DurationMS = time.Since(start).Milliseconds()
	return result, nil
}

// textRow copies one row out of the reader. The values it hands out are reused by the next row,
// so every cell is copied here; a NULL stays nil and renders as such.
func textRow(values [][]byte) []any {
	row := make([]any, len(values))
	for i, value := range values {
		if value == nil {
			continue
		}
		text := string(value)
		if len(text) > queryCellLimit {
			text = text[:queryCellLimit] + "..."
		}
		row[i] = text
	}
	return row
}

// QueryAuditDetail is the one-line form of a statement an audit row keeps.
func QueryAuditDetail(sql string) string {
	sql = strings.Join(strings.Fields(sql), " ")
	if len(sql) > queryAuditLimit {
		return sql[:queryAuditLimit] + "..."
	}
	return sql
}
