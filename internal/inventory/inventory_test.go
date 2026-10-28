package inventory

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func apply(t *testing.T, s *Store, root string, files ...Observation) {
	t.Helper()
	if err := s.Apply(root, files); err != nil {
		t.Fatal(err)
	}
}

func mustSnapshot(t *testing.T, s *Store) Snapshot {
	t.Helper()
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func findAsset(snap Snapshot, contentID string) (Asset, bool) {
	for _, asset := range snap.Assets {
		if asset.ContentID == contentID {
			return asset, true
		}
	}
	return Asset{}, false
}

func locationState(asset Asset) []string {
	out := make([]string, 0, len(asset.Locations))
	for _, loc := range asset.Locations {
		state := "missing"
		if loc.Present {
			state = "present"
		}
		out = append(out, state+" "+loc.Path)
	}
	return out
}

func TestInitialImportAssignsStableIDs(t *testing.T) {
	s := newStore(t)
	file := Observation{Path: "albums/one", ContentID: "alpha", Size: 5}
	apply(t, s, "/music", file)
	first, ok := findAsset(mustSnapshot(t, s), "alpha")
	if !ok {
		t.Fatal("missing asset")
	}
	apply(t, s, "/music", file)
	second, ok := findAsset(mustSnapshot(t, s), "alpha")
	if !ok {
		t.Fatal("missing asset after rescan")
	}
	if first.ID != second.ID {
		t.Fatalf("id changed from %s to %s", first.ID, second.ID)
	}
}

func TestRescanAddsNothing(t *testing.T) {
	s := newStore(t)
	files := []Observation{
		{Path: "a", ContentID: "alpha", Size: 1},
		{Path: "b", ContentID: "beta", Size: 2},
	}
	apply(t, s, "/music", files...)
	before := mustSnapshot(t, s)
	apply(t, s, "/music", files...)
	after := mustSnapshot(t, s)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("rescan changed inventory\nbefore %#v\nafter %#v", before, after)
	}
}

func TestRenameIsQuiet(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "old/song", ContentID: "alpha", Size: 4})
	before, _ := findAsset(mustSnapshot(t, s), "alpha")
	apply(t, s, "/music", Observation{Path: "new/song", ContentID: "alpha", Size: 4})
	snap := mustSnapshot(t, s)
	if len(snap.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(snap.Assets))
	}
	got, _ := findAsset(snap, "alpha")
	if got.ID != before.ID {
		t.Fatalf("id changed from %s to %s", before.ID, got.ID)
	}
	want := []string{"present new/song"}
	if !reflect.DeepEqual(locationState(got), want) {
		t.Fatalf("locations = %v, want %v", locationState(got), want)
	}
}

func TestIdenticalCopiesShareOneAsset(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music",
		Observation{Path: "copy-a", ContentID: "alpha", Size: 3},
		Observation{Path: "copy-b", ContentID: "alpha", Size: 3},
	)
	snap := mustSnapshot(t, s)
	if len(snap.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(snap.Assets))
	}
	want := []string{"present copy-a", "present copy-b"}
	if !reflect.DeepEqual(locationState(snap.Assets[0]), want) {
		t.Fatalf("locations = %v, want %v", locationState(snap.Assets[0]), want)
	}
}

func TestMoveAmongCopiesIsQuiet(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music",
		Observation{Path: "a", ContentID: "alpha", Size: 1},
		Observation{Path: "b", ContentID: "alpha", Size: 1},
	)
	id := mustSnapshot(t, s).Assets[0].ID
	apply(t, s, "/music",
		Observation{Path: "b", ContentID: "alpha", Size: 1},
		Observation{Path: "c", ContentID: "alpha", Size: 1},
	)
	got := mustSnapshot(t, s).Assets[0]
	if got.ID != id {
		t.Fatal("asset id changed")
	}
	want := []string{"present b", "present c"}
	if !reflect.DeepEqual(locationState(got), want) {
		t.Fatalf("locations = %v, want %v", locationState(got), want)
	}
}

func TestOneCopyRemovedMarksOnlyThatPath(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music",
		Observation{Path: "keep", ContentID: "alpha", Size: 1},
		Observation{Path: "drop", ContentID: "alpha", Size: 1},
	)
	id := mustSnapshot(t, s).Assets[0].ID
	apply(t, s, "/music", Observation{Path: "keep", ContentID: "alpha", Size: 1})
	got := mustSnapshot(t, s).Assets[0]
	if got.ID != id {
		t.Fatal("asset id changed")
	}
	want := []string{"present keep", "missing drop"}
	if !reflect.DeepEqual(locationState(got), want) {
		t.Fatalf("locations = %v, want %v", locationState(got), want)
	}
}

