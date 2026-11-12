package main

import (
	"database/sql"
	"errors"
)

// Old offline readers stopped after six accepted quotations and could later
// expand a partial note reading into the whole document. The offline reader is
// gone: new content only enters memory through an authorized read the owner
// selected. All that remains of this migration is bookkeeping: mark legacy
// notes as processed so no later import tries to expand them, and touch
// nothing that was already recorded.
func (s *Store) completeStoredReadings(user string) error {
	query := `SELECT s.user_id,s.id FROM sources s WHERE s.kind='self'
		AND NOT EXISTS (SELECT 1 FROM checked_note_readings c WHERE c.source_id=s.id)`
	var args []any
	if user != "" {
		// Other historical origins include AI exports or unsigned imported data.
		query += " AND s.user_id=? AND s.origin IN ('note','document','manual','chat','local')"
		args = append(args, user)
	} else {
		// A legacy Android JSON import verifies its full snapshot before requesting recovery.
		query += " AND NOT EXISTS (SELECT 1 FROM android_dossier a WHERE a.user_id=s.user_id)"
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return err
	}
	type sourceRef struct{ user, id string }
	var pending []sourceRef
	for rows.Next() {
		var r sourceRef
		if err = rows.Scan(&r.user, &r.id); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range pending {
		// Each old note commits independently. An interrupted upgrade resumes
		// from the next unprocessed source and cannot expose a partial reading.
		if err = s.write(func(tx *sql.Tx) error {
			var one int
			if err = tx.QueryRow("SELECT 1 FROM sources WHERE user_id=? AND id=? AND kind='self' AND NOT EXISTS (SELECT 1 FROM checked_note_readings WHERE source_id=?)", r.user, r.id, r.id).Scan(&one); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				return err
			}
			_, err = tx.Exec("INSERT INTO checked_note_readings VALUES(?,0)", r.id)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}
