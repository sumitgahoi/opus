package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRootFlag(t *testing.T) {
	root, err := resolveRoot("", "/srv/music")
	if err != nil {
		t.Fatal(err)
	}
	if root != "/srv/music" {
		t.Fatalf("root = %q", root)
	}
	if _, err := resolveRoot("", ""); err != errUsage {
		t.Fatalf("missing folder: %v", err)
	}
	if _, err := resolveRoot("/etc/opus.conf", "/srv/music"); err == nil {
		t.Fatal("config and root were both accepted")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opus.conf")
	body := "# site\n\nmusic_root = /srv/my music\napple_client_ids=app.musicclient.ios\napple_allowed=ada@example.com\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MusicRoot != "/srv/my music" {
		t.Fatalf("root = %q", cfg.MusicRoot)
	}
	if _, ok := cfg.Auth.AppleClientIDs["app.musicclient.ios"]; !ok {
		t.Fatalf("apple clients = %#v", cfg.Auth.AppleClientIDs)
	}
	if _, ok := cfg.Auth.AppleAllowed["ada@example.com"]; !ok {
		t.Fatalf("apple allowed = %#v", cfg.Auth.AppleAllowed)
	}
}

func TestLoadConfigBothProviders(t *testing.T) {
	body := "music_root=/a\ngoogle_client_id=g.client\ngoogle_allowed_emails=Ada@Example.com\napple_client_ids=app.musicclient.ios,com.example.web\napple_allowed=ada@example.com,apple.sub\ndev_auth=true\n"
	cfg, err := parseConfig("opus.conf", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.GoogleClientID != "g.client" {
		t.Fatalf("google id = %q", cfg.Auth.GoogleClientID)
	}
	if _, ok := cfg.Auth.GoogleAllowedEmails["ada@example.com"]; !ok {
		t.Fatalf("google emails = %#v", cfg.Auth.GoogleAllowedEmails)
	}
	if len(cfg.Auth.AppleClientIDs) != 2 {
		t.Fatalf("apple clients = %#v", cfg.Auth.AppleClientIDs)
	}
	if !cfg.Auth.DevAuth {
		t.Fatal("dev_auth was not enabled")
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := []string{
		"",
		"music_root=\n",
		"music_root=/a\nmusic_root=/b\n",
		"port=80\nmusic_root=/a\n",
		"music_root\n",
		"music_root=/a\n",
		"music_root=/a\ngoogle_client_id=g\n",
		"music_root=/a\napple_client_ids=app.x\n",
	}
	for _, body := range cases {
		_, err := parseConfig("opus.conf", strings.NewReader(body))
		if err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}
