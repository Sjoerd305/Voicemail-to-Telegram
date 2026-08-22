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
