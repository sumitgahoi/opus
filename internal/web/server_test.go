package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opus/internal/auth"
	"opus/internal/inventory"
)

func TestHealth(t *testing.T) {
	store := openStore(t)
	ts, _ := serve(t, t.TempDir(), store)

	res, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s", res.Status)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %s", ct)
	}
	var body healthDocument
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK {
		t.Fatalf("health = %+v", body)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	res, err = http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %s", res.Status)
	}
}

func TestInventoryJSONAndQuietRename(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "old.mp3"), []byte("song"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	ts, handler := serve(t, root, store)

	doc := getInventory(t, ts.URL+"/inventory.json")
	if doc.Root != root || len(doc.Assets) != 1 {
		t.Fatalf("doc root=%s assets=%d", doc.Root, len(doc.Assets))
	}
	id := doc.Assets[0].ID
	if id == "" || doc.Assets[0].ContentID == "" || doc.Assets[0].Size != 4 {
		t.Fatalf("asset = %+v", doc.Assets[0])
	}
	if paths := locationState(doc.Assets[0]); !same(paths, []string{"present old.mp3"}) {
		t.Fatalf("locations = %v", paths)
	}

	if err := os.Rename(filepath.Join(root, "old.mp3"), filepath.Join(root, "moved.mp3")); err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(ts.URL+"/rescan", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s", res.Status)
	}
	if err := handler.waitForScan(); err != nil {
		t.Fatal(err)
	}
	doc = getInventory(t, ts.URL+"/inventory.json")
	if len(doc.Assets) != 1 || doc.Assets[0].ID != id {
		t.Fatalf("rename changed the asset: %+v", doc.Assets)
	}
	if paths := locationState(doc.Assets[0]); !same(paths, []string{"present moved.mp3"}) {
		t.Fatalf("locations after rename = %v", paths)
	}
}

func TestFailedRescanLeavesInventory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("song"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	ts, handler := serve(t, root, store)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	res, err := http.Post(ts.URL+"/rescan", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s", res.Status)
	}
	if err := handler.waitForScan(); err == nil {
		t.Fatal("expected scan to fail")
	}

	doc := getInventory(t, ts.URL+"/inventory.json")
	if len(doc.Assets) != 1 || !same(locationState(doc.Assets[0]), []string{"present song.mp3"}) {
		t.Fatalf("inventory changed: %+v", doc.Assets)
	}
}

