// Package logger is the project's single entry point for structured logging.
//
// It wraps log/slog (Go 1.21+) with:
//
//   - Environment-driven level and format selection (PALADIN_LOG_LEVEL,
//     PALADIN_LOG_FORMAT) — twelve-factor friendly.
//   - Subsystem loggers via L("raft"), L("sdk"), ... which emit the
//     "subsystem" attribute on every record, so log aggregators can filter
//     without regex.
//   - A single Init() call that installs the chosen handler as slog.Default,
//     so code that just wants to log can call slog.Info(...) directly.
//
// Why not zap / zerolog? slog is stdlib since 1.21, zero-dep, and its
// allocation profile is within 2x of zap for our call volume. The maintenance
// cost of a third-party logger outweighs the perf difference at our RPS.
// If profiling ever proves otherwise, swap the handler — the call sites
// (logger.L("raft").Info("...", k, v)) do not change.
package logger

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Format is the wire format of log output.
type Format string

const (
	// FormatText emits human-friendly lines; use in development.
	FormatText Format = "text"
	// FormatJSON emits one JSON object per record; use in production for
	// Loki / Elasticsearch / CloudWatch ingestion.
	FormatJSON Format = "json"
)

// Config controls logger initialization. All fields are optional; missing
// values fall back to environment variables, then to sensible defaults
// (info level, text format in a TTY, json otherwise).
type Config struct {
	// Level is one of "debug", "info", "warn", "error". Case-insensitive.
	Level string
	// Format is one of "text", "json". Case-insensitive.
	Format Format
	// Writer is where records are written. Defaults to os.Stderr.
	Writer io.Writer
	// AddSource enables file:line annotations. Useful in dev; leave off in
	// prod because it forces runtime.Callers on every call.
	AddSource bool
}

const (
	envLevel  = "PALADIN_LOG_LEVEL"
	envFormat = "PALADIN_LOG_FORMAT"
)

var (
	initOnce sync.Once
	base     *slog.Logger // The configured root logger; shared across subsystems.
)

// Init configures the default slog handler. It is safe to call multiple times
// but only the first call takes effect; use Reset for tests.
func Init(cfg Config) {
	initOnce.Do(func() {
		base = build(cfg)
		slog.SetDefault(base)
	})
}

// Reset is exported for tests that need to re-Init with different config.
// Production code must never call this.
func Reset() {
	initOnce = sync.Once{}
	base = nil
}

// L returns a logger scoped to a subsystem. The returned logger emits
// subsystem=<name> on every record, so ops can filter by component
// without parsing the message.
//
//	logger.L("raft").Info("leader changed", "leader", id)
//
// If Init has not been called, L auto-initializes with defaults — this keeps
// the call-site ergonomic (no global init() required).
func L(subsystem string) *slog.Logger {
	if base == nil {
		Init(Config{})
	}
	return base.With("subsystem", subsystem)
}

// Default returns the underlying root logger. Prefer L(subsystem) in business
// code; Default is for tests and for wiring third-party libs that accept a
// *slog.Logger.
func Default() *slog.Logger {
	if base == nil {
		Init(Config{})
	}
	return base
}

// build materializes an slog.Logger from a Config + env overrides. Separated
// from Init so tests can exercise config resolution without touching the
// process-wide default.
func build(cfg Config) *slog.Logger {
	level := resolveLevel(cfg.Level, os.Getenv(envLevel))
	format := resolveFormat(cfg.Format, Format(os.Getenv(envFormat)))

	w := cfg.Writer
	if w == nil {
		w = os.Stderr
	}

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: cfg.AddSource,
	}

	var h slog.Handler
	switch format {
	case FormatJSON:
		h = slog.NewJSONHandler(w, opts)
	default:
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

// resolveLevel picks the effective level: explicit cfg wins, else env, else info.
// Unknown values degrade to info — we prefer "always emit something" over
// "blow up at startup" for a logging subsystem.
func resolveLevel(cfg, env string) slog.Leveler {
	pick := cfg
	if pick == "" {
		pick = env
	}
	switch strings.ToLower(strings.TrimSpace(pick)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// resolveFormat picks the effective format: explicit cfg wins, else env, else
// text when stderr is a TTY, json otherwise. The TTY check is heuristic; it's
// fine to get it wrong — PALADIN_LOG_FORMAT is the authoritative override.
func resolveFormat(cfg, env Format) Format {
	pick := cfg
	if pick == "" {
		pick = env
	}
	switch Format(strings.ToLower(strings.TrimSpace(string(pick)))) {
	case FormatJSON:
		return FormatJSON
	case FormatText:
		return FormatText
	}
	if isTerminal(os.Stderr) {
		return FormatText
	}
	return FormatJSON
}

// isTerminal reports whether f refers to a terminal. Kept intentionally
// minimal — a false negative just means JSON output in a TTY, which users
// can override via PALADIN_LOG_FORMAT=text.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
