package logstore

import (
	"database/sql"
	"strings"
	"time"
)

// countryChunk bounds the ids of one IN list, well under SQLite's variable limit.
const countryChunk = 500

// RequestCountries maps request ids to the two-letter country the proxy recorded for them
// (CF-IPCountry). Only requests since since are looked at, so the ts index bounds the scan; an id
// without a request row or without a country is left out.
func (s *Store) RequestCountries(app string, ids []string, since time.Time) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	err := s.read(app, func(db *sql.DB) error {
		for start := 0; start < len(ids); start += countryChunk {
			chunk := ids[start:min(start+countryChunk, len(ids))]
			args := []any{stamp(since)}
			for _, id := range chunk {
				args = append(args, id)
			}
			query := `SELECT request_id, country FROM requests WHERE ts >= ? AND country <> '' AND request_id IN (?` + strings.Repeat(",?", len(chunk)-1) + `)`
			rows, err := db.Query(query, args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id, country string
				if err := rows.Scan(&id, &country); err != nil {
					rows.Close()
					return err
				}
				out[id] = country
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}
