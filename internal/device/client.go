package device

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"opus/internal/inventory"

	_ "modernc.org/sqlite"
)

// Remote is the server a simulated device talks to.
type Remote interface {
	Bootstrap(userID string) (inventory.Catalog, error)
	Accept(userID, deviceID string, in inventory.Submit) (int64, error)
	ActionsAfter(userID string, seq int64) ([]inventory.Action, error)
}

// Song is one song as this device shows it, with pending actions folded in.
type Song struct {
	ID             string
	Hash           string
	Available      bool
	Metadata       json.RawMessage
	Artwork        string
	PlayCount      int
	FinishedCount  int
	SkipCount      int
	ListenedMs     int64
	LastPlayedAtMs *int64
	Liked          bool
}

// Client is one device: its own SQLite file and a durable queue.
type Client struct {
	db     *sql.DB
	user   string
	device string
	remote Remote
}

// Open opens or creates this device's database.
func Open(path, userID, deviceID string, remote Remote) (*Client, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", abs)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	c := &Client{db: db, user: userID, device: deviceID, remote: remote}
	if err := c.configure(); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) Close() error {
	return c.db.Close()
}

func (c *Client) configure() error {
	if _, err := c.db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return err
	}
	if _, err := c.db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return err
	}
	_, err := c.db.Exec(`
		CREATE TABLE IF NOT EXISTS songs (
			id        TEXT PRIMARY KEY,
			hash      TEXT NOT NULL,
			available INTEGER NOT NULL,
			metadata  TEXT NOT NULL,
			artwork   TEXT NOT NULL DEFAULT '',
			analytics TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS cursor_state (
			singleton  INTEGER PRIMARY KEY CHECK (singleton = 1),
			library_id TEXT NOT NULL,
			seq        INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS pending (
			position       INTEGER PRIMARY KEY AUTOINCREMENT,
			action_id      TEXT NOT NULL UNIQUE,
			song_id        TEXT NOT NULL,
			action_type    TEXT NOT NULL,
			occurred_at_ms INTEGER,
			payload        TEXT NOT NULL,
			acked_seq      INTEGER
		);
		CREATE TABLE IF NOT EXISTS liked (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			song_id TEXT NOT NULL UNIQUE
		);
	`)
	return err
}

// Enqueue stores one user action until the server has acknowledged it
// and this device has applied that log row.
func (c *Client) Enqueue(songID, typ string, occurred *int64, payload json.RawMessage) (string, error) {
	if songID == "" {
		return "", errors.New("invalid action")
	}
	canonical, _, err := inventory.CanonicalUserPayload(typ, payload)
	if err != nil {
		return "", err
	}
	payload = canonical
	id, err := newID()
	if err != nil {
		return "", err
	}
	_, err = c.db.Exec(`
		INSERT INTO pending (action_id, song_id, action_type, occurred_at_ms, payload)
		VALUES (?, ?, ?, ?, ?)`, id, songID, typ, occurred, string(payload))
	if err != nil {
		return "", err
	}
	return id, nil
}

// Pending is the number of local actions not yet applied from the log.
func (c *Client) Pending() (int, error) {
	var n int
	err := c.db.QueryRow(`SELECT COUNT(*) FROM pending`).Scan(&n)
	return n, err
}

// Sync pushes the queue, pulls the log, and advances the cursor.
// A cursor past the server bootstraps again and keeps unacknowledged actions.
func (c *Client) Sync() error {
	if c.remote == nil {
		return errors.New("no server")
	}
	if err := c.ensureCursor(); err != nil {
		return err
	}
	if err := c.push(); err != nil {
		return err
	}
	actions, err := c.pull()
	if errors.Is(err, inventory.ErrCursor) {
		if err := c.bootstrap(); err != nil {
			return err
		}
		if err := c.push(); err != nil {
			return err
		}
		if err := c.dropSnapshotPending(); err != nil {
			return err
		}
		actions, err = c.pull()
	}
	if err != nil {
		return err
	}
	return c.absorb(actions)
}

