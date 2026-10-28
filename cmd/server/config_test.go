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
	body := "# site\n\nmusic_root = /srv/my music\nauthorized_users=ada@example.com\napple_client_ids=app.hush.ios\n"
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
	if _, ok := cfg.Auth.AppleClientIDs["app.hush.ios"]; !ok {
		t.Fatalf("apple clients = %#v", cfg.Auth.AppleClientIDs)
	}
	if _, ok := cfg.Auth.AuthorizedUsers["ada@example.com"]; !ok {
		t.Fatalf("authorized users = %#v", cfg.Auth.AuthorizedUsers)
	}
}

func TestLoadConfigBothProviders(t *testing.T) {
	body := "music_root=/a\nauthorized_users=Ada@Example.com,apple.sub\ngoogle_client_id=g.client\napple_client_ids=app.hush.ios,com.example.web\n"
	cfg, err := parseConfig("opus.conf", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.GoogleClientID != "g.client" {
		t.Fatalf("google id = %q", cfg.Auth.GoogleClientID)
	}
	if _, ok := cfg.Auth.AuthorizedUsers["ada@example.com"]; !ok {
		t.Fatalf("authorized users = %#v", cfg.Auth.AuthorizedUsers)
	}
	if _, ok := cfg.Auth.AuthorizedUsers["apple.sub"]; !ok {
		t.Fatalf("authorized users = %#v", cfg.Auth.AuthorizedUsers)
	}
	if len(cfg.Auth.AppleClientIDs) != 2 {
		t.Fatalf("apple clients = %#v", cfg.Auth.AppleClientIDs)
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
		"music_root=/a\nauthorized_users=\ngoogle_client_id=g\n",
	}
	for _, body := range cases {
		_, err := parseConfig("opus.conf", strings.NewReader(body))
		if err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}
