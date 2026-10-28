package auth

import (
	"context"
	"crypto/rsa"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	googleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"
	appleJWKSURL  = "https://appleid.apple.com/auth/keys"
)

// Config is the sign-in providers enabled for this server.
type Config struct {
	GoogleClientID      string
	GoogleAllowedEmails map[string]struct{}
	AppleClientIDs      map[string]struct{}
	AppleAllowed        map[string]struct{} // emails and/or Apple subject ids
	// DevAuth enables POST /auth/dev for temporary phone login without Apple/Google.
	DevAuth bool
}

// Service verifies Apple and Google identity tokens and issues sessions.
type Service struct {
	Sessions *Sessions
	Config   Config

	GoogleKeys *KeySet
	AppleKeys  *KeySet
	Now        func() time.Time

	// optional overrides for tests
	verifyGoogle func(ctx context.Context, idToken string) (userID string, err error)
	verifyApple  func(ctx context.Context, idToken string) (userID string, err error)
}

// New builds a Service that stores sessions in db.
func New(sessions *Sessions, cfg Config) *Service {
	return &Service{
		Sessions: sessions,
		Config:   cfg,
		GoogleKeys: &KeySet{
			URL:    googleJWKSURL,
			Client: &http.Client{Timeout: 15 * time.Second},
		},
		AppleKeys: &KeySet{
			URL:    appleJWKSURL,
			Client: &http.Client{Timeout: 15 * time.Second},
		},
		Now: time.Now,
	}
}

// Enabled reports whether at least one provider is configured.
func (s *Service) Enabled() bool {
	return s.GoogleEnabled() || s.AppleEnabled()
}

// GoogleEnabled is true when a Google client id and allowlist are set.
func (s *Service) GoogleEnabled() bool {
	return s.Config.GoogleClientID != "" && len(s.Config.GoogleAllowedEmails) > 0
}

// AppleEnabled is true when Apple client ids and an allowlist are set.
func (s *Service) AppleEnabled() bool {
	return len(s.Config.AppleClientIDs) > 0 && len(s.Config.AppleAllowed) > 0
}

// SignInGoogle verifies a Google ID token and returns a session.
func (s *Service) SignInGoogle(ctx context.Context, idToken string) (userID, token string, expiresAt time.Time, err error) {
	if !s.GoogleEnabled() {
		return "", "", time.Time{}, ErrProviderDisabled
	}
	verify := s.verifyGoogle
	if verify == nil {
		verify = s.verifyGoogleIDToken
	}
	userID, err = verify(ctx, idToken)
	if err != nil {
		return "", "", time.Time{}, err
	}
	token, expiresAt, err = s.Sessions.Create(userID)
	return userID, token, expiresAt, err
}

// SignInApple verifies an Apple identity token and returns a session.
func (s *Service) SignInApple(ctx context.Context, idToken string) (userID, token string, expiresAt time.Time, err error) {
	if !s.AppleEnabled() {
		return "", "", time.Time{}, ErrProviderDisabled
	}
	verify := s.verifyApple
	if verify == nil {
		verify = s.verifyAppleIDToken
	}
	userID, err = verify(ctx, idToken)
	if err != nil {
		return "", "", time.Time{}, err
	}
	token, expiresAt, err = s.Sessions.Create(userID)
	return userID, token, expiresAt, err
}

// SignInDev issues a session for an allowlisted user id without an identity token.
// Only available when Config.DevAuth is true.
func (s *Service) SignInDev(userID string) (resolvedUserID, token string, expiresAt time.Time, err error) {
	if !s.Config.DevAuth {
		return "", "", time.Time{}, ErrProviderDisabled
	}
	resolvedUserID = strings.TrimSpace(userID)
	if resolvedUserID == "" {
		return "", "", time.Time{}, ErrInvalidToken
	}
	if strings.Contains(resolvedUserID, "@") {
		resolvedUserID = strings.ToLower(resolvedUserID)
	}
	if !s.allowlisted(resolvedUserID) {
		return "", "", time.Time{}, ErrNotAllowlisted
	}
	token, expiresAt, err = s.Sessions.Create(resolvedUserID)
	return resolvedUserID, token, expiresAt, err
}

func (s *Service) allowlisted(userID string) bool {
	if _, ok := s.Config.GoogleAllowedEmails[userID]; ok {
		return true
	}
	if _, ok := s.Config.AppleAllowed[userID]; ok {
		return true
	}
	return false
}

