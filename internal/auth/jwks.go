package auth

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type jwksDocument struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// KeySet fetches and caches JWKS public keys.
type KeySet struct {
	URL    string
	Client *http.Client

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func (k *KeySet) key(kid string) (*rsa.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if key, ok := k.keys[kid]; ok && time.Since(k.fetched) < time.Hour {
		return key, nil
	}
	if err := k.refreshLocked(); err != nil {
		if key, ok := k.keys[kid]; ok {
			return key, nil
		}
		return nil, err
	}
	key, ok := k.keys[kid]
	if !ok {
		return nil, fmt.Errorf("jwks: unknown kid %q", kid)
	}
	return key, nil
}

func (k *KeySet) refreshLocked() error {
	client := k.Client
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Get(k.URL)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: status %s", res.Status)
	}
	var doc jwksDocument
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		return err
	}
	next := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, item := range doc.Keys {
		if item.Kty != "RSA" || item.N == "" || item.E == "" {
			continue
		}
		pub, err := jwkRSA(item.N, item.E)
		if err != nil {
			continue
		}
		next[item.Kid] = pub
	}
	if len(next) == 0 {
		return fmt.Errorf("jwks: no rsa keys")
	}
	k.keys = next
	k.fetched = time.Now()
	return nil
}

// SetKeys installs keys for tests without fetching.
func (k *KeySet) SetKeys(keys map[string]*rsa.PublicKey) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = keys
	k.fetched = time.Now()
}
