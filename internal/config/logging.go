package config

import (
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ParseLogLevel parses a string log level into a zapcore.Level. Valid values
// are "debug", "info", "warn", "error" (case-insensitive). The empty string
// is rejected; callers should pick a default before invoking this.
func ParseLogLevel(s string) (zapcore.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return zapcore.DebugLevel, nil
	case "info":
		return zapcore.InfoLevel, nil
	case "warn", "warning":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return zapcore.InvalidLevel, fmt.Errorf("unknown log level %q (valid: debug, info, warn, error)", s)
	}
}

// ResolvedLogLevel returns the effective log level for the process.
// Precedence (highest to lowest): the LOG_LEVEL environment variable, the
// log_level field on the config, the built-in default of info. If the env
// var is set but invalid the error is returned so the caller can refuse to
// start.
func ResolvedLogLevel(cfg *Config) (zapcore.Level, error) {
	if env := os.Getenv("LOG_LEVEL"); env != "" {
		return ParseLogLevel(env)
	}
	if cfg != nil && cfg.LogLevel != "" {
		return ParseLogLevel(cfg.LogLevel)
	}
	return zapcore.InfoLevel, nil
}

// NewLogger returns a zap production logger pinned to the supplied level.
// Use ResolvedLogLevel to compute the level from config + env var.
func NewLogger(level zapcore.Level) (*zap.Logger, error) {
	zcfg := zap.NewProductionConfig()
	zcfg.Level = zap.NewAtomicLevelAt(level)
	return zcfg.Build()
}
