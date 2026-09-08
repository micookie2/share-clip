// Package store persists the clipboard history on the server in a SQLite
// database. Payloads (text or PNG images) are stored as BLOBs; list queries
// avoid loading payloads, only their length and a text preview.
package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no cgo)
)

// Kind values for history entries.
const (
	KindText  = "text"
	KindImage = "image"
)

// Entry describes one history item without its payload.
type Entry struct {
	ID          int64
	Kind        string
	MIME        string
	Size        int
	SourceID    string
	SourceName  string
	CreatedAt   time.Time
	TextPreview string // first bytes of text payloads; empty for images
}

// Store is a SQLite backed history store.
type Store struct {
	db    *sql.DB
	limit int
}

// Open opens (creating if needed) the SQLite database at path and keeps at
// most limit entries, pruning the oldest whenever the limit is exceeded.
func Open(path string, historyLimit int) (*Store, error) {
	if historyLimit <= 0 {
		historyLimit = 500
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// A single connection serializes access; SQLite in this role is a local
	// single-writer store and this avoids busy/race noise entirely.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: pragma: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: pragma: %w", err)
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS history (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	kind        TEXT    NOT NULL,
	mime        TEXT    NOT NULL,
	payload     BLOB    NOT NULL,
	source_id   TEXT    NOT NULL DEFAULT '',
	source_name TEXT    NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL
)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: create table: %w", err)
	}
	if _, err := db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_history_created ON history (id DESC)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: create index: %w", err)
	}
	return &Store{db: db, limit: historyLimit}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Add stores a clipboard item and prunes entries beyond the retention limit.
// Duplicate filtering: when the item is byte-identical to the most recent
// entry (same kind, MIME type and payload) it is ignored; Add then writes no
// row and reports stored=false. The check and the insert share one
// transaction, so concurrent clients cannot both slip an identical payload
// past it.
func (s *Store) Add(kind, mime, sourceID, sourceName string, payload []byte) (int64, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	var lastKind, lastMIME string
	var lastPayload []byte
	err = tx.QueryRow(
		`SELECT kind, mime, payload FROM history ORDER BY id DESC LIMIT 1`).
		Scan(&lastKind, &lastMIME, &lastPayload)
	switch {
	case errors.Is(err, sql.ErrNoRows): // empty store: nothing to duplicate
	case err != nil:
		return 0, false, fmt.Errorf("store: last entry: %w", err)
	case lastKind == kind && lastMIME == mime && bytes.Equal(lastPayload, payload):
		return 0, false, nil // duplicate of the latest entry, ignored
	}

	res, err := tx.Exec(
		`INSERT INTO history (kind, mime, payload, source_id, source_name, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		kind, mime, payload, sourceID, sourceName, time.Now().UnixMilli())
	if err != nil {
		return 0, false, fmt.Errorf("store: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("store: last id: %w", err)
	}
	if _, err := tx.Exec(
		`DELETE FROM history WHERE id NOT IN (
		   SELECT id FROM history ORDER BY id DESC LIMIT ?)`, s.limit); err != nil {
		return 0, false, fmt.Errorf("store: prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("store: commit: %w", err)
	}
	return id, true, nil
}

// Recent returns up to limit entries, newest first, starting at offset.
func (s *Store) Recent(limit, offset int) ([]Entry, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.Query(`
SELECT id, kind, mime, length(payload), source_id, source_name, created_at,
       CASE WHEN kind = 'text' THEN substr(payload, 1, 500) ELSE '' END
FROM history ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: recent: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var ms int64
		if err := rows.Scan(&e.ID, &e.Kind, &e.MIME, &e.Size, &e.SourceID,
			&e.SourceName, &ms, &e.TextPreview); err != nil {
			return nil, fmt.Errorf("store: scan: %w", err)
		}
		e.CreatedAt = time.UnixMilli(ms).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// Content returns the full payload of one entry.
func (s *Store) Content(id int64) (Entry, []byte, error) {
	var e Entry
	var ms int64
	var payload []byte
	err := s.db.QueryRow(`
SELECT id, kind, mime, length(payload), source_id, source_name, created_at, payload
FROM history WHERE id = ?`, id).
		Scan(&e.ID, &e.Kind, &e.MIME, &e.Size, &e.SourceID, &e.SourceName, &ms, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, nil, ErrNotFound
	}
	if err != nil {
		return Entry{}, nil, fmt.Errorf("store: content: %w", err)
	}
	e.CreatedAt = time.UnixMilli(ms).UTC()
	return e, payload, nil
}

// Count returns the number of stored entries.
func (s *Store) Count() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count: %w", err)
	}
	return n, nil
}

// Clear removes every history entry.
func (s *Store) Clear() error {
	if _, err := s.db.Exec(`DELETE FROM history`); err != nil {
		return fmt.Errorf("store: clear: %w", err)
	}
	return nil
}

// ErrNotFound is returned when an entry id does not exist.
var ErrNotFound = errors.New("store: entry not found")