func (c *Client) ensureCursor() error {
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM cursor_state`).Scan(&n); err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	return c.bootstrap()
}

func (c *Client) bootstrap() error {
	cat, err := c.remote.Bootstrap(c.user)
	if err != nil {
		return err
	}
	return c.install(cat)
}

func (c *Client) install(cat inventory.Catalog) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM songs`); err != nil {
		return err
	}
	for _, song := range cat.Songs {
		meta := song.Metadata
		if len(meta) == 0 {
			meta = []byte(`{}`)
		}
		stats := song.Analytics
		if len(stats) == 0 {
			stats = []byte(`{}`)
		}
		avail := 0
		if song.Available {
			avail = 1
		}
		if _, err := tx.Exec(`
			INSERT INTO songs (id, hash, available, metadata, artwork, analytics)
			VALUES (?, ?, ?, ?, ?, ?)`,
			song.ID, song.Hash, avail, string(meta), song.Artwork, string(stats)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM liked`); err != nil {
		return err
	}
	for i := len(cat.Liked) - 1; i >= 0; i-- {
		if _, err := tx.Exec(`INSERT INTO liked (song_id) VALUES (?)`, cat.Liked[i]); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO cursor_state (singleton, library_id, seq) VALUES (1, ?, ?)
		ON CONFLICT (singleton) DO UPDATE SET library_id = excluded.library_id, seq = excluded.seq`,
		cat.LibraryID, cat.Seq); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Client) push() error {
	rows, err := c.db.Query(`
		SELECT action_id, song_id, action_type, occurred_at_ms, payload
		FROM pending ORDER BY position`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type item struct {
		id, song, typ, payload string
		occurred               *int64
	}
	var items []item
	for rows.Next() {
		var it item
		var occurred sql.NullInt64
		if err := rows.Scan(&it.id, &it.song, &it.typ, &occurred, &it.payload); err != nil {
			return err
		}
		if occurred.Valid {
			v := occurred.Int64
			it.occurred = &v
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, it := range items {
		seq, err := c.remote.Accept(c.user, c.device, inventory.Submit{
			ActionID:     it.id,
			SongID:       it.song,
			Type:         it.typ,
			OccurredAtMs: it.occurred,
			Payload:      json.RawMessage(it.payload),
		})
		if err != nil {
			return err
		}
		if _, err := c.db.Exec(`UPDATE pending SET acked_seq = ? WHERE action_id = ?`, seq, it.id); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) pull() ([]inventory.Action, error) {
	seq, err := c.cursor()
	if err != nil {
		return nil, err
	}
	return c.remote.ActionsAfter(c.user, seq)
}

func (c *Client) cursor() (int64, error) {
	var seq int64
	err := c.db.QueryRow(`SELECT seq FROM cursor_state`).Scan(&seq)
	return seq, err
}

func (c *Client) dropSnapshotPending() error {
	seq, err := c.cursor()
	if err != nil {
		return err
	}
	_, err = c.db.Exec(`DELETE FROM pending WHERE acked_seq IS NOT NULL AND acked_seq <= ?`, seq)
	return err
}

func (c *Client) absorb(actions []inventory.Action) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq int64
	if err := tx.QueryRow(`SELECT seq FROM cursor_state`).Scan(&seq); err != nil {
		return err
	}
	for _, action := range actions {
		if action.Seq <= seq {
			continue
		}
		if err := applyAction(tx, action); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM pending WHERE action_id = ?`, action.ID); err != nil {
			return err
		}
		seq = action.Seq
	}
	if _, err := tx.Exec(`UPDATE cursor_state SET seq = ?`, seq); err != nil {
		return err
	}
	return tx.Commit()
}

func applyAction(tx *sql.Tx, action inventory.Action) error {
	switch action.Type {
	case "song_upsert":
		var body struct {
			Hash      string          `json:"hash"`
			Available bool            `json:"available"`
			Metadata  json.RawMessage `json:"metadata"`
			Artwork   string          `json:"artwork"`
		}
		if err := json.Unmarshal(action.Payload, &body); err != nil {
			return err
		}
		if len(body.Metadata) == 0 {
			body.Metadata = []byte(`{}`)
		}
		avail := 0
		if body.Available {
			avail = 1
		}
		res, err := tx.Exec(`
			UPDATE songs SET hash = ?, available = ?, metadata = ?, artwork = ? WHERE id = ?`,
			body.Hash, avail, string(body.Metadata), body.Artwork, action.SongID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		_, err = tx.Exec(`
			INSERT INTO songs (id, hash, available, metadata, artwork, analytics)
			VALUES (?, ?, ?, ?, ?, '{}')`,
			action.SongID, body.Hash, avail, string(body.Metadata), body.Artwork)
		return err
	case "song_unavailable":
		res, err := tx.Exec(`UPDATE songs SET available = 0 WHERE id = ?`, action.SongID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("song %s is not on this device", action.SongID)
		}
		return nil
	case "like":
		_, err := tx.Exec(`INSERT INTO liked (song_id) VALUES (?) ON CONFLICT (song_id) DO NOTHING`, action.SongID)
		return err
	case "unlike":
		_, err := tx.Exec(`DELETE FROM liked WHERE song_id = ?`, action.SongID)
		return err
	case "play", "finish", "skip":
		var raw string
		err := tx.QueryRow(`SELECT analytics FROM songs WHERE id = ?`, action.SongID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("song %s is not on this device", action.SongID)
		}
		if err != nil {
			return err
		}
		next, err := foldStats([]byte(raw), action.Type, action.Payload, action.OccurredAtMs)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE songs SET analytics = ? WHERE id = ?`, string(next), action.SongID)
		return err
	default:
		return fmt.Errorf("unknown action %s", action.Type)
	}
}

// Songs returns the catalog with this device's queue applied on top.
func (c *Client) Songs() ([]Song, error) {
	rows, err := c.db.Query(`SELECT id, hash, available, metadata, artwork, analytics FROM songs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var songs []Song
	for rows.Next() {
		var song Song
		var avail int
		var meta, art, stats string
		if err := rows.Scan(&song.ID, &song.Hash, &avail, &meta, &art, &stats); err != nil {
			return nil, err
		}
		song.Available = avail == 1
		song.Metadata = json.RawMessage(meta)
		song.Artwork = art
		parsed, err := readStats([]byte(stats))
		if err != nil {
			return nil, err
		}
		song.PlayCount = parsed.PlayCount
		song.FinishedCount = parsed.FinishedCount
		song.SkipCount = parsed.SkipCount
		song.ListenedMs = parsed.ListenedMs
		song.LastPlayedAtMs = parsed.LastPlayedAtMs
		songs = append(songs, song)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pending, err := c.db.Query(`SELECT song_id, action_type, occurred_at_ms, payload FROM pending ORDER BY position`)
	if err != nil {
		return nil, err
	}
	defer pending.Close()
	byID := map[string]*Song{}
	for i := range songs {
		byID[songs[i].ID] = &songs[i]
	}
	for pending.Next() {
		var songID, typ, payload string
		var occurred sql.NullInt64
		if err := pending.Scan(&songID, &typ, &occurred, &payload); err != nil {
			return nil, err
		}
		song := byID[songID]
		if song == nil || typ == "like" || typ == "unlike" {
			continue
		}
		var at *int64
		if occurred.Valid {
			v := occurred.Int64
			at = &v
		}
		raw, err := json.Marshal(stats{
			PlayCount:      song.PlayCount,
			FinishedCount:  song.FinishedCount,
			SkipCount:      song.SkipCount,
			ListenedMs:     song.ListenedMs,
			LastPlayedAtMs: song.LastPlayedAtMs,
		})
		if err != nil {
			return nil, err
		}
		next, err := foldStats(raw, typ, json.RawMessage(payload), at)
		if err != nil {
			return nil, err
		}
		parsed, err := readStats(next)
		if err != nil {
			return nil, err
		}
		song.PlayCount = parsed.PlayCount
		song.FinishedCount = parsed.FinishedCount
		song.SkipCount = parsed.SkipCount
		song.ListenedMs = parsed.ListenedMs
		song.LastPlayedAtMs = parsed.LastPlayedAtMs
	}
	if err := pending.Err(); err != nil {
		return nil, err
	}
	pending.Close()
	liked, err := c.Liked()
	if err != nil {
		return nil, err
	}
	for _, id := range liked {
		if song := byID[id]; song != nil {
			song.Liked = true
		}
	}
	return songs, nil
}

// Liked is the liked song ids, most recently liked first. Pending likes
// sit above the stored list in queue order until the log applies them.
func (c *Client) Liked() ([]string, error) {
	rows, err := c.db.Query(`SELECT song_id FROM liked ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var liked []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		liked = append(liked, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	pending, err := c.db.Query(`
		SELECT song_id, action_type FROM pending
		WHERE action_type IN ('like', 'unlike') ORDER BY position`)
	if err != nil {
		return nil, err
	}
	defer pending.Close()
	for pending.Next() {
		var id, typ string
		if err := pending.Scan(&id, &typ); err != nil {
			return nil, err
		}
		has := false
		for _, existing := range liked {
			if existing == id {
				has = true
				break
			}
		}
		switch {
		case typ == "like" && !has:
			liked = append([]string{id}, liked...)
		case typ == "unlike" && has:
			kept := liked[:0]
			for _, existing := range liked {
				if existing != id {
					kept = append(kept, existing)
				}
			}
			liked = kept
		}
	}
	return liked, pending.Err()
}

type stats struct {
	PlayCount      int    `json:"playCount"`
	FinishedCount  int    `json:"finishedCount"`
	SkipCount      int    `json:"skipCount"`
	ListenedMs     int64  `json:"listenedMs"`
	LastPlayedAtMs *int64 `json:"lastPlayedAtMs"`
}

func readStats(raw []byte) (stats, error) {
	if len(raw) == 0 || string(raw) == "{}" {
		return stats{}, nil
	}
	var s stats
	err := json.Unmarshal(raw, &s)
	return s, err
}

func foldStats(raw []byte, typ string, payload json.RawMessage, occurred *int64) ([]byte, error) {
	s, err := readStats(raw)
	if err != nil {
		return nil, err
	}
	switch typ {
	case "play":
		var body struct {
			ListenedMs int64 `json:"listenedMs"`
		}
		if err := json.Unmarshal(payload, &body); err != nil || body.ListenedMs <= inventory.MinPlayMs {
			return nil, fmt.Errorf("invalid play")
		}
		s.PlayCount++
		s.ListenedMs += body.ListenedMs
		if occurred != nil {
			v := *occurred
			s.LastPlayedAtMs = &v
		}
	case "finish":
		s.FinishedCount++
	case "skip":
		s.SkipCount++
	default:
		return nil, fmt.Errorf("unknown action %s", typ)
	}
	return json.Marshal(s)
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
