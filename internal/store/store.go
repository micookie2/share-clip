// Package store persists the clipboard history on the server in a SQLite
// database. Payloads (text or PNG images) are stored as BLOBs; list queries
// avoid loading payloads, only their length and a text preview.
package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
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
// Duplicate filtering: when the item duplicates the most recent entry it is
// ignored; Add then writes no row and reports stored=false. Two items count as
// duplicates when they are the same kind and MIME and carry the same content —
// for text that means byte-identical payloads, for images it means PNGs that
// decode to the same picture, so re-encoding a received image (as a Windows
// CF_DIB round-trip does) cannot smuggle a duplicate past the filter. The
// check and the insert share one transaction, so concurrent clients cannot
// both slip an identical payload past it.
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
	case kind == KindImage && lastKind == KindImage &&
		lastMIME == mime && samePicture(lastPayload, payload):
		return 0, false, nil // re-encoded duplicate of the latest picture, ignored
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

// maxDecodeArea bounds how many pixels store will decode for image duplicate
// detection. It mirrors the guard used when reading clipboard images;
// decoding anything larger just to decide whether a picture repeats would
// cost too much CPU and memory.
const maxDecodeArea = 1 << 26

// samePicture reports whether two PNG payloads depict the same picture, i.e.
// they decode to the same pixels. The encodings may differ arbitrarily (bit
// depth, color type, palette, compression), so re-encoding an image — as a
// Windows CF_DIB round-trip does when it converts a received PNG to a bitmap
// and back — cannot defeat duplicate filtering. A payload that cannot be
// decoded within the size guard never compares equal; callers keep the plain
// byte comparison for that case.
func samePicture(a, b []byte) bool {
	da, oka := pictureID(a)
	if !oka {
		return false
	}
	db, okb := pictureID(b)
	return okb && da == db
}

// pictureID hashes the decoded pixels of a PNG into a canonical digest. Both
// payloads of a comparison are decoded with the same code, so two encodings
// of one picture yield one digest regardless of how they were encoded.
func pictureID(payload []byte) (id [32]byte, ok bool) {
	cfg, err := png.DecodeConfig(bytes.NewReader(payload))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width*cfg.Height > maxDecodeArea {
		return id, false
	}
	img, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		return id, false
	}
	b := img.Bounds()
	h := sha256.New()
	var dim [8]byte
	binary.BigEndian.PutUint32(dim[0:4], uint32(b.Dx()))
	binary.BigEndian.PutUint32(dim[4:8], uint32(b.Dy()))
	h.Write(dim[:])

	// Every 8-bit opaque picture decodes to an *image.NRGBA whose pixel bytes
	// are already the canonical form; hash them in bulk.
	if n, isNRGBA := img.(*image.NRGBA); isNRGBA && opaqueNRGBA(n) {
		h.Write(n.Pix)
		copy(id[:], h.Sum(nil))
		return id, true
	}

	// Paletted, grayscale, 16-bit or transparent pictures go pixel by pixel
	// through the NRGBA model, which is the format PNG decoders everywhere
	// agree on (non-premultiplied 8-bit color).
	var px [4]byte
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			px[0], px[1], px[2], px[3] = c.R, c.G, c.B, c.A
			h.Write(px[:])
		}
	}
	copy(id[:], h.Sum(nil))
	return id, true
}

// opaqueNRGBA reports whether every pixel of an NRGBA image has alpha 255.
func opaqueNRGBA(m *image.NRGBA) bool {
	for i := 3; i < len(m.Pix); i += 4 {
		if m.Pix[i] != 255 {
			return false
		}
	}
	return true
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
