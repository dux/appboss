package logstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// maxExceptionValues caps the unique user refs and IPs kept on one minute row.
const maxExceptionValues = 5

// ExceptionLimit caps the groups one read returns; the busiest, most recent groups are kept.
const ExceptionLimit = 100

// ExceptionMinuteLimit caps the minute rows returned per group to the most recent ones.
const ExceptionMinuteLimit = 50

// ExceptionFilter narrows ExceptionGroups. A zero Since means every group; Limit is clamped to
// ExceptionLimit.
type ExceptionFilter struct {
	Since time.Time
	Limit int
}

// ExceptionSummary is one exception group for the console: its totals and one row per UTC minute.
// Dump is the first nonempty dump and travels only with the group it belongs to.
type ExceptionSummary struct {
	ExpUID     string               `json:"exp_uid"`
	Dump       string               `json:"dump,omitempty"`
	FirstAt    time.Time            `json:"first_at"`
	LastAt     time.Time            `json:"last_at"`
	Count      int64                `json:"count"`
	IsResolved bool                 `json:"is_resolved"`
	Minutes    []ExceptionMinuteRow `json:"minutes"`
}

// ExceptionMinuteRow is one minute of a group: how often it happened and who it happened to.
type ExceptionMinuteRow struct {
	MinuteAt    time.Time `json:"minute_at"`
	Count       int64     `json:"count"`
	Message     string    `json:"message"`
	Users       []string  `json:"users,omitempty"`
	IPs         []string  `json:"ips,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	Description string    `json:"description,omitempty"`
}

// Exceptions lists an app's groups whose last occurrence is newer than filter.Since, newest
// first, each with its minute rows (newest first). The tables are missing on a database written
// before the feature, which reads as empty.
func (s *Store) Exceptions(app string, filter ExceptionFilter) ([]ExceptionSummary, error) {
	limit := filter.Limit
	if limit <= 0 || limit > ExceptionLimit {
		limit = ExceptionLimit
	}
	summaries := []ExceptionSummary{}
	// writerForPrune opens an existing database without creating one and runs the schema, so a
	// database written before is_resolved existed gains the column before this SELECT reads it.
	w, err := s.writerForPrune(app)
	if err != nil || w == nil {
		return summaries, err
	}
	db := w.db
	query := `SELECT exp_uid, COALESCE(dump, ''), first_at, last_at, count, is_resolved FROM exceptions`
	var args []any
	if !filter.Since.IsZero() {
		query += ` WHERE last_at >= ?`
		args = append(args, filter.Since.UnixMilli())
	}
	query += ` ORDER BY last_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return summaries, nil
		}
		return summaries, err
	}
	defer rows.Close()
	index := map[string]int{}
	var uids []string
	for rows.Next() {
		var summary ExceptionSummary
		var firstAt, lastAt int64
		var resolved int
		if err := rows.Scan(&summary.ExpUID, &summary.Dump, &firstAt, &lastAt, &summary.Count, &resolved); err != nil {
			return summaries, err
		}
		summary.FirstAt = time.UnixMilli(firstAt).UTC()
		summary.LastAt = time.UnixMilli(lastAt).UTC()
		summary.IsResolved = resolved != 0
		index[summary.ExpUID] = len(summaries)
		uids = append(uids, summary.ExpUID)
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return summaries, err
	}
	if len(uids) == 0 {
		return summaries, nil
	}
	return summaries, attachExceptionMinutes(db, summaries, index, uids)
}

// UnresolvedExceptionCount counts the exception groups still flagged unresolved for one app. It
// reads without creating a database and treats a database written before the feature as zero.
func (s *Store) UnresolvedExceptionCount(app string) (int, error) {
	count := 0
	err := s.read(app, func(db *sql.DB) error {
		return db.QueryRow(`SELECT COUNT(*) FROM exceptions WHERE is_resolved = 0`).Scan(&count)
	})
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return 0, nil
	}
	return count, err
}

func attachExceptionMinutes(db *sql.DB, summaries []ExceptionSummary, index map[string]int, uids []string) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(uids)), ",")
	args := make([]any, len(uids))
	for i, uid := range uids {
		args[i] = uid
	}
	// Only the most recent ExceptionMinuteLimit minutes per group travel to the console; a
	// window function keeps that bounded per group instead of truncating after the fact.
	query := `SELECT exp_uid, minute_at, count, message, users, tags, description, ips FROM (
		SELECT *, ROW_NUMBER() OVER (PARTITION BY exp_uid ORDER BY minute_at DESC) AS rn
		FROM exception_logs WHERE exp_uid IN (` + placeholders + `)
	) WHERE rn <= ? ORDER BY minute_at DESC`
	args = append(args, ExceptionMinuteLimit)
	rows, err := db.Query(query, args...)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var uid string
		var minuteAt int64
		var minute ExceptionMinuteRow
		var users, tags, description, ips sql.NullString
		if err := rows.Scan(&uid, &minuteAt, &minute.Count, &minute.Message, &users, &tags, &description, &ips); err != nil {
			return err
		}
		minute.MinuteAt = time.UnixMilli(minuteAt).UTC()
		minute.Users = decodeList(users)
		minute.Tags = decodeList(tags)
		minute.IPs = decodeList(ips)
		minute.Description = description.String
		if i, ok := index[uid]; ok {
			summaries[i].Minutes = append(summaries[i].Minutes, minute)
		}
	}
	return rows.Err()
}

// ExceptionMinute is one UTC minute of an exception group: how many times it happened, the
// metadata of the first occurrence in that minute, and the distinct users and IPs seen.
type ExceptionMinute struct {
	MinuteAt    time.Time
	Count       int
	Message     string
	Tags        []string
	Description string
	Users       []string
	IPs         []string
}

