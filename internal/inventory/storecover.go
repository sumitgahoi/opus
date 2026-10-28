package inventory

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// storeCovers turns an iTunes track or album id into one cover image.
// A definitive result is finished: art may be nil when Apple has no cover.
// A non-nil error is transient and the scan tries again later.
type storeCovers interface {
	Cover(albumID, trackID int64) (*coverArt, bool, error)
}

type storeIDs struct {
	Track int64
	Album int64
}

// storeAudio reports files that can carry an iTunes purchase id.
func storeAudio(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".m4a", ".m4b", ".mp4":
		return true
	default:
		return false
	}
}

// resolveStore reads purchase ids and downloads one shared cover.
// The file is closed before any network call. An open failure fails the
// scan. A tag or network failure leaves the lookup unfinished.
func resolveStore(open func(string) (io.ReadCloser, error), full, rel string, size int64, covers storeCovers) (*coverArt, bool, error) {
	rc, err := open(full)
	if err != nil {
		return nil, false, err
	}
	var ids storeIDs
	var readErr error
	if rs, ok := rc.(io.ReadSeeker); ok {
		ids, readErr = readStoreIDs(rs, size)
	}
	closeErr := rc.Close()
	if readErr != nil {
		log.Printf("artwork %s: %v", rel, readErr)
		return nil, false, nil
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	if ids.Track == 0 && ids.Album == 0 {
		return nil, true, nil
	}
	if covers == nil {
		return nil, true, nil
	}
	art, definitive, err := covers.Cover(ids.Album, ids.Track)
	if err != nil {
		return nil, false, nil
	}
	return art, definitive, nil
}

// readStoreIDs walks MP4 atoms for the iTunes track id (cnID) and album id (plID).
func readStoreIDs(r io.ReadSeeker, size int64) (storeIDs, error) {
	if size < 8 {
		return storeIDs{}, nil
	}
	return findStoreIDs(r, 0, size, false)
}

func findStoreIDs(r io.ReadSeeker, start, end int64, inIlst bool) (storeIDs, error) {
	var ids storeIDs
	for pos := start; pos+8 <= end; {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return ids, err
		}
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return ids, err
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[0:4]))
		kind := string(hdr[4:8])
		header := int64(8)
		if boxSize == 1 {
			var ext [8]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return ids, err
			}
			boxSize = int64(binary.BigEndian.Uint64(ext[:]))
			header = 16
		} else if boxSize == 0 {
			boxSize = end - pos
		}
		if boxSize < header || pos+boxSize > end {
			return ids, nil
		}
		payload := pos + header
		switch kind {
		case "moov", "udta", "ilst":
			child, err := findStoreIDs(r, payload, pos+boxSize, kind == "ilst")
			if err != nil {
				return ids, err
			}
			ids = mergeStoreIDs(ids, child)
		case "meta":
			if payload+4 <= pos+boxSize {
				child, err := findStoreIDs(r, payload+4, pos+boxSize, false)
				if err != nil {
					return ids, err
				}
				ids = mergeStoreIDs(ids, child)
			}
		default:
			if inIlst && (kind == "cnID" || kind == "plID") {
				n, ok, err := readIlstInt(r, payload, pos+boxSize)
				if err != nil {
					return ids, err
				}
				if ok && n != 0 {
					if kind == "cnID" {
						ids.Track = n
					} else {
						ids.Album = n
					}
				}
			}
		}
		if ids.Track != 0 && ids.Album != 0 {
			return ids, nil
		}
		pos += boxSize
	}
	return ids, nil
}

func mergeStoreIDs(a, b storeIDs) storeIDs {
	if a.Track == 0 {
		a.Track = b.Track
	}
	if a.Album == 0 {
		a.Album = b.Album
	}
	return a
}

func readIlstInt(r io.ReadSeeker, start, end int64) (int64, bool, error) {
	if start+16 > end {
		return 0, false, nil
	}
	if _, err := r.Seek(start, io.SeekStart); err != nil {
		return 0, false, err
	}
	var hdr [16]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, false, err
	}
	if string(hdr[4:8]) != "data" {
		return 0, false, nil
	}
	boxSize := int64(binary.BigEndian.Uint32(hdr[0:4]))
	if boxSize < 16 || start+boxSize > end {
		return 0, false, nil
	}
	n := int(boxSize - 16)
	if n <= 0 || n > 8 {
		return 0, false, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, false, err
	}
	var v int64
	for _, b := range buf {
		v = v<<8 | int64(b)
	}
	return v, true, nil
}

