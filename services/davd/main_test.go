package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A machine with no WebDAV share has no rendered config at all. That is the
// state a container cannot gate on (the systemd unit uses ConditionPathExists),
// so it must be distinguishable from a broken render: the first serves an empty
// table, the second must never serve.
func TestLoadConfigMissingFileIsNotAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "davd.conf")

	_, err := loadConfig(missing)
	if err == nil {
		t.Fatal("expected an error for a missing config")
	}
	if !errors.Is(err, errNoConfig) {
		t.Fatalf("missing config must report errNoConfig, got %v", err)
	}
}

func TestLoadConfigBrokenFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "davd.conf")
	if err := os.WriteFile(path, []byte("[server]\nauth = \"nope\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("expected an invalid auth mode to be refused")
	}
	if errors.Is(err, errNoConfig) {
		t.Fatalf("a present but invalid config must not look like an unrendered one: %v", err)
	}
}

// The empty table the daemon starts with has to be a configuration the daemon
// would accept from a file — otherwise "nothing is configured yet" would be a
// state it could not run in.
func TestDefaultConfigIsValidAndServesNothing(t *testing.T) {
	cfg := defaultConfig()
	if err := cfg.validate(); err != nil {
		t.Fatalf("default configuration must validate: %v", err)
	}
	if len(cfg.Shares) != 0 {
		t.Fatalf("default configuration must have no shares, got %d", len(cfg.Shares))
	}
	if cfg.Auth != authModeGateway {
		t.Fatalf("default auth must be %q, got %q", authModeGateway, cfg.Auth)
	}
}

// The watcher is what makes the first WebDAV share appear without an operator
// restarting the daemon.
func TestWatchConfigServesAConfigThatAppears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "davd.conf")

	active := &reloadable{}
	active.swap(newHandler(defaultConfig()), nil)
	// The daemon had no config to load, so the baseline is empty: whatever
	// appears next is a revision.
	go watchConfig(path, "", active, "127.0.0.1:8081", "", 5*time.Millisecond)

	shareDir := filepath.Join(dir, "media")
	if err := os.MkdirAll(shareDir, 0o750); err != nil {
		t.Fatal(err)
	}
	rendered := "[server]\nlisten = \"127.0.0.1:8081\"\n\n[[share]]\nname = \"media\"\npath = \"" + shareDir + "\"\nreadonly = false\n"
	if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		active.mu.RLock()
		shares := append([]share(nil), active.shares...)
		active.mu.RUnlock()
		if len(shares) == 1 && shares[0].Name == "media" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("watcher never picked the config up, shares = %+v", shares)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
