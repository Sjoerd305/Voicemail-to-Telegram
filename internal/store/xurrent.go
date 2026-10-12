package store

import (
	"database/sql"
	"time"
)

// OpenReadOnly opens the database without migrating or writing, so a tool
// can read the live database while the bot keeps running.
func OpenReadOnly(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// XurrentPending returns the voicemails whose Xurrent check is still open,
// oldest first.
func (s *Store) XurrentPending() ([]Voicemail, error) {
	rows, err := s.db.Query(
		`SELECT ` + voicemailColumns + ` FROM voicemails WHERE xurrent_status = 'pending' ORDER BY received_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Voicemail
	for rows.Next() {
		vm, err := scanVoicemail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *vm)
	}
	return out, rows.Err()
}

func (s *Store) SetXurrent(id int64, status, detail string, requestID int) error {
	_, err := s.db.Exec(
		`UPDATE voicemails SET xurrent_status = ?, xurrent_detail = ?, xurrent_request_id = ? WHERE id = ?`,
		status, detail, requestID, id)
	return err
}

// VoicemailsBetween returns voicemails received in [since, until), oldest
// first, with only the columns every schema version has. Used by the
// backtest, which may run against a database the new version never opened.
func (s *Store) VoicemailsBetween(since, until time.Time) ([]Voicemail, error) {
	rows, err := s.db.Query(
		`SELECT id, received_at, subject, email_text FROM voicemails
		  WHERE received_at >= ? AND received_at < ? ORDER BY received_at, id`,
		since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Voicemail
	for rows.Next() {
		var vm Voicemail
		var at string
		if err := rows.Scan(&vm.ID, &at, &vm.Subject, &vm.EmailText); err != nil {
			return nil, err
		}
		vm.ReceivedAt, _ = time.Parse(time.RFC3339, at)
		out = append(out, vm)
	}
	return out, rows.Err()
}
