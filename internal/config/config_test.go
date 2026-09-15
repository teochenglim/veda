package config

import (
	"os"
	"path/filepath"
	"testing"
)

// AC1: `veda init` writes a config.toml whose telemetry default is OFF —
// there is no auto-consent anywhere.
func TestAC1_TelemetryDefaultsOffAndConfigRoundTrips(t *testing.T) {
	cfg := Default()
	if cfg.Telemetry.Enabled {
		t.Fatal("telemetry must default to OFF (no auto-consent)")
	}
	if cfg.UI.Bind != "127.0.0.1:7331" {
		t.Fatalf("UI must default to loopback:7331, got %q", cfg.UI.Bind)
	}
	if cfg.Worker.IntervalMinutes != 15 {
		t.Fatalf("worker interval must default to 15 minutes, got %d", cfg.Worker.IntervalMinutes)
	}

	// round-trip through a real file in an isolated VEDA_HOME
	home := t.TempDir()
	t.Setenv("VEDA_HOME", home)
	if Home() != home {
		t.Fatalf("VEDA_HOME override broken: %q", Home())
	}
	cfg.Telemetry.InstallID = NewInstallID()
	if cfg.Telemetry.InstallID == "" {
		t.Fatal("install id must be generated")
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); err != nil {
		t.Fatalf("config.toml not written: %v", err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Telemetry.Enabled || loaded.Telemetry.InstallID != cfg.Telemetry.InstallID {
		t.Fatalf("round-trip mismatch: %+v", loaded.Telemetry)
	}
	// the API key lives in the environment, never on disk
	data, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if os.Getenv("VEDA_LLM_API_KEY") != "" && contains(string(data), os.Getenv("VEDA_LLM_API_KEY")) {
		t.Fatal("API key must never be written to config.toml")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// Load on a fresh install (no config.toml) yields defaults, not an error.
func TestLoadMissingFileYieldsDefaults(t *testing.T) {
	t.Setenv("VEDA_HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.Enabled {
		t.Fatal("fresh install must have telemetry off")
	}
}
