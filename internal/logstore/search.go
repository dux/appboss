package logstore

import (
	"database/sql"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SearchLogs returns the newest matching rows, newest first.
func (s *Store) SearchLogs(app string, filter LogFilter) ([]LogEntry, error) {
	db, done, err := s.reader(app)
	if err != nil || db == nil {
		return nil, err
	}
	defer done()
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	where := []string{}
	args := []any{}
	channelProcess := false
	if filter.Channel != "" {
		if path, ok := strings.CutPrefix(filter.Channel, "file:"); ok {
			where = append(where, "logs.source = 'file'", "logs.process = ?")
			args = append(args, path)
			channelProcess = true
		} else if name, ok := strings.CutPrefix(filter.Channel, "stdout:"); ok {
			where = append(where, "logs.source = 'stdout'", "logs.process = ?")
			args = append(args, name)
			channelProcess = true
		} else {
			where = append(where, "logs.source = ?")
			args = append(args, filter.Channel)
		}
	}
	if filter.Process != "" && !channelProcess {
		where = append(where, "logs.process = ?")
		args = append(args, filter.Process)
	}
	if filter.Level != "" {
		where = append(where, "logs.level = ?")
		args = append(args, filter.Level)
	}
	if !filter.Since.IsZero() {
		where = append(where, "logs.ts >= ?")
		args = append(args, stamp(filter.Since))
	}
	if !filter.Before.IsZero() {
		where = append(where, "logs.ts < ?")
		args = append(args, stamp(filter.Before))
	}
	from := "logs"
	if term := ftsQuery(filter.Query); term != "" {
		from = "logs JOIN logs_fts ON logs_fts.rowid = logs.rowid"
		where = append(where, "logs_fts MATCH ?")
		args = append(args, term)
	}
	query := `SELECT logs.ts, logs.source, logs.process, logs.stream, logs.level, logs.message, logs.request_id, logs.raw FROM ` + from
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY logs.ts DESC LIMIT ?"
	args = append(args, limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []LogEntry
	for rows.Next() {
		var e LogEntry
		var ts string
		if err := rows.Scan(&ts, &e.Source, &e.Process, &e.Stream, &e.Level, &e.Message, &e.RequestID, &e.Raw); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, ts)
		result = append(result, e)
	}
	return result, rows.Err()
}

// SearchRequests returns the newest matching rows, newest first.
func (s *Store) SearchRequests(app string, filter RequestFilter) ([]RequestEntry, error) {
	db, done, err := s.reader(app)
	if err != nil || db == nil {
		return nil, err
	}
	defer done()
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	where := []string{}
	args := []any{}
	if filter.Method != "" {
		where = append(where, "method = ?")
		args = append(args, filter.Method)
	}
	if filter.Process != "" {
		where = append(where, "process = ?")
		args = append(args, filter.Process)
	}
	if filter.Status != 0 {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.StatusClass != 0 {
		where = append(where, "status >= ? AND status < ?")
		args = append(args, filter.StatusClass, filter.StatusClass+100)
	}
	if !filter.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, stamp(filter.Since))
	}
	if !filter.Before.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, stamp(filter.Before))
	}
	if filter.Query != "" {
		where = append(where, "(host LIKE ? OR path LIKE ? OR ip LIKE ? OR ua LIKE ? OR request_id LIKE ?)")
		like := "%" + filter.Query + "%"
		args = append(args, like, like, like, like, like)
	}
	query := `SELECT ts, method, host, path, status, duration_ms, bytes_out, ip, ua, request_id, process, country FROM requests`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY ts DESC LIMIT ?"
	args = append(args, limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RequestEntry
	for rows.Next() {
		var e RequestEntry
		var ts string
		if err := rows.Scan(&ts, &e.Method, &e.Host, &e.Path, &e.Status, &e.DurationMS, &e.BytesOut, &e.IP, &e.UserAgent, &e.RequestID, &e.Process, &e.Country); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, ts)
		result = append(result, e)
	}
	return result, rows.Err()
}

// SearchAudit returns the newest matching audit rows, newest first.
func (s *Store) SearchAudit(filter AuditFilter) ([]AuditEntry, error) {
	db, done, err := s.reader(HostApp)
	if err != nil || db == nil {
		return nil, err
	}
	defer done()
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	where := []string{}
	args := []any{}
	if filter.ID > 0 {
		where = append(where, "rowid = ?")
		args = append(args, filter.ID)
	}
	for column, value := range map[string]string{"app": filter.App, "actor": filter.Actor, "action": filter.Action} {
		if value != "" {
			where = append(where, column+" = ?")
			args = append(args, value)
		}
	}
	if !filter.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, stamp(filter.Since))
	}
	if !filter.Before.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, stamp(filter.Before))
	}
	query := `SELECT rowid, ts, actor, app, action, detail, result, error FROM audit`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY ts DESC LIMIT ?"
	args = append(args, limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.App, &e.Action, &e.Detail, &e.Result, &e.Error); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, ts)
		result = append(result, e)
	}
	return result, rows.Err()
}

// Tree returns name, on-disk sqlite size and channels for each app. Missing databases stay
// missing: this does not create them.
func (s *Store) Tree(names []string) ([]AppTree, error) {
	out := make([]AppTree, 0, len(names))
	for _, name := range names {
		channels, err := s.Channels(name)
		if err != nil {
			return nil, err
		}
		out = append(out, AppTree{Name: name, Bytes: s.diskBytes(name), Channels: channels})
	}
	return out, nil
}

// Channels lists the log types an app has. The reserved HostApp exposes only the dboss daemon
// log; every real app exposes the request table, one channel per service that wrote stdout, and one
// channel per app log file.
func (s *Store) Channels(app string) ([]Channel, error) {
	if app == HostApp {
		return []Channel{{ID: "dboss", Label: "dboss"}}, nil
	}
	channels := []Channel{}
	requesters, err := s.distinct(app, `SELECT DISTINCT process FROM requests WHERE process <> '' ORDER BY process`)
	if err != nil {
		return nil, err
	}
	for _, name := range requesters {
		channels = append(channels, Channel{ID: "request:" + name, Label: name + " requests"})
	}
	services, err := s.distinct(app, `SELECT DISTINCT process FROM logs WHERE source = 'stdout' ORDER BY process`)
	if err != nil {
		return nil, err
	}
	for _, name := range services {
		channels = append(channels, Channel{ID: "stdout:" + name, Label: name})
	}
	files, err := s.distinct(app, `SELECT DISTINCT process FROM logs WHERE source = 'file' ORDER BY process`)
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		channels = append(channels, Channel{ID: "file:" + path, Label: path})
	}
	return channels, nil
}

func (s *Store) diskBytes(app string) int64 {
	var total int64
	path := s.dbPath(app)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}

// distinct runs a single-column query against the app database, without creating one.
func (s *Store) distinct(app, query string) ([]string, error) {
	var values []string
	err := s.read(app, func(db *sql.DB) (err error) {
		values, err = queryValues(db, query)
		return err
	})
	return values, err
}

func queryValues(db *sql.DB, query string) ([]string, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// ftsQuery wraps a user phrase so each token is a prefix match and keeps FTS syntax out of the
// query. Without the quoting a stray " would turn into an FTS error.
func ftsQuery(query string) string {
	tokens := strings.Fields(query)
	for i, token := range tokens {
		tokens[i] = `"` + strings.ReplaceAll(token, `"`, `""`) + `"*`
	}
	return strings.Join(tokens, " ")
}
