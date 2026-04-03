package config

import (
	"fmt"
	"sync"
)

// Holder provides thread-safe access to a Config that can be reloaded at
// runtime without restarting. All components should call Load() on each
// operation rather than caching the returned pointer.
type Holder struct {
	mu             sync.RWMutex
	cfg            *Config
	path           string
	discoveredPath string
}

// NewHolder wraps an already-loaded Config and records the file path for
// subsequent Reload calls.
func NewHolder(cfg *Config, path string) *Holder {
	return &Holder{cfg: cfg, path: path}
}

// NewHolderWithDiscovered wraps an already-loaded Config and records both the
// primary and discovered file paths for subsequent Reload calls.
func NewHolderWithDiscovered(cfg *Config, path, discoveredPath string) *Holder {
	return &Holder{cfg: cfg, path: path, discoveredPath: discoveredPath}
}

// DiscoveredPath returns the discovered apps file path, or empty if not set.
func (h *Holder) DiscoveredPath() string {
	return h.discoveredPath
}

// Load returns the current Config. The caller should not cache the pointer
// across operations — call Load() again to pick up any reloads.
func (h *Holder) Load() *Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

// Store atomically replaces the current Config.
func (h *Holder) Store(cfg *Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
}

// Reload re-reads the config file (and discovered apps file if configured).
// On success it atomically replaces the current Config and returns the new
// value. On any error it leaves the existing Config unchanged and returns a
// wrapped error — callers should treat this as a no-op warning, not a fatal
// condition.
func (h *Holder) Reload() (*Config, error) {
	newCfg, err := LoadWithDiscovered(h.path, h.discoveredPath)
	if err != nil {
		return nil, fmt.Errorf("reload config: %w", err)
	}
	h.Store(newCfg)
	return newCfg, nil
}

// Path returns the config file path used for reloads.
func (h *Holder) Path() string {
	return h.path
}
