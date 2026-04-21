package logger

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestResolveLevel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  string
		env  string
		want slog.Level
	}{
		{"cfg wins over env", "debug", "error", slog.LevelDebug},
		{"env used when cfg empty", "", "warn", slog.LevelWarn},
		{"warning alias", "warning", "", slog.LevelWarn},
		{"case insensitive", "ERROR", "", slog.LevelError},
		{"default to info on unknown", "verbose", "", slog.LevelInfo},
		{"default to info on empty", "", "", slog.LevelInfo},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolveLevel(tc.cfg, tc.env).Level()
			if got != tc.want {
				t.Fatalf("resolveLevel(%q, %q) = %v, want %v", tc.cfg, tc.env, got, tc.want)
			}
		})
	}
}

func TestResolveFormat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  Format
		env  Format
		want Format // exact match; ignore TTY fallback by passing explicit value
	}{
		{"cfg wins", FormatJSON, FormatText, FormatJSON},
		{"env used when cfg empty", "", FormatJSON, FormatJSON},
		{"case insensitive", "JSON", "", FormatJSON},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolveFormat(tc.cfg, tc.env)
			if got != tc.want {
				t.Fatalf("resolveFormat(%q, %q) = %q, want %q", tc.cfg, tc.env, got, tc.want)
			}
		})
	}
}

func TestBuildEmitsJSON(t *testing.T) {
	var buf bytes.Buffer
	l := build(Config{
		Level:  "info",
		Format: FormatJSON,
		Writer: &buf,
	})
	l.With("subsystem", "raft").Info("hello", "key", "k1", "rev", int64(7))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not valid JSON: %v\nraw=%s", err, buf.String())
	}
	if rec["msg"] != "hello" {
		t.Errorf("msg = %v, want %q", rec["msg"], "hello")
	}
	if rec["subsystem"] != "raft" {
		t.Errorf("subsystem = %v, want %q", rec["subsystem"], "raft")
	}
	if rec["key"] != "k1" {
		t.Errorf("key = %v, want %q", rec["key"], "k1")
	}
}

func TestBuildRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	l := build(Config{
		Level:  "warn",
		Format: FormatText,
		Writer: &buf,
	})
	l.Info("should not appear")
	l.Warn("should appear")

	out := buf.String()
	if strings.Contains(out, "should not appear") {
		t.Errorf("info-level record leaked past warn threshold: %s", out)
	}
	if !strings.Contains(out, "should appear") {
		t.Errorf("warn-level record missing: %s", out)
	}
}
