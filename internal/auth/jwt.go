package auth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

type jwtHeader struct {
	Kid string `json:"kid"`
	Alg string `json:"alg"`
}

type jwtClaims map[string]any

func parseJWT(token string) (header jwtHeader, claims jwtClaims, signed, sig []byte, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtHeader{}, nil, nil, nil, errors.New("jwt: malformed")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jwtHeader{}, nil, nil, nil, fmt.Errorf("jwt header: %w", err)
	}
	if err := json.Unmarshal(hb, &header); err != nil {
		return jwtHeader{}, nil, nil, nil, fmt.Errorf("jwt header: %w", err)
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtHeader{}, nil, nil, nil, fmt.Errorf("jwt claims: %w", err)
	}
	if err := json.Unmarshal(cb, &claims); err != nil {
		return jwtHeader{}, nil, nil, nil, fmt.Errorf("jwt claims: %w", err)
	}
	sig, err = base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jwtHeader{}, nil, nil, nil, fmt.Errorf("jwt sig: %w", err)
	}
	signed = []byte(parts[0] + "." + parts[1])
	return header, claims, signed, sig, nil
}

func verifyRS256(signed, sig []byte, key *rsa.PublicKey) error {
	sum := sha256.Sum256(signed)
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig)
}

func claimString(claims jwtClaims, key string) string {
	v, ok := claims[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func claimBool(claims jwtClaims, key string) bool {
	v, ok := claims[key]
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "True"
	default:
		return false
	}
}

func claimTime(claims jwtClaims, key string) (time.Time, bool) {
	v, ok := claims[key]
	if !ok || v == nil {
		return time.Time{}, false
	}
	switch t := v.(type) {
	case float64:
		return time.Unix(int64(t), 0), true
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(n, 0), true
	default:
		return time.Time{}, false
	}
}

func jwkRSA(nB64, eB64 string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(nb)
	var eInt int
	for _, b := range eb {
		eInt = eInt<<8 + int(b)
	}
	if eInt == 0 {
		return nil, errors.New("jwk: empty exponent")
	}
	return &rsa.PublicKey{N: n, E: eInt}, nil
}
