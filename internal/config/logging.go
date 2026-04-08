package config

import (
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// LogFormat is the zap encoder name. Currently "json" or "console".
type LogFormat string

const (
	LogFormatJSON    LogFormat = "json"
	LogFormatConsole LogFormat = "console"
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

// ParseLogFormat parses a string log format into a LogFormat constant.
// Valid values are "json" and "console" (case-insensitive). The empty
// string is rejected; callers should pick a default first.
func ParseLogFormat(s string) (LogFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "json":
		return LogFormatJSON, nil
	case "console":
		return LogFormatConsole, nil
	default:
		return "", fmt.Errorf("unknown log format %q (valid: json, console)", s)
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

// ResolvedLogFormat returns the effective encoder for the process.
// Precedence (highest to lowest): the LOG_FORMAT environment variable, the
// log_format field on the config, the built-in default of json.
func ResolvedLogFormat(cfg *Config) (LogFormat, error) {
	if env := os.Getenv("LOG_FORMAT"); env != "" {
		return ParseLogFormat(env)
	}
	if cfg != nil && cfg.LogFormat != "" {
		return ParseLogFormat(cfg.LogFormat)
	}
	return LogFormatJSON, nil
}

// NewLogger returns a zap production logger pinned to the supplied level
// and encoder. Use ResolvedLogLevel and ResolvedLogFormat to compute the
// arguments from config + env vars.
func NewLogger(level zapcore.Level, format LogFormat) (*zap.Logger, error) {
	zcfg := zap.NewProductionConfig()
	zcfg.Level = zap.NewAtomicLevelAt(level)
	zcfg.Encoding = string(format)
	if format == LogFormatConsole {
		// Production JSON config uses ISO8601 + numeric levels, which look
		// awful in console mode. Switch to the development encoder for
		// human consumption (colorized levels, short caller, friendly
		// timestamps) without abandoning the rest of the production
		// settings (sampling, stacktrace policy, etc.).
		dev := zap.NewDevelopmentEncoderConfig()
		zcfg.EncoderConfig = dev
	}
	return zcfg.Build()
}
