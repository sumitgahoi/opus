package inventory

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// ErrDifferentRoot means this database already inventories another folder.
var ErrDifferentRoot = errors.New("inventory database belongs to a different folder")

// ErrSongNotFound means no song has that id.
var ErrSongNotFound = errors.New("song not found")

// ErrSongUnavailable means the song is known and every copy is missing.
var ErrSongUnavailable = errors.New("song unavailable")

// ErrArtworkNotFound means no song carries that cover, or no present copy
// can supply the bytes.
var ErrArtworkNotFound = errors.New("artwork not found")

// Asset is one set of file bytes, found at one or more paths.
type Asset struct {
	ID        string     `json:"id"`
	ContentID string     `json:"contentId"`
	Size      int64      `json:"size"`
	Locations []Location `json:"locations"`
}

// Location is a path where an asset is or used to be.
type Location struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
}

// Snapshot is the whole inventory, ready to show.
type Snapshot struct {
	Assets []Asset
}

// Store is the SQLite inventory. One store belongs to one music folder.
type Store struct {
	db   *sql.DB
	path string

	artMu   sync.Mutex
	artLock map[string]*sync.Mutex

	// now is the server clock for received_at_ms. Tests may replace it.
	now func() int64
	// afterSnapshot runs inside Bootstrap after the read that fixes
	// the snapshot. It lets a test commit on another connection
	// without sleeping. Production leaves it nil.
	afterSnapshot func()
}

// Open opens or creates the inventory database at path.
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", abs)
	if err != nil {
		return nil, err
	}
	// One connection keeps the foreign_keys pragma in force and
	// avoids SQLite being used from two connections at once.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: abs}
	if err := s.configure(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) configure() error {
	if _, err := s.db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return err
	}
	var journal string
	if err := s.db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journal); err != nil {
		return err
	}
	var fk int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		return err
	}
	if fk != 1 {
		return errors.New("sqlite foreign keys are off")
	}
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	if _, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES ('schema_version', '1') ON CONFLICT (key) DO NOTHING`); err != nil {
		return err
	}
	var version string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		return err
	}
	if version != "1" {
		return fmt.Errorf("unsupported inventory schema %s", version)
	}
	return s.ensureLibraryID()
}

func (s *Store) ensureLibraryID() error {
	var id string
	err := s.db.QueryRow(`SELECT library_id FROM library_state WHERE singleton_id = 1`).Scan(&id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	id, err = newAssetID()
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO library_state (singleton_id, library_id) VALUES (1, ?)`, id)
	return err
}

func (s *Store) Close() error {
	return s.db.Close()
}

// DB is the open SQLite handle. Auth sessions use the same file.
func (s *Store) DB() *sql.DB {
	return s.db
}

// Ping reports whether the inventory database answers.
func (s *Store) Ping() error {
	return s.db.Ping()
}