// UserFromBearer resolves Authorization: Bearer <token>.
func (s *Service) UserFromBearer(header string) (string, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", ErrUnauthorized
	}
	return s.Sessions.Lookup(strings.TrimSpace(header[len(prefix):]))
}

func (s *Service) verifyGoogleIDToken(ctx context.Context, idToken string) (string, error) {
	_ = ctx
	header, claims, signed, sig, err := parseJWT(idToken)
	if err != nil {
		return "", ErrInvalidToken
	}
	if header.Alg != "RS256" {
		return "", ErrInvalidToken
	}
	key, err := s.GoogleKeys.key(header.Kid)
	if err != nil {
		return "", ErrInvalidToken
	}
	if err := verifyRS256(signed, sig, key); err != nil {
		return "", ErrInvalidToken
	}
	iss := claimString(claims, "iss")
	if iss != "accounts.google.com" && iss != "https://accounts.google.com" {
		return "", ErrInvalidToken
	}
	if claimString(claims, "aud") != s.Config.GoogleClientID {
		return "", ErrInvalidToken
	}
	exp, ok := claimTime(claims, "exp")
	if !ok || !exp.After(s.now()) {
		return "", ErrInvalidToken
	}
	if !claimBool(claims, "email_verified") {
		return "", ErrInvalidToken
	}
	email := strings.ToLower(strings.TrimSpace(claimString(claims, "email")))
	if email == "" {
		return "", ErrInvalidToken
	}
	if _, ok := s.Config.GoogleAllowedEmails[email]; !ok {
		return "", ErrNotAllowlisted
	}
	return email, nil
}

func (s *Service) verifyAppleIDToken(ctx context.Context, idToken string) (string, error) {
	_ = ctx
	header, claims, signed, sig, err := parseJWT(idToken)
	if err != nil {
		return "", ErrInvalidToken
	}
	if header.Alg != "RS256" {
		return "", ErrInvalidToken
	}
	key, err := s.AppleKeys.key(header.Kid)
	if err != nil {
		return "", ErrInvalidToken
	}
	if err := verifyRS256(signed, sig, key); err != nil {
		return "", ErrInvalidToken
	}
	if claimString(claims, "iss") != "https://appleid.apple.com" {
		return "", ErrInvalidToken
	}
	aud := claimString(claims, "aud")
	if _, ok := s.Config.AppleClientIDs[aud]; !ok {
		return "", ErrInvalidToken
	}
	exp, ok := claimTime(claims, "exp")
	if !ok || !exp.After(s.now()) {
		return "", ErrInvalidToken
	}
	sub := strings.TrimSpace(claimString(claims, "sub"))
	if sub == "" {
		return "", ErrInvalidToken
	}
	email := strings.ToLower(strings.TrimSpace(claimString(claims, "email")))
	allowed := false
	if email != "" {
		if _, ok := s.Config.AppleAllowed[email]; ok {
			allowed = true
		}
	}
	if _, ok := s.Config.AppleAllowed[sub]; ok {
		allowed = true
	}
	if !allowed {
		return "", ErrNotAllowlisted
	}
	// Prefer email as user id when Apple provided one and it is allowlisted;
	// otherwise the stable Apple subject.
	if email != "" {
		if _, ok := s.Config.AppleAllowed[email]; ok {
			return email, nil
		}
	}
	return sub, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// UseGoogleKeys installs RSA keys for Google ID token tests.
func (s *Service) UseGoogleKeys(keys map[string]*rsa.PublicKey) {
	s.GoogleKeys.SetKeys(keys)
}

// UseAppleKeys installs RSA keys for Apple identity token tests.
func (s *Service) UseAppleKeys(keys map[string]*rsa.PublicKey) {
	s.AppleKeys.SetKeys(keys)
}

// SetGoogleVerifier replaces Google verification in tests.
func (s *Service) SetGoogleVerifier(fn func(ctx context.Context, idToken string) (string, error)) {
	s.verifyGoogle = fn
}

// SetAppleVerifier replaces Apple verification in tests.
func (s *Service) SetAppleVerifier(fn func(ctx context.Context, idToken string) (string, error)) {
	s.verifyApple = fn
}

// IsAuthError reports whether err should map to 401/403.
func IsAuthError(err error) bool {
	return errors.Is(err, ErrUnauthorized) ||
		errors.Is(err, ErrInvalidToken) ||
		errors.Is(err, ErrNotAllowlisted) ||
		errors.Is(err, ErrProviderDisabled)
}