const storeImageLimit = 8 << 20

type storeCatalog struct {
	client    *http.Client
	lookupURL func(id int64) string
	allowHost func(host string) bool

	mu    sync.Mutex
	cache map[int64]storeHit
}

type storeHit struct {
	art        *coverArt
	definitive bool
	err        error
}

func newStoreCatalog(client *http.Client) *storeCatalog {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if client.CheckRedirect == nil {
		client.CheckRedirect = storeRedirect
	}
	return &storeCatalog{
		client: client,
		cache:  map[int64]storeHit{},
	}
}

func storeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("stopped after 5 redirects")
	}
	if !storeHostOK(req.URL.Hostname()) && req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("refusing redirect to %s", req.URL.Host)
	}
	return nil
}

func storeHostOK(host string) bool {
	host = strings.ToLower(host)
	return host == "mzstatic.com" || strings.HasSuffix(host, ".mzstatic.com") || host == "itunes.apple.com"
}

// Cover downloads the album image once per scan. The track id is used when
// the file has no album id. A shared album id is one request for every song.
func (c *storeCatalog) Cover(albumID, trackID int64) (*coverArt, bool, error) {
	if c == nil {
		return nil, true, nil
	}
	id := albumID
	if id == 0 {
		id = trackID
	}
	if id == 0 {
		return nil, true, nil
	}
	c.mu.Lock()
	if hit, ok := c.cache[id]; ok {
		c.mu.Unlock()
		return hit.art, hit.definitive, hit.err
	}
	c.mu.Unlock()

	art, definitive, err := c.fetch(id)
	if err != nil {
		log.Printf("artwork store %d: %v", id, err)
	}
	c.mu.Lock()
	c.cache[id] = storeHit{art: art, definitive: definitive, err: err}
	c.mu.Unlock()
	return art, definitive, err
}

func (c *storeCatalog) fetch(id int64) (*coverArt, bool, error) {
	lookup := "https://itunes.apple.com/lookup?id=" + strconv.FormatInt(id, 10)
	if c.lookupURL != nil {
		lookup = c.lookupURL(id)
	}
	body, err := c.get(lookup, storeImageLimit)
	if err != nil {
		return nil, false, err
	}
	var doc struct {
		ResultCount int `json:"resultCount"`
		Results     []struct {
			ArtworkURL string `json:"artworkUrl100"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false, err
	}
	if doc.ResultCount == 0 {
		return nil, true, nil
	}
	var raw string
	for _, result := range doc.Results {
		if result.ArtworkURL != "" {
			raw = result.ArtworkURL
			break
		}
	}
	if raw == "" {
		return nil, true, nil
	}
	imageURL, err := storeImageURL(raw, c.allow)
	if err != nil {
		return nil, false, err
	}
	img, err := c.get(imageURL, storeImageLimit)
	if err != nil {
		return nil, false, err
	}
	art := coverFromBytes(img, "")
	if art == nil {
		return nil, false, errors.New("store artwork is not an image")
	}
	return art, true, nil
}

func (c *storeCatalog) allow(host string) bool {
	if c.allowHost != nil {
		return c.allowHost(host)
	}
	return storeHostOK(host)
}

func (c *storeCatalog) get(raw string, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "opus")
	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("store artwork status %s", res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("store artwork is too large")
	}
	return body, nil
}

func storeImageURL(raw string, allow func(string) bool) (string, error) {
	raw = strings.Replace(raw, "100x100bb", "600x600bb", 1)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", errors.New("store artwork url")
	}
	if allow == nil || !allow(u.Hostname()) {
		return "", errors.New("store artwork url")
	}
	return u.String(), nil
}

func coverFromBytes(data []byte, mime string) *coverArt {
	mime = imageMIME(mime, data)
	if mime == "" || len(data) == 0 {
		return nil
	}
	sum := sha256.Sum256(data)
	return &coverArt{Hash: hex.EncodeToString(sum[:]), MIME: mime, Data: append([]byte(nil), data...)}
}
