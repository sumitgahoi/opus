package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"opus/internal/inventory"
)

func TestGoogleAndAppleIDTokens(t *testing.T) {
	store, err := inventory.Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sessions := NewSessions(store.DB())
	svc := New(sessions, Config{
		GoogleClientID: "ios.google.client",
		AppleClientIDs: map[string]struct{}{"app.hush.ios": {}},
		AuthorizedUsers: map[string]struct{}{
			"ada@example.com": {},
			"apple.sub.1":     {},
		},
	})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	svc.UseGoogleKeys(map[string]*rsa.PublicKey{"g1": &key.PublicKey})
	svc.UseAppleKeys(map[string]*rsa.PublicKey{"a1": &key.PublicKey})
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	sessions.now = svc.Now

	googleTok := mustSign(t, key, "g1", map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            "ios.google.client",
		"exp":            now.Add(time.Hour).Unix(),
		"email":          "Ada@Example.com",
		"email_verified": true,
	})
	userID, token, _, err := svc.SignInGoogle(context.Background(), googleTok)
	if err != nil || userID != "ada@example.com" || token == "" {
		t.Fatalf("google sign-in: user=%q token=%q err=%v", userID, token, err)
	}
	got, err := svc.UserFromBearer("Bearer " + token)
	if err != nil || got != "ada@example.com" {
		t.Fatalf("lookup = %q %v", got, err)
	}

	blocked := mustSign(t, key, "g1", map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            "ios.google.client",
		"exp":            now.Add(time.Hour).Unix(),
		"email":          "eve@example.com",
		"email_verified": true,
	})
	if _, _, _, err := svc.SignInGoogle(context.Background(), blocked); err != ErrNotAllowlisted {
		t.Fatalf("blocked google: %v", err)
	}

	appleTok := mustSign(t, key, "a1", map[string]any{
		"iss":   "https://appleid.apple.com",
		"aud":   "app.hush.ios",
		"exp":   now.Add(time.Hour).Unix(),
		"sub":   "apple.sub.1",
		"email": "ada@example.com",
	})
	userID, _, _, err = svc.SignInApple(context.Background(), appleTok)
	if err != nil || userID != "ada@example.com" {
		t.Fatalf("apple email user=%q err=%v", userID, err)
	}

	appleSubOnly := mustSign(t, key, "a1", map[string]any{
		"iss": "https://appleid.apple.com",
		"aud": "app.hush.ios",
		"exp": now.Add(time.Hour).Unix(),
		"sub": "apple.sub.1",
	})
	userID, _, _, err = svc.SignInApple(context.Background(), appleSubOnly)
	if err != nil || userID != "apple.sub.1" {
		t.Fatalf("apple sub user=%q err=%v", userID, err)
	}
}

func mustSign(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	hb, err := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString(hb)
	payload := base64.RawURLEncoding.EncodeToString(cb)
	signed := header + "." + payload
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}