func TestMissingFileKeepsHistory(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "only", ContentID: "alpha", Size: 2})
	id := mustSnapshot(t, s).Assets[0].ID
	apply(t, s, "/music")
	snap := mustSnapshot(t, s)
	if len(snap.Assets) != 1 || snap.Assets[0].ID != id {
		t.Fatalf("asset was not kept: %#v", snap.Assets)
	}
	want := []string{"missing only"}
	if !reflect.DeepEqual(locationState(snap.Assets[0]), want) {
		t.Fatalf("locations = %v, want %v", locationState(snap.Assets[0]), want)
	}
}

func TestRetagAtSamePathKeepsIdentity(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "song", ContentID: "alpha", Size: 2})
	id := mustSnapshot(t, s).Assets[0].ID
	apply(t, s, "/music", Observation{Path: "song", ContentID: "beta", Size: 3})
	snap := mustSnapshot(t, s)
	if len(snap.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(snap.Assets))
	}
	got, ok := findAsset(snap, "beta")
	if !ok || got.ID != id || got.Size != 3 {
		t.Fatalf("asset = %+v, id %s", got, id)
	}
	if !reflect.DeepEqual(locationState(got), []string{"present song"}) {
		t.Fatalf("locations = %v", locationState(got))
	}
	if _, ok := findAsset(snap, "alpha"); ok {
		t.Fatal("old hash still present")
	}
}

func TestMoveAndRetagIsANewSong(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "old", ContentID: "alpha", Size: 2})
	oldID := mustSnapshot(t, s).Assets[0].ID
	apply(t, s, "/music", Observation{Path: "new", ContentID: "beta", Size: 3})
	snap := mustSnapshot(t, s)
	if len(snap.Assets) != 2 {
		t.Fatalf("assets = %d, want 2", len(snap.Assets))
	}
	old, ok := findAsset(snap, "alpha")
	if !ok || old.ID != oldID {
		t.Fatal("old song was not kept")
	}
	neu, ok := findAsset(snap, "beta")
	if !ok || neu.ID == old.ID {
		t.Fatal("moved retag did not become its own song")
	}
	if !reflect.DeepEqual(locationState(old), []string{"missing old"}) {
		t.Fatalf("old locations = %v", locationState(old))
	}
	if !reflect.DeepEqual(locationState(neu), []string{"present new"}) {
		t.Fatalf("new locations = %v", locationState(neu))
	}
}

func TestRetagOneCopySplitsThatPath(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music",
		Observation{Path: "copy-a", ContentID: "alpha", Size: 2},
		Observation{Path: "copy-b", ContentID: "alpha", Size: 2},
	)
	id := mustSnapshot(t, s).Assets[0].ID
	apply(t, s, "/music",
		Observation{Path: "copy-a", ContentID: "beta", Size: 4},
		Observation{Path: "copy-b", ContentID: "alpha", Size: 2},
	)
	snap := mustSnapshot(t, s)
	kept, ok := findAsset(snap, "alpha")
	if !ok || kept.ID != id {
		t.Fatal("untouched copy lost its song")
	}
	if !reflect.DeepEqual(locationState(kept), []string{"present copy-b", "missing copy-a"}) {
		t.Fatalf("kept locations = %v", locationState(kept))
	}
	split, ok := findAsset(snap, "beta")
	if !ok || split.ID == kept.ID {
		t.Fatal("retagged copy did not split")
	}
	if !reflect.DeepEqual(locationState(split), []string{"present copy-a"}) {
		t.Fatalf("split locations = %v", locationState(split))
	}
}

func TestReplaceWithExistingBytesFollowsThatSong(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music",
		Observation{Path: "a", ContentID: "alpha", Size: 1},
		Observation{Path: "b", ContentID: "beta", Size: 2},
	)
	before := mustSnapshot(t, s)
	alpha, _ := findAsset(before, "alpha")
	beta, _ := findAsset(before, "beta")
	apply(t, s, "/music",
		Observation{Path: "a", ContentID: "beta", Size: 2},
		Observation{Path: "b", ContentID: "beta", Size: 2},
	)
	snap := mustSnapshot(t, s)
	gotAlpha, ok := findAsset(snap, "alpha")
	if !ok || gotAlpha.ID != alpha.ID {
		t.Fatal("replaced song was not kept")
	}
	if !reflect.DeepEqual(locationState(gotAlpha), []string{"missing a"}) {
		t.Fatalf("alpha locations = %v", locationState(gotAlpha))
	}
	gotBeta, ok := findAsset(snap, "beta")
	if !ok || gotBeta.ID != beta.ID {
		t.Fatal("existing bytes lost their song")
	}
	if !reflect.DeepEqual(locationState(gotBeta), []string{"present a", "present b"}) {
		t.Fatalf("beta locations = %v", locationState(gotBeta))
	}
}