// ExceptionGroup is one exception fingerprint as read from an exception log: its total count and
// span, the first nonempty dump, and one entry per UTC minute it occurred in.
type ExceptionGroup struct {
	ExpUID  string
	Dump    string
	FirstAt time.Time
	LastAt  time.Time
	Count   int
	Minutes []ExceptionMinute
}

// ExceptionBatch is one pass over an exception log file: the groups and malformed-line warnings
// to commit, and the file offset to advance. All four are written in one transaction.
type ExceptionBatch struct {
	Path     string
	Inode    uint64
	Offset   int64
	Groups   []ExceptionGroup
	Warnings []LogEntry
}

// AppendExceptions upserts exception groups and their minute rows, appends malformed-line
// warnings as log rows and advances the tail offset, all in one transaction. The first nonempty
// dump and the first occurrence's metadata per minute win; later occurrences only raise counters
// and add distinct users/IPs (capped at maxExceptionValues).
func (s *Store) AppendExceptions(app string, batch ExceptionBatch) error {
	if len(batch.Groups) == 0 && len(batch.Warnings) == 0 && batch.Path == "" {
		return nil
	}
	w, err := s.writer(app)
	if err != nil {
		return err
	}
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}
	if err := saveExceptionGroups(tx, batch.Groups); err != nil {
		_ = tx.Rollback()
		return err
	}
	if len(batch.Warnings) > 0 {
		if err := insertLogs(tx, batch.Warnings); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if batch.Path != "" {
		if _, err := tx.Exec(`INSERT INTO tail_offsets (path, inode, offset, updated_ts) VALUES (?, ?, ?, ?) ON CONFLICT(path) DO UPDATE SET inode = excluded.inode, offset = excluded.offset, updated_ts = excluded.updated_ts`, batch.Path, batch.Inode, batch.Offset, stamp(time.Now())); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func saveExceptionGroups(tx *sql.Tx, groups []ExceptionGroup) error {
	for _, group := range groups {
		if _, err := tx.Exec(`
			INSERT INTO exceptions (exp_uid, dump, first_at, last_at, count) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(exp_uid) DO UPDATE SET
				dump = CASE WHEN (exceptions.dump IS NULL OR exceptions.dump = '') AND excluded.dump <> '' THEN excluded.dump ELSE exceptions.dump END,
				first_at = MIN(exceptions.first_at, excluded.first_at),
				last_at = MAX(exceptions.last_at, excluded.last_at),
				count = exceptions.count + excluded.count`,
			group.ExpUID, group.Dump, group.FirstAt.UnixMilli(), group.LastAt.UnixMilli(), group.Count); err != nil {
			return err
		}
		for _, minute := range group.Minutes {
			if err := saveExceptionMinute(tx, group.ExpUID, minute); err != nil {
				return err
			}
		}
	}
	return nil
}

// saveExceptionMinute inserts a minute row or, when it exists, adds the count and merges the
// distinct users and IPs. Metadata is written only on insert, so the first occurrence in the
// minute owns the row's message, tags and description.
func saveExceptionMinute(tx *sql.Tx, expUID string, minute ExceptionMinute) error {
	minuteAt := minute.MinuteAt.UnixMilli()
	var (
		existingCount int
		existingUsers sql.NullString
		existingIPs   sql.NullString
	)
	err := tx.QueryRow(`SELECT count, users, ips FROM exception_logs WHERE exp_uid = ? AND minute_at = ?`, expUID, minuteAt).Scan(&existingCount, &existingUsers, &existingIPs)
	switch {
	case err == sql.ErrNoRows:
		_, err = tx.Exec(`INSERT INTO exception_logs (exp_uid, minute_at, count, message, tags, description, users, ips) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			expUID, minuteAt, minute.Count, minute.Message, encodeList(minute.Tags), nullString(minute.Description), encodeList(minute.Users), encodeList(minute.IPs))
		return err
	case err != nil:
		return err
	}
	users := AppendUniqueValues(decodeList(existingUsers), minute.Users...)
	ips := AppendUniqueValues(decodeList(existingIPs), minute.IPs...)
	_, err = tx.Exec(`UPDATE exception_logs SET count = count + ?, users = ?, ips = ? WHERE exp_uid = ? AND minute_at = ?`,
		minute.Count, encodeList(users), encodeList(ips), expUID, minuteAt)
	return err
}

// SetExceptionResolved flips the is_resolved flag on one exception group. A fingerprint that is
// not on disk is an error, so the console never reports a resolve that did not land.
func (s *Store) SetExceptionResolved(app, expUID string, resolved bool) error {
	w, err := s.writer(app)
	if err != nil {
		return err
	}
	flag := 0
	if resolved {
		flag = 1
	}
	result, err := w.db.Exec(`UPDATE exceptions SET is_resolved = ? WHERE exp_uid = ?`, flag, expUID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("exception not found")
	}
	return nil
}

// AppendUniqueValues trims and dedupes values in order, keeping at most maxExceptionValues
// non-empty entries. Existing values win, so a full row stays as it was.
func AppendUniqueValues(existing []string, values ...string) []string {
	out := make([]string, 0, len(existing)+len(values))
	seen := map[string]bool{}
	appendOne := func(value string) bool {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return true
		}
		seen[value] = true
		out = append(out, value)
		return len(out) < maxExceptionValues
	}
	for _, value := range existing {
		if !appendOne(value) {
			return out
		}
	}
	for _, value := range values {
		if !appendOne(value) {
			break
		}
	}
	return out
}

func encodeList(values []string) any {
	if len(values) == 0 {
		return nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	return string(encoded)
}

func decodeList(value sql.NullString) []string {
	if !value.Valid || value.String == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(value.String), &out) != nil {
		return nil
	}
	return out
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