// BoundRoot is the music folder stored with this inventory.
// An empty result means the inventory has never been scanned.
func (s *Store) BoundRoot() (string, error) {
	var root string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'root'`).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return root, err
}

func (s *Store) skipDB(full string) bool {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		candidate := s.path + suffix
		if full == candidate {
			return true
		}
		a, err1 := os.Stat(full)
		b, err2 := os.Stat(candidate)
		if err1 == nil && err2 == nil && os.SameFile(a, b) {
			return true
		}
	}
	return false
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS assets (
	id           TEXT PRIMARY KEY,
	content_id   TEXT NOT NULL UNIQUE,
	size         INTEGER NOT NULL,
	metadata     TEXT NOT NULL DEFAULT '{}',
	artwork_hash  TEXT,
	artwork_mime  TEXT,
	store_checked INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS locations (
	id       INTEGER PRIMARY KEY,
	asset_id TEXT NOT NULL REFERENCES assets(id),
	path     TEXT NOT NULL,
	present  INTEGER NOT NULL CHECK (present IN (0, 1)),
	mtime_ns INTEGER NOT NULL DEFAULT 0,
	UNIQUE (asset_id, path)
);

CREATE UNIQUE INDEX IF NOT EXISTS locations_one_present_path
	ON locations(path) WHERE present = 1;

CREATE INDEX IF NOT EXISTS assets_artwork_hash ON assets(artwork_hash);

CREATE TABLE IF NOT EXISTS library_state (
	singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
	library_id   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS actions (
	seq            INTEGER PRIMARY KEY,
	action_id      TEXT NOT NULL UNIQUE,
	user_id        TEXT,
	device_id      TEXT,
	song_id        TEXT NOT NULL REFERENCES assets(id),
	action_type    TEXT NOT NULL,
	occurred_at_ms INTEGER,
	received_at_ms INTEGER NOT NULL,
	payload        BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS analytics (
	user_id          TEXT NOT NULL,
	song_id          TEXT NOT NULL REFERENCES assets(id),
	play_count       INTEGER NOT NULL DEFAULT 0,
	finished_count   INTEGER NOT NULL DEFAULT 0,
	skip_count       INTEGER NOT NULL DEFAULT 0,
	listened_ms      INTEGER NOT NULL DEFAULT 0,
	last_played_at_ms INTEGER,
	PRIMARY KEY (user_id, song_id)
);

CREATE TABLE IF NOT EXISTS liked (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id TEXT NOT NULL,
	song_id TEXT NOT NULL REFERENCES assets(id),
	UNIQUE (user_id, song_id)
);

CREATE TABLE IF NOT EXISTS sessions (
	token         TEXT PRIMARY KEY,
	user_id       TEXT NOT NULL,
	expires_at_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS sessions_expires ON sessions(expires_at_ms);
`

type assetRec struct {
	id           string
	contentID    string
	size         int64
	metadata     string
	artworkKnown bool
	artworkHash  string
	artworkMIME  string
	storeChecked bool
}

type assetPlan struct {
	assetID string
	rec     reconciliation
}

// Apply stores one finished view of the folder.
// root is the cleaned music folder. An empty root skips the folder check,
// which tests use when they supply observations directly.
// The update commits only after every asset is reconciled, so a failed
// scan cannot mark files missing halfway through.
func (s *Store) Apply(root string, files []Observation) error {
	if err := validate(files); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	before, err := loadDeviceViews(tx)
	if err != nil {
		return err
	}
	if err := bindRoot(tx, root); err != nil {
		return err
	}
	if err := dropIgnored(tx, files); err != nil {
		return err
	}
	byContent, err := loadAssets(tx)
	if err != nil {
		return err
	}
	locs, err := loadLocations(tx)
	if err != nil {
		return err
	}

	// Hash matches claim their songs first, which is how a move relinks.
	// A song whose hash is gone then keeps its id when exactly one of its
	// paths holds a new hash. A move plus a byte change has no such path,
	// so that song stays missing and the new path becomes a new song.
	if err := adoptRetags(tx, byContent, locs, files); err != nil {
		return err
	}

	seen := map[string][]string{}
	for _, file := range files {
		asset, ok := byContent[file.ContentID]
		if !ok {
			id, err := newAssetID()
			if err != nil {
				return err
			}
			meta := file.meta()
			hashVal, mimeVal, checkedVal := artworkValues(file)
			_, err = tx.Exec(`INSERT INTO assets (id, content_id, size, metadata, artwork_hash, artwork_mime, store_checked) VALUES (?, ?, ?, ?, ?, ?, ?)`, id, file.ContentID, file.Size, meta, hashVal, mimeVal, checkedVal)
			if err != nil {
				return err
			}
			asset = assetRec{id: id, contentID: file.ContentID, size: file.Size, metadata: meta}
			applyArtwork(&asset, file)
			byContent[file.ContentID] = asset
		} else if asset.size != file.Size {
			return fmt.Errorf("content %s stored size %d, scan saw %d", file.ContentID, asset.size, file.Size)
		} else {
			next, changed, err := mergeMetadata(asset.metadata, file.meta())
			if err != nil {
				return err
			}
			if changed {
				if _, err := tx.Exec(`UPDATE assets SET metadata = ? WHERE id = ?`, next, asset.id); err != nil {
					return err
				}
				asset.metadata = next
			}
			if err := fillArtwork(tx, &asset, file); err != nil {
				return err
			}
			byContent[file.ContentID] = asset
		}
		seen[asset.id] = append(seen[asset.id], file.Path)
	}

	var plans []assetPlan
	for _, asset := range byContent {
		paths := append([]string(nil), seen[asset.id]...)
		sort.Strings(paths)
		plans = append(plans, assetPlan{
			assetID: asset.id,
			rec:     reconcile(locs[asset.id], paths),
		})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].assetID < plans[j].assetID })
	if err := applyPlans(tx, plans); err != nil {
		return err
	}
	if err := updateMtimes(tx, files); err != nil {
		return err
	}
	after, err := loadDeviceViews(tx)
	if err != nil {
		return err
	}
	if err := logDeviceChanges(tx, before, after, s.nowMS()); err != nil {
		return err
	}
	return tx.Commit()
}

