package config

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
)

const defaultPollInterval = 30 * time.Second

// Watch monitors the config file for changes and reloads it when either:
//   - a SIGHUP is received, or
//   - the file's mtime advances (checked every interval).
//
// On a successful reload onReload is called with the new Config. On error the
// existing Config is kept and a warning is logged (no-op policy).
// Watch runs until ctx is cancelled.
func Watch(ctx context.Context, h *Holder, interval time.Duration, onReload func(*Config), log *zap.Logger) {
	if interval <= 0 {
		interval = defaultPollInterval
	}

	// Capture initial mtime so the first poll doesn't immediately reload.
	lastMod := mtimeOrZero(h.path)

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)

	ticker := time.NewTicker(interval)

	go func() {
		defer signal.Stop(sighup)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-sighup:
				reload(h, "SIGHUP", &lastMod, onReload, log)

			case <-ticker.C:
				mod := mtimeOrZero(h.path)
				if mod.IsZero() {
					log.Warn("config watcher: could not stat config file", zap.String("path", h.path))
					continue
				}
				if mod.After(lastMod) {
					reload(h, "file changed", &lastMod, onReload, log)
				}
			}
		}
	}()
}

func reload(h *Holder, reason string, lastMod *time.Time, onReload func(*Config), log *zap.Logger) {
	newCfg, err := h.Reload()
	if err != nil {
		log.Warn("config reload failed, keeping current config",
			zap.String("reason", reason),
			zap.String("path", h.path),
			zap.Error(err))
		return
	}
	// Update tracked mtime after a successful reload so we don't re-fire
	// on the same file version.
	*lastMod = mtimeOrZero(h.path)
	log.Info("config reloaded", zap.String("reason", reason), zap.String("path", h.path))
	if onReload != nil {
		onReload(newCfg)
	}
}

func mtimeOrZero(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