func TestTwoSongsBecomingIdenticalStayMissing(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music",
		Observation{Path: "a", ContentID: "alpha", Size: 1},
		Observation{Path: "b", ContentID: "beta", Size: 2},
	)
	before := mustSnapshot(t, s)
	alpha, _ := findAsset(before, "alpha")
	beta, _ := findAsset(before, "beta")
	apply(t, s, "/music",
		Observation{Path: "a", ContentID: "gamma", Size: 3},
		Observation{Path: "b", ContentID: "gamma", Size: 3},
	)
	snap := mustSnapshot(t, s)
	if len(snap.Assets) != 3 {
		t.Fatalf("assets = %d, want 3", len(snap.Assets))
	}
	gotAlpha, _ := findAsset(snap, "alpha")
	gotBeta, _ := findAsset(snap, "beta")
	gotGamma, _ := findAsset(snap, "gamma")
	if gotAlpha.ID != alpha.ID || gotBeta.ID != beta.ID || gotGamma.ID == alpha.ID || gotGamma.ID == beta.ID {
		t.Fatal("histories were not kept on the old songs")
	}
	if !reflect.DeepEqual(locationState(gotAlpha), []string{"missing a"}) {
		t.Fatalf("alpha locations = %v", locationState(gotAlpha))
	}
	if !reflect.DeepEqual(locationState(gotBeta), []string{"missing b"}) {
		t.Fatalf("beta locations = %v", locationState(gotBeta))
	}
	if !reflect.DeepEqual(locationState(gotGamma), []string{"present a", "present b"}) {
		t.Fatalf("gamma locations = %v", locationState(gotGamma))
	}
}

func TestSwappedFilesFollowTheirBytes(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music",
		Observation{Path: "a", ContentID: "alpha", Size: 1},
		Observation{Path: "b", ContentID: "beta", Size: 1},
	)
	before := mustSnapshot(t, s)
	alpha, _ := findAsset(before, "alpha")
	beta, _ := findAsset(before, "beta")
	apply(t, s, "/music",
		Observation{Path: "b", ContentID: "alpha", Size: 1},
		Observation{Path: "a", ContentID: "beta", Size: 1},
	)
	after := mustSnapshot(t, s)
	gotAlpha, _ := findAsset(after, "alpha")
	gotBeta, _ := findAsset(after, "beta")
	if gotAlpha.ID != alpha.ID || gotBeta.ID != beta.ID {
		t.Fatal("ids did not follow the bytes")
	}
	if !reflect.DeepEqual(locationState(gotAlpha), []string{"present b"}) {
		t.Fatalf("alpha locations = %v", locationState(gotAlpha))
	}
	if !reflect.DeepEqual(locationState(gotBeta), []string{"present a"}) {
		t.Fatalf("beta locations = %v", locationState(gotBeta))
	}
}

func TestReappearingElsewhereKeepsTheOldMissingPath(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "old", ContentID: "alpha", Size: 1})
	apply(t, s, "/music")
	apply(t, s, "/music", Observation{Path: "else", ContentID: "alpha", Size: 1})
	got := mustSnapshot(t, s).Assets[0]
	want := []string{"present else", "missing old"}
	if !reflect.DeepEqual(locationState(got), want) {
		t.Fatalf("locations = %v, want %v", locationState(got), want)
	}
}

func TestDifferentFolderIsRefused(t *testing.T) {
	s := newStore(t)
	apply(t, s, "/music", Observation{Path: "a", ContentID: "alpha", Size: 1})
	before := mustSnapshot(t, s)
	err := s.Apply("/other", []Observation{{Path: "a", ContentID: "alpha", Size: 1}})
	if !errors.Is(err, ErrDifferentRoot) {
		t.Fatalf("err = %v, want ErrDifferentRoot", err)
	}
	after := mustSnapshot(t, s)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refused scan changed the inventory")
	}
}

func TestFailedScanLeavesInventoryUnchanged(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	before := mustSnapshot(t, s)
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("hello!"), 0o644); err != nil {
		t.Fatal(err)
	}

	open := func(name string) (io.ReadCloser, error) {
		return nil, errors.New("injected read failure")
	}
	err := scan(root, s, s.skipDB, open, nil)
	if err == nil {
		t.Fatal("expected scan to fail")
	}
	after := mustSnapshot(t, s)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("failed scan changed inventory\nbefore %#v\nafter %#v", before, after)
	}
}