// dropIgnored removes stored paths the scanner no longer treats as songs.
// A path in this scan is kept even when a test uses a stand-in name.
// An asset with no locations left is removed when it has no history.
func dropIgnored(tx *sql.Tx, files []Observation) error {
	keep := make(map[string]struct{}, len(files))
	for _, file := range files {
		keep[file.Path] = struct{}{}
	}
	rows, err := tx.Query(`SELECT id, asset_id, path FROM locations`)
	if err != nil {
		return err
	}
	type loc struct {
		id      int64
		assetID string
	}
	var drop []loc
	touched := map[string]struct{}{}
	for rows.Next() {
		var row loc
		var filePath string
		if err := rows.Scan(&row.id, &row.assetID, &filePath); err != nil {
			rows.Close()
			return err
		}
		if _, ok := keep[filePath]; ok || supportedAudio(filePath) || filepath.Ext(filePath) == "" {
			continue
		}
		drop = append(drop, row)
		touched[row.assetID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, row := range drop {
		if _, err := tx.Exec(`DELETE FROM locations WHERE id = ?`, row.id); err != nil {
			return err
		}
	}
	for assetID := range touched {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM locations WHERE asset_id = ?`, assetID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM actions WHERE song_id = ?`, assetID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM analytics WHERE song_id = ?`, assetID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM assets WHERE id = ?`, assetID); err != nil {
			return err
		}
	}
	return nil
}

func updateMtimes(tx *sql.Tx, files []Observation) error {
	for _, file := range files {
		if !file.WriteMtime {
			continue
		}
		if _, err := tx.Exec(`UPDATE locations SET mtime_ns = ? WHERE path = ? AND present = 1 AND mtime_ns != ?`, file.Mtime, file.Path, file.Mtime); err != nil {
			return err
		}
	}
	return nil
}

// adoptRetags updates a song's hash in place for an edit at one of its paths.
// The new hash must not already belong to a song, and only one song may claim it.
func adoptRetags(tx *sql.Tx, byContent map[string]assetRec, locs map[string][]locationRow, files []Observation) error {
	byPath := make(map[string]Observation, len(files))
	seenContent := map[string]struct{}{}
	for _, file := range files {
		byPath[file.Path] = file
		if _, ok := byContent[file.ContentID]; ok {
			seenContent[file.ContentID] = struct{}{}
		}
	}

	type candidate struct {
		asset assetRec
		file  Observation
	}
	grouped := map[string][]candidate{}
	assets := make([]assetRec, 0, len(byContent))
	for _, asset := range byContent {
		assets = append(assets, asset)
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].id < assets[j].id })
	for _, asset := range assets {
		if _, ok := seenContent[asset.contentID]; ok {
			continue
		}
		var hits []Observation
		for _, loc := range locs[asset.id] {
			file, ok := byPath[loc.path]
			if !ok || file.ContentID == asset.contentID {
				continue
			}
			if _, exists := byContent[file.ContentID]; exists {
				continue
			}
			hits = append(hits, file)
		}
		if len(hits) != 1 {
			continue
		}
		file := hits[0]
		grouped[file.ContentID] = append(grouped[file.ContentID], candidate{asset: asset, file: file})
	}

	ids := make([]string, 0, len(grouped))
	for id := range grouped {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		cands := grouped[id]
		if len(cands) != 1 {
			continue
		}
		cand := cands[0]
		meta := cand.file.meta()
		hashVal, mimeVal, checkedVal := artworkValues(cand.file)
		_, err := tx.Exec(`UPDATE assets SET content_id = ?, size = ?, metadata = ?, artwork_hash = ?, artwork_mime = ?, store_checked = ? WHERE id = ?`, cand.file.ContentID, cand.file.Size, meta, hashVal, mimeVal, checkedVal, cand.asset.id)
		if err != nil {
			return err
		}
		delete(byContent, cand.asset.contentID)
		cand.asset.contentID = cand.file.ContentID
		cand.asset.size = cand.file.Size
		cand.asset.metadata = meta
		applyArtwork(&cand.asset, cand.file)
		byContent[cand.file.ContentID] = cand.asset
	}
	return nil
}

