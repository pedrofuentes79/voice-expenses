package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS recordings (
	id           TEXT PRIMARY KEY,
	recorded_at  TEXT NOT NULL,
	audio_path   TEXT NOT NULL,
	transcript   TEXT,
	error        TEXT,
	processed_at TEXT
);
CREATE TABLE IF NOT EXISTS entries (
	id             TEXT PRIMARY KEY,
	recording_id   TEXT REFERENCES recordings(id),
	status         TEXT NOT NULL,
	amount         REAL,
	currency       TEXT NOT NULL,
	category       TEXT,
	merchant       TEXT,
	payment_method TEXT,
	note           TEXT NOT NULL DEFAULT '',
	date           TEXT NOT NULL,
	source         TEXT NOT NULL,
	created_at     TEXT NOT NULL,
	updated_at     TEXT NOT NULL,
	deleted        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS entries_recording ON entries(recording_id);
`

// Entry is one expense. Status "review" means the model filled it and nobody has confirmed it yet.
type Entry struct {
	ID            string   `json:"id"`
	RecordingID   *string  `json:"recording_id"`
	Status        string   `json:"status"`
	Amount        *float64 `json:"amount"`
	Currency      string   `json:"currency"`
	Category      *string  `json:"category"`
	Merchant      *string  `json:"merchant"`
	PaymentMethod *string  `json:"payment_method"`
	Note          string   `json:"note"`
	Date          string   `json:"date"`
	Source        string   `json:"source"`
	Transcript    *string  `json:"transcript"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db}, nil
}

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

const entryColumns = `e.id, e.recording_id, e.status, e.amount, e.currency, e.category, e.merchant,
	e.payment_method, e.note, e.date, e.source, r.transcript, e.created_at, e.updated_at`

func scanEntries(rows *sql.Rows) ([]Entry, error) {
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.RecordingID, &e.Status, &e.Amount, &e.Currency, &e.Category, &e.Merchant,
			&e.PaymentMethod, &e.Note, &e.Date, &e.Source, &e.Transcript, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (s *Store) Entries(ctx context.Context) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryColumns+` FROM entries e
		LEFT JOIN recordings r ON r.id = e.recording_id
		WHERE e.deleted = 0 ORDER BY e.date DESC, e.created_at DESC`)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

func (s *Store) Entry(ctx context.Context, id string) (*Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryColumns+` FROM entries e
		LEFT JOIN recordings r ON r.id = e.recording_id WHERE e.id = ?`, id)
	if err != nil {
		return nil, err
	}
	entries, err := scanEntries(rows)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	return &entries[0], nil
}

func (s *Store) RecordingEntries(ctx context.Context, recordingID string) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryColumns+` FROM entries e
		JOIN recordings r ON r.id = e.recording_id
		WHERE e.recording_id = ? AND e.deleted = 0 ORDER BY e.id`, recordingID)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

// RecordingProcessed reports whether a recording already produced its entries, so a client retry is a no-op.
func (s *Store) RecordingProcessed(ctx context.Context, id string) (bool, error) {
	var processedAt *string
	err := s.db.QueryRowContext(ctx, `SELECT processed_at FROM recordings WHERE id = ?`, id).Scan(&processedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return processedAt != nil, err
}

func (s *Store) SaveRecording(ctx context.Context, id, recordedAt, audioPath string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO recordings (id, recorded_at, audio_path) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET audio_path = excluded.audio_path, error = NULL`, id, recordedAt, audioPath)
	return err
}

func (s *Store) FailRecording(ctx context.Context, id, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recordings SET error = ? WHERE id = ?`, msg, id)
	return err
}

func (s *Store) FinishRecording(ctx context.Context, id, transcript string, entries []Entry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	if _, err := tx.ExecContext(ctx, `UPDATE recordings SET transcript = ?, error = NULL, processed_at = ? WHERE id = ?`,
		transcript, ts, id); err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx, `INSERT INTO entries (id, recording_id, status, amount, currency, category,
			merchant, payment_method, note, date, source, created_at, updated_at)
			VALUES (?, ?, 'review', ?, ?, ?, ?, ?, ?, ?, 'voice', ?, ?)`,
			e.ID, id, e.Amount, e.Currency, e.Category, e.Merchant, e.PaymentMethod, e.Note, e.Date, ts, ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PutEntry creates or edits an entry; an edit older than the stored one is ignored (last write wins).
func (s *Store) PutEntry(ctx context.Context, e Entry) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO entries (id, status, amount, currency, category, merchant,
		payment_method, note, date, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'manual', ?, ?)
		ON CONFLICT(id) DO UPDATE SET status = excluded.status, amount = excluded.amount,
			currency = excluded.currency, category = excluded.category, merchant = excluded.merchant,
			payment_method = excluded.payment_method, note = excluded.note, date = excluded.date,
			updated_at = excluded.updated_at, deleted = 0
		WHERE excluded.updated_at >= entries.updated_at`,
		e.ID, e.Status, e.Amount, e.Currency, e.Category, e.Merchant, e.PaymentMethod, e.Note, e.Date,
		e.UpdatedAt, e.UpdatedAt)
	return err
}

func (s *Store) DeleteEntry(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE entries SET deleted = 1, updated_at = ? WHERE id = ?`, now(), id)
	return err
}
