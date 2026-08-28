package store

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// PruneAudio deletes the recordings of voicemails received before the cutoff
// and clears their audio_path. The voicemail rows themselves stay, so the
// transcription and mail text remain searchable forever. Returns the number of
// recordings removed.
func (s *Store) PruneAudio(before time.Time) (int, error) {
	rows, err := s.db.Query(
		`SELECT id, audio_path FROM voicemails WHERE audio_path != '' AND received_at < ?`,
		before.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	type victim struct {
		id   int64
		path string
	}
	var victims []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.path); err != nil {
			rows.Close()
			return 0, err
		}
		victims = append(victims, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	removed := 0
	var firstErr error
	for _, v := range victims {
		// A file that is already gone still counts as pruned: the row must
		// stop pointing at it either way.
		if err := os.Remove(v.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			if firstErr == nil {
				firstErr = fmt.Errorf("remove %s: %w", v.path, err)
			}
			continue
		}
		if _, err := s.db.Exec(`UPDATE voicemails SET audio_path = '' WHERE id = ?`, v.id); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

// AutoDone marks every still-open voicemail received longer than window ago
// as afgehandeld, and reports how many it closed. This is display state only:
// no PBX command is run and nothing is deleted. On weekdays the meldingen are
// picked up by a party that does not use the dashboard, so without this sweep
// the open list grows forever; anyone can still reopen a voicemail from the
// web UI, after which it stays open for good (SetDone sets reopened).
//
// done_at is set to received_at + window — the moment the voicemail aged out,
// not the moment the sweep happened — so closing a backlog leaves timestamps
// that still say something.
func (s *Store) AutoDone(now time.Time, window time.Duration) (int, error) {
	if window <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-window).UTC().Format(time.RFC3339)
	// received_at is UTC RFC3339, which SQLite's date functions parse and
	// string comparison orders by time.
	res, err := s.db.Exec(
		`UPDATE voicemails
		    SET done_at = strftime('%Y-%m-%dT%H:%M:%SZ', received_at, ?), done_auto = 1
		  WHERE done_at = '' AND reopened = 0 AND received_at < ?`,
		fmt.Sprintf("+%d seconds", int64(window/time.Second)), cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
