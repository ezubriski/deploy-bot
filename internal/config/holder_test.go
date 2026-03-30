package config

import (
	"context"
	"encoding/json"
	"os"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// writeConfigFile writes cfg as JSON to a temp file and returns the path.
func writeConfigFile(t *testing.T, cfg *Config) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.json")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if err := json.NewEncoder(f).Encode(cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	f.Close()
	return f.Name()
}

func minimalConfig() *Config {
	return &Config{
		GitHub: GitHubConfig{Org: "org", Repo: "repo"},
		Deployment: DeploymentConfig{
			StaleDuration: "2h",
			MergeMethod:   "squash",
		},
	}
}

// --- Holder tests ---

func TestHolderLoadReturnsInitialConfig(t *testing.T) {
	cfg := minimalConfig()
	h := NewHolder(cfg, "/tmp/fake")
	if h.Load() != cfg {
		t.Error("Load() should return the initial config")
	}
}

func TestHolderStore(t *testing.T) {
	h := NewHolder(minimalConfig(), "/tmp/fake")
	newCfg := &Config{GitHub: GitHubConfig{Org: "new-org"}}
	h.Store(newCfg)
	if got := h.Load(); got != newCfg {
		t.Errorf("Load() after Store() = %p, want %p", got, newCfg)
	}
}

func TestHolderReload_Success(t *testing.T) {
	original := minimalConfig()
	path := writeConfigFile(t, original)
	h := NewHolder(original, path)

	// Overwrite the file with a changed value.
	updated := minimalConfig()
	updated.GitHub.Org = "reloaded-org"
	data, _ := json.Marshal(updated)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write updated config: %v", err)
	}

	got, err := h.Reload()
	if err != nil {
		t.Fatalf("Reload() error: %v", err)
	}
	if got.GitHub.Org != "reloaded-org" {
		t.Errorf("reloaded org = %q, want %q", got.GitHub.Org, "reloaded-org")
	}
	if h.Load().GitHub.Org != "reloaded-org" {
		t.Error("Load() after Reload() should return updated config")
	}
}

func TestHolderReload_InvalidJSON_KeepsOld(t *testing.T) {
	original := minimalConfig()
	path := writeConfigFile(t, original)
	h := NewHolder(original, path)

	if err := os.WriteFile(path, []byte("not valid json{{{"), 0644); err != nil {
		t.Fatalf("write bad config: %v", err)
	}

	_, err := h.Reload()
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	// Config must be unchanged.
	if h.Load() != original {
		t.Error("Reload() with bad JSON should not replace the config")
	}
}

func TestHolderReload_MissingFile_KeepsOld(t *testing.T) {
	original := minimalConfig()
	h := NewHolder(original, "/nonexistent/path/config.json")

	_, err := h.Reload()
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if h.Load() != original {
		t.Error("Reload() with missing file should not replace the config")
	}
}

// --- Watcher tests ---

func TestWatchReloadsOnMtimeChange(t *testing.T) {
	original := minimalConfig()
	path := writeConfigFile(t, original)
	h := NewHolder(original, path)

	reloaded := make(chan *Config, 1)
	onReload := func(cfg *Config) { reloaded <- cfg }

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	Watch(ctx, h, 20*time.Millisecond, onReload, zap.NewNop())

	// Give the watcher goroutine time to start and record the initial mtime.
	time.Sleep(30 * time.Millisecond)

	// Write an updated config. Ensure the mtime advances by sleeping 1ms
	// (filesystem resolution is typically 1ns on Linux but be safe).
	updated := minimalConfig()
	updated.GitHub.Org = "watched-org"
	data, _ := json.Marshal(updated)
	time.Sleep(time.Millisecond)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write updated config: %v", err)
	}

	select {
	case cfg := <-reloaded:
		if cfg.GitHub.Org != "watched-org" {
			t.Errorf("reloaded org = %q, want %q", cfg.GitHub.Org, "watched-org")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watcher did not reload config after mtime change")
	}
}

func TestWatchNoReloadWhenFileUnchanged(t *testing.T) {
	path := writeConfigFile(t, minimalConfig())
	h := NewHolder(minimalConfig(), path)

	reloaded := make(chan struct{}, 1)
	onReload := func(_ *Config) { reloaded <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	Watch(ctx, h, 20*time.Millisecond, onReload, zap.NewNop())

	// Wait for a couple of poll ticks without modifying the file.
	time.Sleep(80 * time.Millisecond)

	select {
	case <-reloaded:
		t.Error("watcher should not reload when file has not changed")
	default:
		// pass
	}
}

func TestWatchReloadsOnSIGHUP(t *testing.T) {
	original := minimalConfig()
	path := writeConfigFile(t, original)
	h := NewHolder(original, path)

	updated := minimalConfig()
	updated.GitHub.Org = "sighup-org"
	data, _ := json.Marshal(updated)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write updated config: %v", err)
	}

	reloaded := make(chan *Config, 1)
	onReload := func(cfg *Config) { reloaded <- cfg }

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Use a long poll interval so only SIGHUP triggers a reload.
	Watch(ctx, h, time.Hour, onReload, zap.NewNop())
	time.Sleep(20 * time.Millisecond) // let watcher goroutine start

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}

	select {
	case cfg := <-reloaded:
		if cfg.GitHub.Org != "sighup-org" {
			t.Errorf("reloaded org = %q, want %q", cfg.GitHub.Org, "sighup-org")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watcher did not reload config after SIGHUP")
	}
}

func TestWatchInvalidReload_KeepsOldAndContinues(t *testing.T) {
	original := minimalConfig()
	path := writeConfigFile(t, original)
	h := NewHolder(original, path)

	reloaded := make(chan *Config, 1)
	onReload := func(cfg *Config) { reloaded <- cfg }

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	Watch(ctx, h, 20*time.Millisecond, onReload, zap.NewNop())
	time.Sleep(30 * time.Millisecond)

	// Write bad JSON — should log a warning and not call onReload.
	time.Sleep(time.Millisecond)
	if err := os.WriteFile(path, []byte("{bad"), 0644); err != nil {
		t.Fatalf("write bad config: %v", err)
	}

	time.Sleep(80 * time.Millisecond)

	select {
	case <-reloaded:
		t.Error("onReload should not be called when reload fails")
	default:
		// pass
	}

	// Original config must still be in place.
	if h.Load() != original {
		t.Error("holder should still have original config after failed reload")
	}

	// Now write a valid update — watcher should recover and reload.
	good := minimalConfig()
	good.GitHub.Org = "recovered-org"
	data, _ := json.Marshal(good)
	time.Sleep(time.Millisecond)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write recovered config: %v", err)
	}

	select {
	case cfg := <-reloaded:
		if cfg.GitHub.Org != "recovered-org" {
			t.Errorf("recovered org = %q, want %q", cfg.GitHub.Org, "recovered-org")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watcher did not recover after bad config was replaced with valid one")
	}
}
