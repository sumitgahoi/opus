package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"opus/internal/auth"
)

// errUsage means the process was started without a configuration file or a music folder.
var errUsage = errors.New("usage: server -config FILE | -root DIR [-db inventory.db] [-addr 0.0.0.0:80] [-scan-interval 15m]")

// fileConfig is the site configuration. The database path, listen address,
// and scan interval stay flags.
type fileConfig struct {
	MusicRoot string
	Auth      auth.Config
}

// resolveConfig returns the music folder and auth settings.
// -config loads both. -root is for local inventory work without sign-in providers.
func resolveConfig(configPath, rootFlag string) (fileConfig, error) {
	switch {
	case configPath != "" && rootFlag != "":
		return fileConfig{}, errors.New("set -config or -root, not both")
	case configPath != "":
		return loadConfig(configPath)
	case rootFlag == "":
		return fileConfig{}, errUsage
	default:
		return fileConfig{MusicRoot: rootFlag}, nil
	}
}

// resolveRoot returns the music folder from the configuration file or from -root.
func resolveRoot(configPath, rootFlag string) (string, error) {
	cfg, err := resolveConfig(configPath, rootFlag)
	if err != nil {
		return "", err
	}
	return cfg.MusicRoot, nil
}

// loadConfig reads key=value settings. Blank lines and lines starting with # are ignored.
// music_root is required. At least one of Google or Apple sign-in must be configured.
func loadConfig(path string) (fileConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return fileConfig{}, err
	}
	defer f.Close()
	return parseConfig(path, f)
}

func parseConfig(path string, r io.Reader) (fileConfig, error) {
	var cfg fileConfig
	var sawRoot bool
	var googleID, googleEmails, appleIDs, appleAllowed, devAuth string
	sc := bufio.NewScanner(r)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fileConfig{}, fmt.Errorf("%s:%d: expected key=value", path, lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "music_root":
			if sawRoot {
				return fileConfig{}, fmt.Errorf("%s:%d: music_root is set twice", path, lineNo)
			}
			if value == "" {
				return fileConfig{}, fmt.Errorf("%s:%d: music_root is empty", path, lineNo)
			}
			sawRoot = true
			cfg.MusicRoot = value
		case "google_client_id":
			googleID = value
		case "google_allowed_emails":
			googleEmails = value
		case "apple_client_ids":
			appleIDs = value
		case "apple_allowed":
			appleAllowed = value
		case "dev_auth":
			devAuth = value
		default:
			return fileConfig{}, fmt.Errorf("%s:%d: unknown setting %q", path, lineNo, key)
		}
	}
	if err := sc.Err(); err != nil {
		return fileConfig{}, err
	}
	if !sawRoot {
		return fileConfig{}, fmt.Errorf("%s: music_root is required", path)
	}
	cfg.Auth.GoogleClientID = googleID
	cfg.Auth.GoogleAllowedEmails = splitEmails(googleEmails)
	cfg.Auth.AppleClientIDs = splitIDs(appleIDs)
	cfg.Auth.AppleAllowed = splitAppleAllowed(appleAllowed)
	switch strings.ToLower(devAuth) {
	case "", "0", "false", "no", "off":
		cfg.Auth.DevAuth = false
	case "1", "true", "yes", "on":
		cfg.Auth.DevAuth = true
	default:
		return fileConfig{}, fmt.Errorf("%s: dev_auth must be true or false", path)
	}
	if googleID != "" && len(cfg.Auth.GoogleAllowedEmails) == 0 {
		return fileConfig{}, fmt.Errorf("%s: google_allowed_emails is required when google_client_id is set", path)
	}
	if len(cfg.Auth.GoogleAllowedEmails) > 0 && googleID == "" {
		return fileConfig{}, fmt.Errorf("%s: google_client_id is required when google_allowed_emails is set", path)
	}
	if len(cfg.Auth.AppleClientIDs) > 0 && len(cfg.Auth.AppleAllowed) == 0 {
		return fileConfig{}, fmt.Errorf("%s: apple_allowed is required when apple_client_ids is set", path)
	}
	if len(cfg.Auth.AppleAllowed) > 0 && len(cfg.Auth.AppleClientIDs) == 0 {
		return fileConfig{}, fmt.Errorf("%s: apple_client_ids is required when apple_allowed is set", path)
	}
	if googleID == "" && len(cfg.Auth.AppleClientIDs) == 0 {
		return fileConfig{}, fmt.Errorf("%s: configure google_client_id or apple_client_ids", path)
	}
	return cfg, nil
}

func splitEmails(csv string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, part := range strings.Split(csv, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		out[part] = struct{}{}
	}
	return out
}

func splitIDs(csv string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out[part] = struct{}{}
	}
	return out
}

// splitAppleAllowed keeps emails lowercased and Apple subject ids as written.
func splitAppleAllowed(csv string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "@") {
			part = strings.ToLower(part)
		}
		out[part] = struct{}{}
	}
	return out
}