func openStore(t *testing.T) *inventory.Store {
	t.Helper()
	store, err := inventory.Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func serve(t *testing.T, root string, store *inventory.Store) (*httptest.Server, *Handler) {
	t.Helper()
	sessions := auth.NewSessions(store.DB())
	svc := auth.New(sessions, auth.Config{
		GoogleClientID: "test-google-client",
		GoogleAllowedEmails: map[string]struct{}{
			"ada@example.com": {},
		},
		AppleClientIDs: map[string]struct{}{"app.musicclient.ios": {}},
		AppleAllowed: map[string]struct{}{
			"ada@example.com": {},
			"ada.sub":         {},
		},
	})
	handler := New(root, store, svc)
	ts := httptest.NewServer(handler.Routes())
	t.Cleanup(ts.Close)
	return ts, handler
}

func testAuth(t *testing.T, store *inventory.Store) *auth.Service {
	t.Helper()
	sessions := auth.NewSessions(store.DB())
	return auth.New(sessions, auth.Config{
		GoogleClientID: "test-google-client",
		GoogleAllowedEmails: map[string]struct{}{
			"ada@example.com": {},
		},
		AppleClientIDs: map[string]struct{}{"app.musicclient.ios": {}},
		AppleAllowed: map[string]struct{}{
			"ada@example.com": {},
			"ada.sub":         {},
		},
	})
}

func bearer(t *testing.T, h *Handler, user string) string {
	t.Helper()
	token, _, err := h.Auth.Sessions.Create(user)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func getInventory(t *testing.T, url string) inventoryDocument {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return decodeInventory(t, res, http.StatusOK)
}

func postInventory(t *testing.T, url string, status int) inventoryDocument {
	t.Helper()
	res, err := http.Post(url, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return decodeInventory(t, res, status)
}

func decodeInventory(t *testing.T, res *http.Response, status int) inventoryDocument {
	t.Helper()
	if res.StatusCode != status {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status %s: %s", res.Status, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %s", ct)
	}
	var doc inventoryDocument
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestSnapshotWhileScanRuns(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("song"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	handler := New(root, store, testAuth(t, store))
	started := make(chan struct{})
	release := make(chan struct{})
	handler.scan = func(string, *inventory.Store) error {
		close(started)
		<-release
		return nil
	}
	ts := httptest.NewServer(handler.Routes())
	t.Cleanup(ts.Close)

	if !handler.trigger() {
		t.Fatal("scan did not start")
	}
	<-started
	if handler.trigger() {
		t.Fatal("second scan started while the first was running")
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/snapshot.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer(t, handler, "ada"))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status %s: %s", res.Status, body)
	}
	close(release)
	if err := handler.waitForScan(); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadSongByID(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Album")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("song-bytes")
	if err := os.WriteFile(filepath.Join(dir, "track.mp3"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "copy.mp3"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	ts, handler := serve(t, root, store)
	doc := getInventory(t, ts.URL+"/inventory.json")
	if len(doc.Assets) != 1 {
		t.Fatalf("assets = %d", len(doc.Assets))
	}
	id := doc.Assets[0].ID
	token := bearer(t, handler, "ada")

	res := getAuthed(t, ts.URL+"/songs/"+id, token)
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s: %s", res.Status, got)
	}
	if string(got) != string(body) {
		t.Fatalf("body = %q", got)
	}
	if ct := res.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("content type %s", ct)
	}
	cd := res.Header.Get("Content-Disposition")
	if !strings.Contains(cd, "track.mp3") || strings.Contains(cd, "Album") {
		t.Fatalf("disposition = %s", cd)
	}
	if strings.Contains(res.Header.Get("Content-Type"), root) {
		t.Fatal("response exposes the library path")
	}

	missing := getAuthed(t, ts.URL+"/songs/no-such-song", token)
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing status %s", missing.Status)
	}
	var missingDoc errorDocument
	if err := json.NewDecoder(missing.Body).Decode(&missingDoc); err != nil {
		t.Fatal(err)
	}
	if missingDoc.Error != "song not found" || strings.Contains(missingDoc.Error, root) {
		t.Fatalf("missing error = %q", missingDoc.Error)
	}

	if err := os.Remove(filepath.Join(dir, "track.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "copy.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	gone := getAuthed(t, ts.URL+"/songs/"+id, token)
	defer gone.Body.Close()
	if gone.StatusCode != http.StatusNotFound {
		t.Fatalf("gone status %s", gone.Status)
	}
	var goneDoc errorDocument
	if err := json.NewDecoder(gone.Body).Decode(&goneDoc); err != nil {
		t.Fatal(err)
	}
	if goneDoc.Error != "song unavailable" {
		t.Fatalf("gone error = %q", goneDoc.Error)
	}
}

func TestSnapshotIsTheDeviceBootstrap(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("song"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	ts, handler := serve(t, root, store)

	res, err := http.Get(ts.URL + "/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %s", res.Status)
	}

	res = getAuthed(t, ts.URL+"/snapshot.json", bearer(t, handler, "ada"))
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s: %s", res.Status, body)
	}
	var cat inventory.Catalog
	if err := json.Unmarshal(body, &cat); err != nil {
		t.Fatal(err)
	}
	if cat.LibraryID == "" || cat.Seq != 1 || len(cat.Songs) != 1 {
		t.Fatalf("catalog = %+v", cat)
	}
	song := cat.Songs[0]
	if song.ID == "" || song.Hash == "" || !song.Available {
		t.Fatalf("song = %+v", song)
	}
	if string(song.Metadata) != "{}" || string(song.Analytics) != "{}" {
		t.Fatalf("documents = %s %s", song.Metadata, song.Analytics)
	}
	if strings.Contains(string(body), "\"path\"") {
		t.Fatalf("snapshot includes a path: %s", body)
	}
}

func TestArtworkByHash(t *testing.T) {
	picture := []byte{0xff, 0xd8, 0xff, 0xd9}
	body := webID3(webAPIC(picture))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	ts, handler := serve(t, root, store)
	sum := sha256.Sum256(picture)
	hash := hex.EncodeToString(sum[:])
	token := bearer(t, handler, "ada")

	res := getAuthed(t, ts.URL+"/snapshot.json", token)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s: %s", res.Status, raw)
	}
	if !strings.Contains(string(raw), `"artwork": "`+hash+`"`) {
		t.Fatalf("snapshot = %s", raw)
	}
	if strings.Contains(string(raw), "\"path\"") || bytes.Contains(raw, picture) {
		t.Fatalf("snapshot includes a path or the image: %s", raw)
	}

	res = getAuthed(t, ts.URL+"/artwork/"+hash, token)
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s: %s", res.Status, got)
	}
	if res.Header.Get("Content-Type") != "image/jpeg" || res.Header.Get("ETag") != `"`+hash+`"` {
		t.Fatalf("type %s etag %s", res.Header.Get("Content-Type"), res.Header.Get("ETag"))
	}
	if !bytes.Equal(got, picture) {
		t.Fatalf("body = %x", got)
	}
	if strings.Contains(res.Header.Get("Content-Type"), root) || bytes.Contains(got, []byte(root)) {
		t.Fatal("response exposes the library path")
	}

	cond, err := http.NewRequest(http.MethodGet, ts.URL+"/artwork/"+hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	cond.Header.Set("Authorization", "Bearer "+token)
	cond.Header.Set("If-None-Match", `"`+hash+`"`)
	res, err = http.DefaultClient.Do(cond)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotModified {
		t.Fatalf("status %s", res.Status)
	}

	missing := getAuthed(t, ts.URL+"/artwork/nope", token)
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("status %s", missing.Status)
	}
	var doc errorDocument
	if err := json.NewDecoder(missing.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Error != "artwork not found" || strings.Contains(doc.Error, root) {
		t.Fatalf("error = %q", doc.Error)
	}
}

func webAPIC(data []byte) []byte {
	payload := append([]byte("image/jpeg"), 0, 3, 0)
	payload = append([]byte{0}, append(payload, data...)...)
	frame := make([]byte, 10+len(payload))
	copy(frame[0:4], "APIC")
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[10:], payload)
	return frame
}

func webID3(frames ...[]byte) []byte {
	var body []byte
	for _, frame := range frames {
		body = append(body, frame...)
	}
	hdr := make([]byte, 10+len(body))
	copy(hdr, "ID3")
	hdr[3] = 3
	n := len(body)
	hdr[6] = byte((n >> 21) & 0x7f)
	hdr[7] = byte((n >> 14) & 0x7f)
	hdr[8] = byte((n >> 7) & 0x7f)
	hdr[9] = byte(n & 0x7f)
	copy(hdr[10:], body)
	return hdr
}

func locationState(asset inventory.Asset) []string {
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

func same(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestActionLogUsesTheHeaderUser(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("song"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openStore(t)
	if err := inventory.Scan(root, store); err != nil {
		t.Fatal(err)
	}
	ts, handler := serve(t, root, store)
	token := bearer(t, handler, "ada")
	cat := getSnapshot(t, handler, ts.URL, token)
	body := []byte(`{"actionId":"play-1","userId":"bea","songId":"` + cat.Songs[0].ID + `","type":"play","payload":{"userId":"bea","listenedMs":30001}}`)
	res := postJSON(t, ts.URL+"/actions", "phone", token, body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %s", res.Status)
	}
	var ack struct {
		Seq int64 `json:"seq"`
	}
	if err := json.NewDecoder(res.Body).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	again := postJSON(t, ts.URL+"/actions", "phone", token, body)
	var ack2 struct {
		Seq int64 `json:"seq"`
	}
	if err := json.NewDecoder(again.Body).Decode(&ack2); err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.StatusCode != http.StatusOK || ack2.Seq != ack.Seq {
		t.Fatalf("retry status %s seq %d want %d", again.Status, ack2.Seq, ack.Seq)
	}
	changed := bytes.Replace(body, []byte(`"listenedMs":30001`), []byte(`"listenedMs":40000`), 1)
	conflict := postJSON(t, ts.URL+"/actions", "phone", token, changed)
	conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status %s", conflict.Status)
	}
	got := getAuthed(t, ts.URL+"/actions?since=0", token)
	defer got.Body.Close()
	var page struct {
		Actions []inventory.Action `json:"actions"`
	}
	if err := json.NewDecoder(got.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	var play inventory.Action
	for _, action := range page.Actions {
		if action.Type == "play" {
			play = action
		}
	}
	if play.UserID != "ada" || play.DeviceID != "phone" || play.Seq != ack.Seq {
		t.Fatalf("play = %+v", play)
	}
	bea, err := store.Bootstrap("bea")
	if err != nil {
		t.Fatal(err)
	}
	if string(bea.Songs[0].Analytics) != "{}" {
		t.Fatalf("bea analytics = %s", bea.Songs[0].Analytics)
	}
	ahead := getAuthed(t, ts.URL+"/actions?since=999999", token)
	ahead.Body.Close()
	if ahead.StatusCode != http.StatusConflict {
		t.Fatalf("ahead status %s", ahead.Status)
	}
}

func TestAuthGoogleAndApple(t *testing.T) {
	store := openStore(t)
	ts, handler := serve(t, t.TempDir(), store)

	handler.Auth.SetGoogleVerifier(func(ctx context.Context, idToken string) (string, error) {
		if idToken != "good-google" {
			return "", auth.ErrInvalidToken
		}
		return "ada@example.com", nil
	})
	handler.Auth.SetAppleVerifier(func(ctx context.Context, idToken string) (string, error) {
		if idToken == "blocked" {
			return "", auth.ErrNotAllowlisted
		}
		if idToken != "good-apple" {
			return "", auth.ErrInvalidToken
		}
		return "ada@example.com", nil
	})

	bad := postAuth(t, ts.URL+"/auth/google", `{"idToken":"nope"}`)
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad google status %s", bad.Status)
	}
	blocked := postAuth(t, ts.URL+"/auth/apple", `{"idToken":"blocked"}`)
	blocked.Body.Close()
	if blocked.StatusCode != http.StatusForbidden {
		t.Fatalf("blocked apple status %s", blocked.Status)
	}

	ok := postAuth(t, ts.URL+"/auth/google", `{"idToken":"good-google"}`)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("google status %s", ok.Status)
	}
	var session authSessionDocument
	if err := json.NewDecoder(ok.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if session.UserID != "ada@example.com" || session.Token == "" {
		t.Fatalf("session = %+v", session)
	}
	snap := getAuthed(t, ts.URL+"/snapshot.json", session.Token)
	snap.Body.Close()
	if snap.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status %s", snap.Status)
	}
	stray, err := http.NewRequest(http.MethodGet, ts.URL+"/snapshot.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	stray.Header.Set("Authorization", "Bearer "+session.Token)
	stray.Header.Set("X-User-Id", "other")
	ignoredHeader, err := http.DefaultClient.Do(stray)
	if err != nil {
		t.Fatal(err)
	}
	ignoredHeader.Body.Close()
	if ignoredHeader.StatusCode != http.StatusOK {
		t.Fatalf("stray user header status %s", ignoredHeader.Status)
	}

	apple := postAuth(t, ts.URL+"/auth/apple", `{"idToken":"good-apple"}`)
	apple.Body.Close()
	if apple.StatusCode != http.StatusOK {
		t.Fatalf("apple status %s", apple.Status)
	}
}

func postAuth(t *testing.T, url, body string) *http.Response {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func getAuthed(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func getSnapshot(t *testing.T, h *Handler, base, token string) inventory.Catalog {
	t.Helper()
	res := getAuthed(t, base+"/snapshot.json", token)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status %s: %s", res.Status, body)
	}
	var cat inventory.Catalog
	if err := json.NewDecoder(res.Body).Decode(&cat); err != nil {
		t.Fatal(err)
	}
	return cat
}

func postJSON(t *testing.T, url, device, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Device-Id", device)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
