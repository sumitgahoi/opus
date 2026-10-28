package web

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"opus/internal/auth"
	"opus/internal/inventory"
)

// Handler serves the inventory as JSON.
type Handler struct {
	Root  string
	Store *inventory.Store
	Auth  *auth.Service

	scan func(root string, store *inventory.Store) error

	mu       sync.Mutex
	scanning bool
	done     chan struct{}
	scanErr  error
}

// New prepares the JSON inventory handler. Auth may be nil only for
// local -root inventory work; phone and library routes then refuse access.
func New(root string, store *inventory.Store, authSvc *auth.Service) *Handler {
	return &Handler{Root: root, Store: store, Auth: authSvc, scan: inventory.Scan}
}

// Start begins an immediate library scan. A positive interval scans
// again on that period. A tick or POST /rescan during a scan does nothing.
func (h *Handler) Start(interval time.Duration) {
	h.trigger()
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			h.trigger()
		}
	}()
}

// Routes serves the inventory document, a rescan that returns the same document,
// a song download by id, a cover image by its hash, the action log, and a
// health check that confirms the database still answers.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("POST /auth/google", h.authGoogle)
	mux.HandleFunc("POST /auth/apple", h.authApple)
	mux.HandleFunc("POST /auth/dev", h.authDev)
	mux.HandleFunc("GET /inventory.json", h.inventory)
	mux.HandleFunc("GET /snapshot.json", h.snapshot)
	mux.HandleFunc("GET /songs/{id}", h.song)
	mux.HandleFunc("GET /artwork/{hash}", h.artwork)
	mux.HandleFunc("GET /actions", h.actions)
	mux.HandleFunc("POST /actions", h.postAction)
	mux.HandleFunc("POST /rescan", h.rescan)
	return mux
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.Ping(); err != nil {
		log.Printf("health: %v", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, healthDocument{OK: true})
}

func (h *Handler) inventory(w http.ResponseWriter, r *http.Request) {
	doc, err := h.document()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "inventory could not be read")
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (h *Handler) authGoogle(w http.ResponseWriter, r *http.Request) {
	h.signIn(w, r, func(idToken string) (string, string, time.Time, error) {
		return h.Auth.SignInGoogle(r.Context(), idToken)
	})
}

func (h *Handler) authApple(w http.ResponseWriter, r *http.Request) {
	h.signIn(w, r, func(idToken string) (string, string, time.Time, error) {
		return h.Auth.SignInApple(r.Context(), idToken)
	})
}

