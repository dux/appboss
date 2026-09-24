package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// MinDuckDB is the oldest duckdb CLI dboss runs: allowed_directories, which keeps a query
	// inside one app's events, and the lambda syntax the filters compile to both need it.
	MinDuckDB = "1.3.0"
	// queryTimeout bounds one run; the process is killed when it passes.
	queryTimeout = 30 * time.Second
	// queryRowLimit is how many rows travel back. The run stops reading after it.
	queryRowLimit = 500
	// queryCellLimit keeps one wide value (a data blob) from filling the response.
	queryCellLimit = 10000
	// QueryAuditLimit is how much of a statement an audit row keeps.
	QueryAuditLimit = 200
)

// QueryResult is one DuckDB run, in the same shape as the Postgres runner's so the console can
// share its grid. Rows hold JSON values as DuckDB printed them.
type QueryResult struct {
	Columns    []string `json:"columns"`
	Rows       [][]any  `json:"rows"`
	RowCount   int64    `json:"row_count"`
	Truncated  bool     `json:"truncated"`
	DurationMS int64    `json:"duration_ms"`
}

// DuckDB is an external duckdb CLI. dboss stays pure Go: it never links DuckDB, it runs the CLI
// when a query needs SQL and works without it everywhere else.
type DuckDB struct {
	Path    string
	Version string
}

var (
	lookPath       = exec.LookPath
	versionPattern = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)
)

// FindDuckDB looks for duckdb on PATH and checks its version.
func FindDuckDB(ctx context.Context) (DuckDB, error) {
	path, err := lookPath("duckdb")
	if err != nil {
		return DuckDB{}, errors.New("duckdb is not installed (https://duckdb.org); event SQL and funnels need it")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return DuckDB{}, fmt.Errorf("duckdb --version: %w", err)
	}
	version := versionPattern.FindString(string(output))
	if version == "" {
		return DuckDB{}, fmt.Errorf("duckdb --version: unexpected output %q", strings.TrimSpace(string(output)))
	}
	version = strings.TrimPrefix(version, "v")
	if !versionAtLeast(version, MinDuckDB) {
		return DuckDB{}, fmt.Errorf("duckdb %s is too old; events need %s or newer", version, MinDuckDB)
	}
	return DuckDB{Path: path, Version: version}, nil
}

func versionAtLeast(version, minimum string) bool {
	parse := func(text string) [3]int {
		var out [3]int
		match := versionPattern.FindStringSubmatch(text)
		for i := range 3 {
			if match != nil {
				out[i], _ = strconv.Atoi(match[i+1])
			}
		}
		return out
	}
	have, want := parse(version), parse(minimum)
	for i := range 3 {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}

// sandbox confines a run to one app's events directory: nothing outside it can be read, no
// extension installed and no database attached, and the query cannot lift the limits because
// the configuration is locked. Writing inside the directory stays possible (COPY TO), which is why
// the SQL runner is an audited admin action.
func sandbox(dir string) string {
	return strings.Join([]string{
		"SET TimeZone='UTC'",
		fmt.Sprintf("SET allowed_directories=[%s]", quote(dir+"/")),
		"SET enable_external_access=false",
		"SET autoinstall_known_extensions=false",
		"SET lock_configuration=true",
	}, ";\n") + ";\n"
}

// Query runs sql against the app's views inside the sandbox and returns the last result set.
func (d DuckDB) Query(ctx context.Context, dir, views, sql string) (QueryResult, error) {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return QueryResult{}, errors.New("there is no SQL to run")
	}
	return d.run(ctx, sandbox(dir)+views+sql, queryRowLimit)
}

func (d DuckDB) run(ctx context.Context, script string, limit int) (QueryResult, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	// :memory: keeps the run from creating a database file; -bail stops at the first error.
	command := exec.CommandContext(ctx, d.Path, ":memory:", "-json", "-bail", "-noheader")
	command.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return QueryResult{}, err
	}
	start := time.Now()
	if err := command.Start(); err != nil {
		return QueryResult{}, err
	}
	result, readErr := readResults(stdout, limit)
	if result.Truncated {
		// Enough rows: stop the run instead of draining the rest.
		cancel()
	}
	io.Copy(io.Discard, stdout)
	waitErr := command.Wait()
	result.DurationMS = time.Since(start).Milliseconds()
	if result.Truncated {
		return result, nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return QueryResult{}, fmt.Errorf("query ran longer than %s", queryTimeout)
	}
	if message := strings.TrimSpace(stderr.String()); message != "" {
		return QueryResult{}, errors.New(firstLines(message, 3))
	}
	if waitErr != nil {
		return QueryResult{}, fmt.Errorf("duckdb: %w", waitErr)
	}
	if readErr != nil {
		return QueryResult{}, readErr
	}
	return result, nil
}

// readResults decodes the JSON arrays duckdb prints, one per statement that returned rows, and
// keeps the last. Objects are decoded token by token so the column order survives.
func readResults(output io.Reader, limit int) (QueryResult, error) {
	decoder := json.NewDecoder(output)
	decoder.UseNumber()
	var result QueryResult
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return result, fmt.Errorf("read duckdb output: %w", err)
		}
		if delim, ok := token.(json.Delim); !ok || delim != '[' {
			continue
		}
		result = QueryResult{}
		for decoder.More() {
			columns, row, err := readObject(decoder)
			if err != nil {
				return result, fmt.Errorf("read duckdb output: %w", err)
			}
			if result.Columns == nil {
				result.Columns = columns
			}
			result.RowCount++
			if len(result.Rows) >= limit {
				result.Truncated = true
				return result, nil
			}
			result.Rows = append(result.Rows, row)
		}
		decoder.Token() // ]
	}
}

func readObject(decoder *json.Decoder) ([]string, []any, error) {
	if _, err := decoder.Token(); err != nil { // {
		return nil, nil, err
	}
	var columns []string
	var row []any
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, nil, err
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, err
		}
		if text, ok := value.(string); ok && len(text) > queryCellLimit {
			value = text[:queryCellLimit] + "…"
		}
		columns = append(columns, fmt.Sprint(key))
		row = append(row, value)
	}
	_, err := decoder.Token() // }
	return columns, row, err
}

func firstLines(text string, count int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.Join(lines, "\n")
}
