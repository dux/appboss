package logstore

import (
	"time"

	_ "modernc.org/sqlite"
)

// TailOffsets returns every tracked app log file for app, keyed by absolute path.
func (s *Store) TailOffsets(app string) (map[string]TailOffset, error) {
	result := map[string]TailOffset{}
	db, done, err := s.reader(app)
	if err != nil || db == nil {
		return result, err
	}
	defer done()
	rows, err := db.Query(`SELECT path, inode, offset FROM tail_offsets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var offset TailOffset
		if err := rows.Scan(&offset.Path, &offset.Inode, &offset.Offset); err != nil {
			return nil, err
		}
		result[offset.Path] = offset
	}
	return result, rows.Err()
}

// SaveTailOffset records how far the tailer read into one app log file.
func (s *Store) SaveTailOffset(app, path string, inode uint64, offset int64) error {
	w, err := s.writer(app)
	if err != nil {
		return err
	}
	_, err = w.db.Exec(`INSERT INTO tail_offsets (path, inode, offset, updated_ts) VALUES (?, ?, ?, ?) ON CONFLICT(path) DO UPDATE SET inode = excluded.inode, offset = excluded.offset, updated_ts = excluded.updated_ts`, path, inode, offset, stamp(time.Now()))
	return err
}

// RemoveTailOffsets drops the tracked offsets of files that no longer exist.
func (s *Store) RemoveTailOffsets(app string, paths []string) error {
	if len(paths) == 0 {
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
	for _, path := range paths {
		if _, err := tx.Exec(`DELETE FROM tail_offsets WHERE path = ?`, path); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
