package device

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"opus/internal/inventory"
)

type server struct {
	*inventory.Store
}

func (s server) Bootstrap(userID string) (inventory.Catalog, error) {
	return s.Store.Bootstrap(userID)
}

func (s server) Accept(userID, deviceID string, in inventory.Submit) (int64, error) {
	return s.Store.Accept(userID, deviceID, in)
}

func (s server) ActionsAfter(userID string, seq int64) ([]inventory.Action, error) {
	return s.Store.ActionsAfter(userID, seq)
}

type loseOnce struct {
	Remote
	lost bool
}

func (l *loseOnce) Accept(userID, deviceID string, in inventory.Submit) (int64, error) {
	seq, err := l.Remote.Accept(userID, deviceID, in)
	if err != nil {
		return 0, err
	}
	if !l.lost {
		l.lost = true
		return 0, errors.New("lost response")
	}
	return seq, nil
}

func TestClientsConvergeAndStayIsolated(t *testing.T) {
	store := openInventory(t)
	song := seedSong(t, store)
	ada := openClient(t, store, "ada", "phone-a")
	other := openClient(t, store, "ada", "phone-b")
	bea := openClient(t, store, "bea", "phone-c")
	if err := ada.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := ada.Enqueue(song, "like", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Enqueue(song, "unlike", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := ada.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := other.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := ada.Sync(); err != nil {
		t.Fatal(err)
	}
	if liked(t, ada, song) || liked(t, other, song) {
		t.Fatalf("like did not follow accept order")
	}
	when := int64(20)
	if _, err := ada.Enqueue(song, "play", &when, []byte(`{"listenedMs":30001}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Enqueue(song, "play", &when, []byte(`{"listenedMs":45000}`)); err != nil {
		t.Fatal(err)
	}
	if err := ada.Sync(); err != nil || other.Sync() != nil || ada.Sync() != nil {
		t.Fatal("play sync")
	}
	if plays(t, ada, song) != 2 || plays(t, other, song) != 2 {
		t.Fatalf("plays ada %d other %d", plays(t, ada, song), plays(t, other, song))
	}
	if err := bea.Sync(); err != nil {
		t.Fatal(err)
	}
	if plays(t, bea, song) != 0 || liked(t, bea, song) {
		t.Fatal("second user saw the first user's state")
	}
	if err := store.Apply("/music", []inventory.Observation{
		{Path: "song.mp3", ContentID: "alpha", Size: 1},
		{Path: "next.mp3", ContentID: "beta", Size: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ada.Enqueue(song, "like", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := ada.Sync(); err != nil {
		t.Fatal(err)
	}
	if songByHash(t, ada, "beta") == "" || !liked(t, ada, song) {
		t.Fatal("later song and like did not share the cursor")
	}
}

func TestRetryDoesNotDoubleApply(t *testing.T) {
	store := openInventory(t)
	song := seedSong(t, store)
	remote := &loseOnce{Remote: server{store}}
	phone := openClient(t, store, "ada", "phone")
	phone.remote = remote
	if err := phone.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := phone.Enqueue(song, "play", nil, []byte(`{"listenedMs":30001}`)); err != nil {
		t.Fatal(err)
	}
	if err := phone.Sync(); err == nil {
		t.Fatal("expected the lost response")
	}
	if n, err := phone.Pending(); err != nil || n != 1 {
		t.Fatalf("pending = %d err %v", n, err)
	}
	if err := phone.Sync(); err != nil {
		t.Fatal(err)
	}
	if plays(t, phone, song) != 1 {
		t.Fatalf("plays = %d", plays(t, phone, song))
	}
	if err := phone.Sync(); err != nil {
		t.Fatal(err)
	}
	if plays(t, phone, song) != 1 {
		t.Fatalf("reapply plays = %d", plays(t, phone, song))
	}
	cat, err := store.Bootstrap("ada")
	if err != nil {
		t.Fatal(err)
	}
	var stats struct {
		PlayCount int `json:"playCount"`
	}
	if err := json.Unmarshal(cat.Songs[0].Analytics, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.PlayCount != 1 {
		t.Fatalf("server plays = %d", stats.PlayCount)
	}
}

func TestPendingSurvivesRestartAndRecovery(t *testing.T) {
	store := openInventory(t)
	song := seedSong(t, store)
	dir := t.TempDir()
	path := filepath.Join(dir, "phone.db")
	phone, err := Open(path, "ada", "phone", server{store})
	if err != nil {
		t.Fatal(err)
	}
	if err := phone.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := phone.Enqueue(song, "like", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := phone.Close(); err != nil {
		t.Fatal(err)
	}
	phone, err = Open(path, "ada", "phone", server{store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { phone.Close() })
	if n, err := phone.Pending(); err != nil || n != 1 {
		t.Fatalf("pending = %d err %v", n, err)
	}
	if !liked(t, phone, song) {
		t.Fatal("pending like was not kept")
	}
	if _, err := phone.db.Exec(`UPDATE cursor_state SET seq = 999999`); err != nil {
		t.Fatal(err)
	}
	if err := phone.Sync(); err != nil {
		t.Fatal(err)
	}
	if n, err := phone.Pending(); err != nil || n != 0 {
		t.Fatalf("pending after recovery = %d err %v", n, err)
	}
	if !liked(t, phone, song) || songByHash(t, phone, "alpha") == "" {
		t.Fatal("recovery dropped the song or the like")
	}
}

func TestLikedOrderMatchesTheServerAcrossDevices(t *testing.T) {
	store := openInventory(t)
	if err := store.Apply("/music", []inventory.Observation{
		{Path: "a.mp3", ContentID: "alpha", Size: 1},
		{Path: "b.mp3", ContentID: "beta", Size: 1},
		{Path: "c.mp3", ContentID: "gamma", Size: 1},
	}); err != nil {
		t.Fatal(err)
	}
	phone := openClient(t, store, "ada", "phone")
	web := openClient(t, store, "ada", "web")
	if err := phone.Sync(); err != nil || web.Sync() != nil {
		t.Fatal("first sync")
	}
	alpha, beta, gamma := songByHash(t, phone, "alpha"), songByHash(t, phone, "beta"), songByHash(t, phone, "gamma")

	if _, err := phone.Enqueue(alpha, "like", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := web.Enqueue(beta, "like", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := web.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := phone.Enqueue(gamma, "like", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustLiked(t, phone); !sameIDs(got, gamma, alpha) {
		t.Fatalf("offline phone liked = %v", got)
	}
	if err := phone.Sync(); err != nil || web.Sync() != nil {
		t.Fatal("second sync")
	}
	for name, c := range map[string]*Client{"phone": phone, "web": web} {
		if got := mustLiked(t, c); !sameIDs(got, gamma, alpha, beta) {
			t.Fatalf("%s liked = %v", name, got)
		}
	}
	fresh := openClient(t, store, "ada", "fresh")
	if err := fresh.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := mustLiked(t, fresh); !sameIDs(got, gamma, alpha, beta) {
		t.Fatalf("bootstrap liked = %v", got)
	}
}

func mustLiked(t *testing.T, c *Client) []string {
	t.Helper()
	ids, err := c.Liked()
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func sameIDs(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func openInventory(t *testing.T) *inventory.Store {
	t.Helper()
	s, err := inventory.Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedSong(t *testing.T, s *inventory.Store) string {
	t.Helper()
	if err := s.Apply("/music", []inventory.Observation{{Path: "song.mp3", ContentID: "alpha", Size: 1}}); err != nil {
		t.Fatal(err)
	}
	cat, err := s.Bootstrap("ada")
	if err != nil {
		t.Fatal(err)
	}
	return cat.Songs[0].ID
}

func openClient(t *testing.T, s *inventory.Store, user, deviceID string) *Client {
	t.Helper()
	c, err := Open(filepath.Join(t.TempDir(), deviceID+".db"), user, deviceID, server{s})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func plays(t *testing.T, c *Client, songID string) int {
	t.Helper()
	for _, song := range mustSongs(t, c) {
		if song.ID == songID {
			return song.PlayCount
		}
	}
	t.Fatalf("missing %s", songID)
	return 0
}

func liked(t *testing.T, c *Client, songID string) bool {
	t.Helper()
	for _, song := range mustSongs(t, c) {
		if song.ID == songID {
			return song.Liked
		}
	}
	t.Fatalf("missing %s", songID)
	return false
}

func songByHash(t *testing.T, c *Client, hash string) string {
	t.Helper()
	for _, song := range mustSongs(t, c) {
		if song.Hash == hash {
			return song.ID
		}
	}
	return ""
}

func mustSongs(t *testing.T, c *Client) []Song {
	t.Helper()
	songs, err := c.Songs()
	if err != nil {
		t.Fatal(err)
	}
	return songs
}
