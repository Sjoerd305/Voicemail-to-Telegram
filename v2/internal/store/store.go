package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Voicemail struct {
	ID            int64     `json:"id"`
	ReceivedAt    time.Time `json:"received_at"`
	Subject       string    `json:"subject"`
	EmailText     string    `json:"email_text"`
	Transcription string    `json:"transcription"`
	AudioPath     string    `json:"-"`
	HasAudio      bool      `json:"has_audio"`
	MessageID     string    `json:"-"`
	Done          bool      `json:"done"`
	DoneAt        time.Time `json:"done_at,omitzero"`
}

type Event struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// modernc.org/sqlite works best with a single writer connection.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS voicemails (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	received_at TEXT NOT NULL,
	subject TEXT NOT NULL DEFAULT '',
	email_text TEXT NOT NULL DEFAULT '',
	transcription TEXT NOT NULL DEFAULT '',
	audio_path TEXT NOT NULL DEFAULT '',
	message_id TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_voicemails_message_id
	ON voicemails(message_id) WHERE message_id != '';
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	at TEXT NOT NULL,
	kind TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT ''
);
`)
	if err != nil {
		return err
	}
	// Added after the initial schema; empty string means "not done".
	if !s.hasColumn("voicemails", "done_at") {
		if _, err := s.db.Exec(`ALTER TABLE voicemails ADD COLUMN done_at TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) hasColumn(table, column string) bool {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil && name == column {
			return true
		}
	}
	return false
}

func (s *Store) Close() error { return s.db.Close() }

// IsProcessed reports whether a mail with this Message-ID was already handled.
// This is the duplicate-send guard: even if the process restarts mid-poll or
// the \Seen flag never lands, the same voicemail is never sent twice.
func (s *Store) IsProcessed(messageID string) (bool, error) {
	if messageID == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM voicemails WHERE message_id = ?`, messageID).Scan(&n)
	return n > 0, err
}

func (s *Store) SaveVoicemail(vm *Voicemail) error {
	res, err := s.db.Exec(
		`INSERT INTO voicemails (received_at, subject, email_text, transcription, audio_path, message_id)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		vm.ReceivedAt.UTC().Format(time.RFC3339), vm.Subject, vm.EmailText,
		vm.Transcription, vm.AudioPath, vm.MessageID,
	)
	if err != nil {
		return err
	}
	vm.ID, err = res.LastInsertId()
	return err
}

// ListOptions selects a page of voicemails. Paging is cursor based on the
// id (Before), so a page stays stable while new voicemails arrive at the top.
type ListOptions struct {
	Limit  int
	Before int64  // only voicemails with id < Before; 0 means from the newest
	Query  string // case-insensitive substring match on transcription, subject and mail body
	Done   *bool  // nil: all; true: handled only; false: open only
}

type Page struct {
	Items   []Voicemail `json:"items"`
	Total   int         `json:"total"`    // matches for Query/Done, ignoring Before
	HasMore bool        `json:"has_more"` // older matches exist beyond this page
}

const (
	DefaultPageSize = 10
	MaxPageSize     = 500
)