func bindRoot(tx *sql.Tx, root string) error {
	if root == "" {
		return nil
	}
	var have string
	err := tx.QueryRow(`SELECT value FROM meta WHERE key = 'root'`).Scan(&have)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.Exec(`INSERT INTO meta (key, value) VALUES ('root', ?)`, root)
		return err
	}
	if err != nil {
		return err
	}
	if have != root {
		return fmt.Errorf("%w: database has %s", ErrDifferentRoot, have)
	}
	return nil
}

func loadAssets(tx *sql.Tx) (map[string]assetRec, error) {
	rows, err := tx.Query(`SELECT id, content_id, size, metadata, artwork_hash, artwork_mime, store_checked FROM assets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]assetRec{}
	for rows.Next() {
		var rec assetRec
		var artworkHash, artworkMIME sql.NullString
		var storeChecked int
		if err := rows.Scan(&rec.id, &rec.contentID, &rec.size, &rec.metadata, &artworkHash, &artworkMIME, &storeChecked); err != nil {
			return nil, err
		}
		rec.metadata = normalizeMetadata(rec.metadata)
		rec.artworkKnown = artworkHash.Valid
		rec.artworkHash = artworkHash.String
		rec.artworkMIME = artworkMIME.String
		rec.storeChecked = storeChecked == 1
		out[rec.contentID] = rec
	}
	return out, rows.Err()
}

// fillArtwork records a cover the first time the bytes are inspected.
// An empty hash may later gain a store cover. A hash already stored for
// these bytes must not change.
func fillArtwork(tx *sql.Tx, asset *assetRec, file Observation) error {
	if !file.ArtworkKnown {
		return nil
	}
	if asset.artworkKnown && asset.artworkHash != "" {
		if asset.artworkHash != file.ArtworkHash || asset.artworkMIME != file.ArtworkMIME {
			return fmt.Errorf("artwork changed for unchanged file bytes")
		}
	}
	if asset.artworkKnown && asset.artworkHash == file.ArtworkHash && asset.artworkMIME == file.ArtworkMIME && asset.storeChecked == file.StoreChecked {
		return nil
	}
	hashVal, mimeVal, checkedVal := artworkValues(file)
	if _, err := tx.Exec(`UPDATE assets SET artwork_hash = ?, artwork_mime = ?, store_checked = ? WHERE id = ?`, hashVal, mimeVal, checkedVal, asset.id); err != nil {
		return err
	}
	applyArtwork(asset, file)
	return nil
}

func applyArtwork(asset *assetRec, file Observation) {
	asset.storeChecked = file.StoreChecked
	if !file.ArtworkKnown {
		asset.artworkKnown = false
		asset.artworkHash = ""
		asset.artworkMIME = ""
		return
	}
	asset.artworkKnown = true
	asset.artworkHash = file.ArtworkHash
	asset.artworkMIME = file.ArtworkMIME
}

func artworkValues(file Observation) (any, any, any) {
	checked := 0
	if file.StoreChecked {
		checked = 1
	}
	if !file.ArtworkKnown {
		return nil, nil, checked
	}
	if file.ArtworkHash == "" {
		return "", nil, checked
	}
	return file.ArtworkHash, file.ArtworkMIME, checked
}

func (file Observation) meta() string {
	return normalizeMetadata(file.Metadata)
}

func normalizeMetadata(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// mergeMetadata keeps stored tags when a later read finds none, and fills
// them in when the row is still {}. Same bytes with two different tag
// documents is an error.
func mergeMetadata(stored, next string) (string, bool, error) {
	stored = normalizeMetadata(stored)
	next = normalizeMetadata(next)
	if stored == next || next == "{}" {
		return stored, false, nil
	}
	if stored == "{}" {
		return next, true, nil
	}
	return "", false, fmt.Errorf("metadata changed for unchanged file bytes")
}

func loadLocations(tx *sql.Tx) (map[string][]locationRow, error) {
	rows, err := tx.Query(`SELECT id, asset_id, path, present FROM locations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]locationRow{}
	for rows.Next() {
		var assetID string
		var rec locationRow
		var present int
		if err := rows.Scan(&rec.id, &assetID, &rec.path, &present); err != nil {
			return nil, err
		}
		rec.present = present == 1
		out[assetID] = append(out[assetID], rec)
	}
	return out, rows.Err()
}