func (h *Handler) authDev(w http.ResponseWriter, r *http.Request) {
	if h.Auth == nil {
		writeError(w, http.StatusServiceUnavailable, "sign-in is not configured")
		return
	}
	var body struct {
		UserID string `json:"userId"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil || strings.TrimSpace(body.UserID) == "" {
		writeError(w, http.StatusBadRequest, "user id is required")
		return
	}
	userID, token, expiresAt, err := h.Auth.SignInDev(body.UserID)
	switch {
	case errors.Is(err, auth.ErrProviderDisabled):
		writeError(w, http.StatusServiceUnavailable, "dev sign-in is not enabled")
	case errors.Is(err, auth.ErrNotAllowlisted):
		writeError(w, http.StatusForbidden, "account not allowed")
	case errors.Is(err, auth.ErrInvalidToken):
		writeError(w, http.StatusBadRequest, "user id is required")
	case err != nil:
		log.Printf("auth: %v", err)
		writeError(w, http.StatusInternalServerError, "sign-in failed")
	default:
		writeJSON(w, http.StatusOK, authSessionDocument{
			UserID:    userID,
			Token:     token,
			ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		})
	}
}

func (h *Handler) signIn(w http.ResponseWriter, r *http.Request, fn func(idToken string) (string, string, time.Time, error)) {
	if h.Auth == nil {
		writeError(w, http.StatusServiceUnavailable, "sign-in is not configured")
		return
	}
	var body struct {
		IDToken string `json:"idToken"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil || strings.TrimSpace(body.IDToken) == "" {
		writeError(w, http.StatusBadRequest, "invalid token")
		return
	}
	userID, token, expiresAt, err := fn(strings.TrimSpace(body.IDToken))
	switch {
	case errors.Is(err, auth.ErrProviderDisabled):
		writeError(w, http.StatusServiceUnavailable, "provider not configured")
	case errors.Is(err, auth.ErrNotAllowlisted):
		writeError(w, http.StatusForbidden, "account not allowed")
	case errors.Is(err, auth.ErrInvalidToken):
		writeError(w, http.StatusUnauthorized, "unauthorized")
	case err != nil:
		log.Printf("auth: %v", err)
		writeError(w, http.StatusInternalServerError, "sign-in failed")
	default:
		writeJSON(w, http.StatusOK, authSessionDocument{
			UserID:    userID,
			Token:     token,
			ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		})
	}
}

func (h *Handler) postAction(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	var body struct {
		ActionID     string          `json:"actionId"`
		UserID       string          `json:"userId"`
		SongID       string          `json:"songId"`
		Type         string          `json:"type"`
		OccurredAtMs *int64          `json:"occurredAtMs"`
		Payload      json.RawMessage `json:"payload"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid action")
		return
	}
	seq, err := h.Store.Accept(userID, r.Header.Get("X-Device-Id"), inventory.Submit{
		ActionID:     body.ActionID,
		SongID:       body.SongID,
		Type:         body.Type,
		OccurredAtMs: body.OccurredAtMs,
		Payload:      body.Payload,
	})
	switch {
	case errors.Is(err, inventory.ErrUserID):
		writeError(w, http.StatusBadRequest, "user id is required")
	case errors.Is(err, inventory.ErrAction):
		writeError(w, http.StatusBadRequest, "invalid action")
	case errors.Is(err, inventory.ErrSongNotFound):
		writeError(w, http.StatusNotFound, "song not found")
	case errors.Is(err, inventory.ErrConflict):
		writeError(w, http.StatusConflict, "action conflict")
	case err != nil:
		log.Printf("action: %v", err)
		writeError(w, http.StatusInternalServerError, "action could not be stored")
	default:
		writeJSON(w, http.StatusOK, struct {
			Seq int64 `json:"seq"`
		}{seq})
	}
}

func (h *Handler) actions(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	list, err := h.Store.ActionsAfter(userID, since)
	switch {
	case errors.Is(err, inventory.ErrUserID):
		writeError(w, http.StatusBadRequest, "user id is required")
	case errors.Is(err, inventory.ErrAction):
		writeError(w, http.StatusBadRequest, "invalid cursor")
	case errors.Is(err, inventory.ErrCursor):
		writeError(w, http.StatusConflict, "cursor is ahead of the server")
	case err != nil:
		log.Printf("actions: %v", err)
		writeError(w, http.StatusInternalServerError, "actions could not be read")
	default:
		if list == nil {
			list = []inventory.Action{}
		}
		writeJSON(w, http.StatusOK, struct {
			Actions []inventory.Action `json:"actions"`
		}{list})
	}
}

func (h *Handler) snapshot(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	cat, err := h.Store.Bootstrap(userID)
	if errors.Is(err, inventory.ErrUserID) {
		writeError(w, http.StatusBadRequest, "user id is required")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot could not be read")
		return
	}
	if cat.Songs == nil {
		cat.Songs = []inventory.CatalogSong{}
	}
	writeJSON(w, http.StatusOK, cat)
}

// requireUser resolves the session user from Authorization: Bearer.
func (h *Handler) requireUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	if h.Auth == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	userID, err := h.Auth.UserFromBearer(r.Header.Get("Authorization"))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	return userID, true
}

// song sends the bytes of one present copy. The id is the song; the path
// stays on the server. A missing song and a song with no present copy are
// both errors, and neither reports a path.
func (h *Handler) song(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireUser(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	rel, err := h.Store.PresentPath(id)
	if errors.Is(err, inventory.ErrSongNotFound) {
		writeError(w, http.StatusNotFound, "song not found")
		return
	}
	if errors.Is(err, inventory.ErrSongUnavailable) {
		writeError(w, http.StatusNotFound, "song unavailable")
		return
	}
	if err != nil {
		log.Printf("song %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "song could not be read")
		return
	}
	full, err := fileUnderRoot(h.Root, rel)
	if err != nil {
		log.Printf("song %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "song could not be read")
		return
	}
	info, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "song unavailable")
			return
		}
		log.Printf("song %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "song could not be read")
		return
	}
	if !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "song unavailable")
		return
	}
	f, err := os.Open(full)
	if err != nil {
		log.Printf("song %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "song could not be read")
		return
	}
	defer f.Close()

	name := path.Base(rel)
	w.Header().Set("Content-Type", audioContentType(name))
	if cd := mime.FormatMediaType("attachment", map[string]string{"filename": name}); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// artwork sends one cached cover. The hash names the image bytes. A missing
// cache file is rebuilt from a present copy: its embedded picture, or the
// iTunes id when that picture is absent.
func (h *Handler) artwork(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireUser(w, r); !ok {
		return
	}
	hash := r.PathValue("hash")
	f, mime, err := h.Store.OpenArtwork(hash, func(rel string) (io.ReadSeekCloser, error) {
		full, err := fileUnderRoot(h.Root, rel)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(full)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, os.ErrNotExist
		}
		return os.Open(full)
	})
	if errors.Is(err, inventory.ErrArtworkNotFound) {
		writeError(w, http.StatusNotFound, "artwork not found")
		return
	}
	if err != nil {
		log.Printf("artwork %s: %v", hash, err)
		writeError(w, http.StatusInternalServerError, "artwork could not be read")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		log.Printf("artwork %s: %v", hash, err)
		writeError(w, http.StatusInternalServerError, "artwork could not be read")
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("ETag", `"`+hash+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, hash, info.ModTime(), f)
}

func fileUnderRoot(root, rel string) (string, error) {
	if rel == "" || path.IsAbs(rel) || strings.Contains(rel, `\`) {
		return "", errors.New("invalid song path")
	}
	cleaned := path.Clean(rel)
	if cleaned != rel || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("invalid song path")
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	rootClean := filepath.Clean(root)
	if full != rootClean && !strings.HasPrefix(full, rootClean+string(os.PathSeparator)) {
		return "", errors.New("invalid song path")
	}
	return full, nil
}

func audioContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".aac":
		return "audio/aac"
	case ".ac3":
		return "audio/ac3"
	case ".aif", ".aiff":
		return "audio/aiff"
	case ".caf":
		return "audio/x-caf"
	case ".eac3", ".ec3":
		return "audio/eac3"
	case ".flac":
		return "audio/flac"
	case ".m4a", ".m4b", ".mp4":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".wav", ".wave":
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

func (h *Handler) rescan(w http.ResponseWriter, r *http.Request) {
	h.trigger()
	h.inventory(w, r)
}

// trigger starts one scan if none is running. The scan reads the library
// off the request path and commits only after the walk finishes.
func (h *Handler) trigger() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scanning {
		return false
	}
	h.scanning = true
	h.scanErr = nil
	h.done = make(chan struct{})
	done := h.done
	go func() {
		err := h.scan(h.Root, h.Store)
		h.mu.Lock()
		h.scanning = false
		h.scanErr = err
		h.mu.Unlock()
		close(done)
		if err != nil {
			log.Printf("scan: %v", err)
			return
		}
		snap, err := h.Store.Snapshot()
		if err != nil {
			log.Printf("scan: %v", err)
			return
		}
		log.Printf("scan finished: %d songs", len(snap.Assets))
	}()
	return true
}

func (h *Handler) waitForScan() error {
	for {
		h.mu.Lock()
		scanning := h.scanning
		done := h.done
		err := h.scanErr
		h.mu.Unlock()
		if !scanning {
			return err
		}
		<-done
	}
}

func (h *Handler) document() (inventoryDocument, error) {
	snap, err := h.Store.Snapshot()
	if err != nil {
		return inventoryDocument{}, err
	}
	assets := snap.Assets
	if assets == nil {
		assets = []inventory.Asset{}
	}
	for i := range assets {
		if assets[i].Locations == nil {
			assets[i].Locations = []inventory.Location{}
		}
	}
	return inventoryDocument{Root: h.Root, Assets: assets}, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		log.Printf("inventory json: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorDocument{Error: message})
}

// inventoryDocument is the inventory a program reads.
type inventoryDocument struct {
	Root   string            `json:"root"`
	Assets []inventory.Asset `json:"assets"`
}

type errorDocument struct {
	Error string `json:"error"`
}

type healthDocument struct {
	OK bool `json:"ok"`
}

type authSessionDocument struct {
	UserID    string `json:"userId"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
}