func (s *Store) ListVoicemails(opts ListOptions) (*Page, error) {
	limit := opts.Limit
	if limit <= 0 || limit > MaxPageSize {
		limit = DefaultPageSize
	}
	where := []string{"1=1"}
	args := []any{}
	if q := strings.TrimSpace(opts.Query); q != "" {
		pat := "%" + escapeLike(q) + "%"
		where = append(where, `(transcription LIKE ? ESCAPE '\' OR subject LIKE ? ESCAPE '\' OR email_text LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat, pat)
	}
	if opts.Done != nil {
		if *opts.Done {
			where = append(where, "done_at != ''")
		} else {
			where = append(where, "done_at = ''")
		}
	}
	cond := strings.Join(where, " AND ")

	page := &Page{Items: []Voicemail{}}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM voicemails WHERE `+cond, args...).Scan(&page.Total); err != nil {
		return nil, err
	}

	pageArgs := append([]any{}, args...)
	if opts.Before > 0 {
		cond += " AND id < ?"
		pageArgs = append(pageArgs, opts.Before)
	}
	// Fetch one extra row to learn whether another page exists.
	pageArgs = append(pageArgs, limit+1)
	rows, err := s.db.Query(
		`SELECT id, received_at, subject, email_text, transcription, audio_path, done_at
		 FROM voicemails WHERE `+cond+` ORDER BY id DESC LIMIT ?`, pageArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var vm Voicemail
		var at, doneAt string
		if err := rows.Scan(&vm.ID, &at, &vm.Subject, &vm.EmailText, &vm.Transcription, &vm.AudioPath, &doneAt); err != nil {
			return nil, err
		}
		vm.ReceivedAt, _ = time.Parse(time.RFC3339, at)
		vm.HasAudio = vm.AudioPath != ""
		vm.setDone(doneAt)
		page.Items = append(page.Items, vm)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.HasMore = true
	}
	return page, nil
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Stats are the dashboard counters. Days holds the last 14 calendar days in
// the server's local timezone, oldest first.
type Stats struct {
	Open  int        `json:"open"`
	Today int        `json:"today"`
	Week  int        `json:"week"`
	Days  []DayCount `json:"days"`
}

type DayCount struct {
	Date  string `json:"date"` // YYYY-MM-DD
	Count int    `json:"count"`
}

const statsDays = 14

func (s *Store) Stats(now time.Time) (*Stats, error) {
	st := &Stats{}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM voicemails WHERE done_at = ''`).Scan(&st.Open); err != nil {
		return nil, err
	}
	now = now.In(time.Local)
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	// ISO week starts on Monday.
	startOfWeek := startOfDay.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7))
	firstDay := startOfDay.AddDate(0, 0, -(statsDays - 1))
	st.Days = make([]DayCount, statsDays)
	for i := range st.Days {
		st.Days[i].Date = firstDay.AddDate(0, 0, i).Format("2006-01-02")
	}
	// received_at is stored as UTC RFC3339, so string comparison orders by time.
	since := firstDay.UTC()
	if startOfWeek.Before(firstDay) {
		since = startOfWeek.UTC()
	}
	rows, err := s.db.Query(`SELECT received_at FROM voicemails WHERE received_at >= ?`, since.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			continue
		}
		t = t.In(time.Local)
		if !t.Before(startOfDay) {
			st.Today++
		}
		if !t.Before(startOfWeek) {
			st.Week++
		}
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
		if idx := int(day.Sub(firstDay).Hours() / 24); idx >= 0 && idx < statsDays {
			st.Days[idx].Count++
		}
	}
	return st, rows.Err()
}

func (vm *Voicemail) setDone(doneAt string) {
	if doneAt == "" {
		return
	}
	vm.Done = true
	vm.DoneAt, _ = time.Parse(time.RFC3339, doneAt)
}

// SetDone marks a voicemail as handled (or reopens it) and returns the
// updated record.
func (s *Store) SetDone(id int64, done bool) (*Voicemail, error) {
	doneAt := ""
	if done {
		doneAt = time.Now().UTC().Format(time.RFC3339)
	}
	res, err := s.db.Exec(`UPDATE voicemails SET done_at = ? WHERE id = ?`, doneAt, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.GetVoicemail(id)
}

func (s *Store) GetVoicemail(id int64) (*Voicemail, error) {
	var vm Voicemail
	var at, doneAt string
	err := s.db.QueryRow(
		`SELECT id, received_at, subject, email_text, transcription, audio_path, done_at
		 FROM voicemails WHERE id = ?`, id).
		Scan(&vm.ID, &at, &vm.Subject, &vm.EmailText, &vm.Transcription, &vm.AudioPath, &doneAt)
	if err != nil {
		return nil, err
	}
	vm.ReceivedAt, _ = time.Parse(time.RFC3339, at)
	vm.HasAudio = vm.AudioPath != ""
	vm.setDone(doneAt)
	return &vm, nil
}

func (s *Store) LogEvent(kind, detail string) {
	// Best effort; events are informational only.
	_, _ = s.db.Exec(`INSERT INTO events (at, kind, detail) VALUES (?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), kind, detail)
}

func (s *Store) ListEvents(limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id, at, kind, detail FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var at string
		if err := rows.Scan(&e.ID, &at, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		e.At, _ = time.Parse(time.RFC3339, at)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CountVoicemails() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM voicemails`).Scan(&n)
	return n, err
}

var ErrNotFound = fmt.Errorf("not found")