// applyPlans changes locations in phases. Every path that will move is
// marked not present before any row takes its new path, so two assets
// can exchange paths without both looking present there at once.
func applyPlans(tx *sql.Tx, plans []assetPlan) error {
	for _, plan := range plans {
		for _, id := range plan.rec.clear {
			if _, err := tx.Exec(`UPDATE locations SET present = 0 WHERE id = ?`, id); err != nil {
				return err
			}
		}
	}
	for _, plan := range plans {
		for _, id := range plan.rec.remove {
			if _, err := tx.Exec(`DELETE FROM locations WHERE id = ?`, id); err != nil {
				return err
			}
		}
	}
	for _, plan := range plans {
		for _, mv := range plan.rec.move {
			_, err := tx.Exec(`UPDATE locations SET path = ?, present = 1 WHERE id = ?`, mv.path, mv.id)
			if err != nil {
				return err
			}
		}
	}
	for _, plan := range plans {
		for _, id := range plan.rec.revive {
			if _, err := tx.Exec(`UPDATE locations SET present = 1 WHERE id = ?`, id); err != nil {
				return err
			}
		}
	}
	for _, plan := range plans {
		for _, p := range plan.rec.create {
			_, err := tx.Exec(`INSERT INTO locations (asset_id, path, present) VALUES (?, ?, 1)`, plan.assetID, p)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) artworkDir() string {
	return s.path + ".artwork"
}

func (s *Store) artworkPath(hash string) (string, error) {
	if err := validateArtworkHash(hash); err != nil {
		return "", err
	}
	return filepath.Join(s.artworkDir(), hash), nil
}

func (s *Store) writeArtwork(art *coverArt) error {
	if art == nil || art.Hash == "" || len(art.Data) == 0 {
		return nil
	}
	dest, err := s.artworkPath(art.Hash)
	if err != nil {
		return err
	}
	if info, err := os.Stat(dest); err == nil && info.Mode().IsRegular() && info.Size() == int64(len(art.Data)) {
		return nil
	}
	if err := os.MkdirAll(s.artworkDir(), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.artworkDir(), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(art.Data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func (s *Store) lockArtwork(hash string) func() {
	s.artMu.Lock()
	if s.artLock == nil {
		s.artLock = map[string]*sync.Mutex{}
	}
	mu := s.artLock[hash]
	if mu == nil {
		mu = &sync.Mutex{}
		s.artLock[hash] = mu
	}
	s.artMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// OpenArtwork opens the cached cover for hash. A missing file is rebuilt
// from a present copy the same way a scan finds a cover: the embedded
// picture, then the iTunes id. The bytes must hash to hash. The caller
// closes the file.
func (s *Store) OpenArtwork(hash string, open func(rel string) (io.ReadSeekCloser, error)) (*os.File, string, error) {
	return s.openArtwork(hash, open, newStoreCatalog(nil))
}

func (s *Store) openArtwork(hash string, open func(rel string) (io.ReadSeekCloser, error), covers storeCovers) (*os.File, string, error) {
	if err := validateArtworkHash(hash); err != nil {
		return nil, "", ErrArtworkNotFound
	}
	var mime string
	unlock := s.lockArtwork(hash)
	err := s.ensureArtwork(hash, open, covers, &mime)
	unlock()
	if err != nil {
		return nil, "", err
	}
	dest, err := s.artworkPath(hash)
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(dest)
	if err != nil {
		return nil, "", err
	}
	return f, mime, nil
}

func (s *Store) ensureArtwork(hash string, open func(rel string) (io.ReadSeekCloser, error), covers storeCovers, mime *string) error {
	found, storedMIME, paths, err := s.artworkSources(hash)
	if err != nil {
		return err
	}
	if !found {
		return ErrArtworkNotFound
	}
	dest, err := s.artworkPath(hash)
	if err != nil {
		return err
	}
	if info, err := os.Stat(dest); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		*mime = storedMIME
		return nil
	}
	if len(paths) == 0 {
		return ErrArtworkNotFound
	}
	var storeErr error
	replaced := map[string]*coverArt{}
	replaceIDs := map[string][]string{}
	for _, src := range paths {
		match, next, err := coverForHash(open, src.path, hash, covers)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			storeErr = err
			continue
		}
		if match != nil {
			if err := s.writeArtwork(match); err != nil {
				return err
			}
			if storedMIME == "" {
				storedMIME = match.MIME
			}
			*mime = storedMIME
			return nil
		}
		if next != nil {
			replaced[next.Hash] = next
			replaceIDs[next.Hash] = append(replaceIDs[next.Hash], src.id)
		}
	}
	if len(replaced) > 0 {
		// Apple replaced the image. Keep each new image under its own hash
		// and point those songs at it. The requested hash is no longer served.
		for sum, art := range replaced {
			if err := s.writeArtwork(art); err != nil {
				return err
			}
			if err := s.retargetArtwork(hash, art, replaceIDs[sum]); err != nil {
				return err
			}
		}
		return ErrArtworkNotFound
	}
	if storeErr != nil {
		return storeErr
	}
	return ErrArtworkNotFound
}

// coverForHash reads one copy. match is an image whose hash is hash.
// next is a store image that replaced it. An embedded picture with a
// different hash is neither: that file is not the source of this cover.
func coverForHash(open func(rel string) (io.ReadSeekCloser, error), rel, hash string, covers storeCovers) (*coverArt, *coverArt, error) {
	f, err := open(rel)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	art, readErr := readCover(f)
	if readErr != nil {
		log.Printf("artwork %s: %v", hash, readErr)
	} else if art != nil {
		if art.Hash == hash {
			return art, nil, nil
		}
		return nil, nil, nil
	}
	if !storeAudio(rel) || covers == nil {
		return nil, nil, nil
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, nil, err
	}
	ids, err := readStoreIDs(f, size)
	if err != nil {
		log.Printf("artwork %s: %v", hash, err)
		return nil, nil, err
	}
	if ids.Track == 0 && ids.Album == 0 {
		return nil, nil, nil
	}
	got, _, err := covers.Cover(ids.Album, ids.Track)
	if err != nil {
		return nil, nil, err
	}
	if got == nil {
		return nil, nil, nil
	}
	if got.Hash == hash {
		return got, nil, nil
	}
	return nil, got, nil
}

func (s *Store) retargetArtwork(old string, art *coverArt, ids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	received := s.nowMS()
	for _, id := range ids {
		if id == "" {
			continue
		}
		res, err := tx.Exec(`UPDATE assets SET artwork_hash = ?, artwork_mime = ?, store_checked = 1 WHERE id = ? AND artwork_hash = ?`, art.Hash, art.MIME, id, old)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if err := appendSongUpsert(tx, id, received); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type artworkSource struct {
	id   string
	path string
}

func (s *Store) artworkSources(hash string) (bool, string, []artworkSource, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.artwork_mime, l.path
		FROM assets a
		LEFT JOIN locations l ON l.asset_id = a.id AND l.present = 1
		WHERE a.artwork_hash = ?
		ORDER BY l.path`, hash)
	if err != nil {
		return false, "", nil, err
	}
	defer rows.Close()
	var mime string
	var paths []artworkSource
	found := false
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		var stored sql.NullString
		var rel sql.NullString
		if err := rows.Scan(&id, &stored, &rel); err != nil {
			return false, "", nil, err
		}
		found = true
		if mime == "" && stored.Valid {
			mime = stored.String
		}
		if rel.Valid && rel.String != "" && !seen[id+"\x00"+rel.String] {
			seen[id+"\x00"+rel.String] = true
			paths = append(paths, artworkSource{id: id, path: rel.String})
		}
	}
	if err := rows.Err(); err != nil {
		return false, "", nil, err
	}
	return found, mime, paths, nil
}

// PresentPath returns one library-relative path where songID can be read.
// Several present copies share one song; the path that sorts first is used.
func (s *Store) PresentPath(songID string) (string, error) {
	var rel string
	err := s.db.QueryRow(`
		SELECT path FROM locations
		WHERE asset_id = ? AND present = 1
		ORDER BY path
		LIMIT 1`, songID).Scan(&rel)
	if err == nil {
		return rel, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id = ?`, songID).Scan(&n); err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrSongNotFound
	}
	return "", ErrSongUnavailable
}

// Snapshot returns every asset and its locations.
func (s *Store) Snapshot() (Snapshot, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.content_id, a.size, l.path, l.present
		FROM assets a
		LEFT JOIN locations l ON l.asset_id = a.id
		ORDER BY a.id, l.path`)
	if err != nil {
		return Snapshot{}, err
	}
	defer rows.Close()

	var assets []Asset
	index := map[string]int{}
	for rows.Next() {
		var id, contentID string
		var size int64
		var locPath sql.NullString
		var present sql.NullInt64
		if err := rows.Scan(&id, &contentID, &size, &locPath, &present); err != nil {
			return Snapshot{}, err
		}
		i, ok := index[id]
		if !ok {
			i = len(assets)
			index[id] = i
			assets = append(assets, Asset{ID: id, ContentID: contentID, Size: size})
		}
		if locPath.Valid {
			assets[i].Locations = append(assets[i].Locations, Location{
				Path:    locPath.String,
				Present: present.Int64 == 1,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, err
	}
	for i := range assets {
		sort.Slice(assets[i].Locations, func(a, b int) bool {
			if assets[i].Locations[a].Present != assets[i].Locations[b].Present {
				return assets[i].Locations[a].Present
			}
			return assets[i].Locations[a].Path < assets[i].Locations[b].Path
		})
	}
	sort.Slice(assets, func(i, j int) bool {
		return assetKey(assets[i]) < assetKey(assets[j])
	})
	return Snapshot{Assets: assets}, nil
}

func assetKey(a Asset) string {
	for _, loc := range a.Locations {
		if loc.Present {
			return "0\x00" + loc.Path
		}
	}
	if len(a.Locations) > 0 {
		return "1\x00" + a.Locations[0].Path
	}
	return "2\x00" + a.ID
}

func validate(files []Observation) error {
	seen := map[string]struct{}{}
	for _, file := range files {
		if err := validatePath(file.Path); err != nil {
			return err
		}
		if err := validateContentID(file.ContentID); err != nil {
			return err
		}
		if file.Size < 0 {
			return fmt.Errorf("negative size for %s", file.Path)
		}
		if _, ok := seen[file.Path]; ok {
			return fmt.Errorf("duplicate path %s", file.Path)
		}
		seen[file.Path] = struct{}{}
	}
	return nil
}

func validateArtworkHash(id string) error {
	if len(id) != 64 {
		return errors.New("invalid artwork hash")
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return errors.New("invalid artwork hash")
		}
	}
	return nil
}

func validateContentID(id string) error {
	if id == "" || len(id) > 128 {
		return errors.New("invalid content id")
	}
	for _, r := range id {
		if r <= ' ' || r > '~' {
			return errors.New("invalid content id")
		}
	}
	return nil
}

func newAssetID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