func TestUnchangedFileIsNotReread(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	var opens int
	open := func(name string) (io.ReadCloser, error) {
		opens++
		return os.Open(name)
	}
	if err := scan(root, s, s.skipDB, open, nil); err != nil {
		t.Fatal(err)
	}
	if opens != 1 {
		t.Fatalf("opens = %d, want 1", opens)
	}
	opens = 0
	if err := scan(root, s, s.skipDB, open, nil); err != nil {
		t.Fatal(err)
	}
	if opens != 0 {
		t.Fatalf("unchanged file opened %d times", opens)
	}
}

func TestTaggedFileIsNotRereadBeforeMtimeIsStored(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE locations SET mtime_ns = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE assets SET metadata = '{"title":"Hello"}'`); err != nil {
		t.Fatal(err)
	}
	var opens int
	open := func(name string) (io.ReadCloser, error) {
		opens++
		return os.Open(name)
	}
	if err := scan(root, s, s.skipDB, open, nil); err != nil {
		t.Fatal(err)
	}
	if opens != 0 {
		t.Fatalf("opened %d times", opens)
	}
	var mtime int64
	if err := s.db.QueryRow(`SELECT mtime_ns FROM locations WHERE present = 1`).Scan(&mtime); err != nil {
		t.Fatal(err)
	}
	if mtime == 0 {
		t.Fatal("mtime was not recorded")
	}
}

func TestScanDoesNotModifySource(t *testing.T) {
	root := t.TempDir()
	full := filepath.Join(root, "song.mp3")
	body := []byte("unchanged")
	if err := os.WriteFile(full, body, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
		t.Fatalf("stat changed from %#v to %#v", before, after)
	}
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("file bytes changed")
	}
}

func TestSymlinkIsNotFollowed(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real.mp3"), []byte("yes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real.mp3"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	snap := mustSnapshot(t, s)
	if len(snap.Assets) != 1 {
		t.Fatalf("assets = %#v, want only real", snap.Assets)
	}
	if !reflect.DeepEqual(locationState(snap.Assets[0]), []string{"present real.mp3"}) {
		t.Fatalf("locations = %v", locationState(snap.Assets[0]))
	}
}

func TestNamedPipeIsSkipped(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	snap := mustSnapshot(t, s)
	if len(snap.Assets) != 1 || !reflect.DeepEqual(locationState(snap.Assets[0]), []string{"present song.mp3"}) {
		t.Fatalf("snapshot = %#v", snap.Assets)
	}
}

func TestDatabaseInsideLibraryIsNotAnAsset(t *testing.T) {
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("track"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	snap := mustSnapshot(t, s)
	if len(snap.Assets) != 1 {
		t.Fatalf("assets = %d, want 1: %#v", len(snap.Assets), snap.Assets)
	}
	if !reflect.DeepEqual(locationState(snap.Assets[0]), []string{"present song.mp3"}) {
		t.Fatalf("locations = %v", locationState(snap.Assets[0]))
	}
}

func TestIgnoredFormatsAreLeftOut(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"song.mp3", "song.flac", "notes.txt", "playlist.m3u", ".DS_Store"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := newStore(t)
	// Empty root skips bindRoot so Scan can bind the symlink-resolved path.
	apply(t, s, "", Observation{Path: "playlist.m3u", ContentID: "list", Size: 1})
	if err := Scan(root, s); err != nil {
		t.Fatal(err)
	}
	snap := mustSnapshot(t, s)
	var paths []string
	for _, asset := range snap.Assets {
		paths = append(paths, locationState(asset)...)
	}
	sort.Strings(paths)
	if !reflect.DeepEqual(paths, []string{"present song.flac", "present song.mp3"}) {
		t.Fatalf("locations = %v", paths)
	}
}

func TestObserveHashesBytes(t *testing.T) {
	root := t.TempDir()
	body := []byte("hello")
	if err := os.WriteFile(filepath.Join(root, "a.mp3"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "b.mp3"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := observe(root, nil, func(name string) (io.ReadCloser, error) {
		return os.Open(name)
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %#v", files)
	}
	if files[0].Path != "a.mp3" || files[1].Path != "nested/b.mp3" {
		t.Fatalf("paths = %#v", files)
	}
	if files[0].ContentID != files[1].ContentID {
		t.Fatal("identical bytes produced different content ids")
	}
	if files[0].Size != int64(len(body)) {
		t.Fatalf("size = %d", files[0].Size)
	}
	sum, n, err := hashReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if files[0].ContentID != sum || n != int64(len(body)) {
		t.Fatalf("hash = %s size %d, file has %s", sum, n, files[0].ContentID)
	}
}
