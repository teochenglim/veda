// Package config loads and saves ~/.veda/config.toml.
//
// VEDA_HOME overrides the Veda home directory (used by tests and by users who
// want an isolated installation). The default is $HOME/.veda.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is the on-disk configuration. The API key itself is never stored on
// disk; it is read from the environment variable named by LLM.APIKeyEnv.
type Config struct {
	LLM       LLMConfig       `toml:"llm"`
	Embed     EmbedConfig     `toml:"embed"`
	Worker    WorkerConfig    `toml:"worker"`
	UI        UIConfig        `toml:"ui"`
	Telemetry TelemetryConfig `toml:"telemetry"`
}

type LLMConfig struct {
	// BaseURL is any OpenAI-compatible /v1 endpoint.
	BaseURL string `toml:"base_url"`
	Model   string `toml:"model"`
	// APIKeyEnv names the environment variable holding the key, e.g. VEDA_LLM_API_KEY.
	APIKeyEnv string `toml:"api_key_env"`
}

type EmbedConfig struct {
	// Enabled defaults to false: semantic recall never makes a network call
	// unless the user opts in.
	Enabled bool `toml:"enabled"`
	// BaseURL is any OpenAI-compatible /v1 endpoint; a localhost URL (Ollama,
	// LM Studio) keeps embeddings on the machine.
	BaseURL string `toml:"base_url"`
	Model   string `toml:"model"`
	// APIKeyEnv is optional — local servers need no key.
	APIKeyEnv string `toml:"api_key_env"`
}

type WorkerConfig struct {
	IntervalMinutes int `toml:"interval_minutes"`
	BatchSize       int `toml:"batch_size"`
}

type UIConfig struct {
	// Bind is host:port. Defaults to loopback only; the memory DB is local
	// and unauthenticated, so it must not be exposed on other interfaces.
	Bind string `toml:"bind"`
}

type TelemetryConfig struct {
	// Enabled defaults to false. Only an explicit first-run prompt answer or
	// `veda telemetry enable` may turn it on. There is no auto-consent.
	Enabled   bool   `toml:"enabled"`
	URL       string `toml:"url"`
	InstallID string `toml:"install_id"`
	// FlushHours is how often queued telemetry is POSTed to URL.
	FlushHours int `toml:"flush_hours"`
}

// Home returns the Veda home directory, creating nothing.
func Home() string {
	if h := os.Getenv("VEDA_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".veda"
	}
	return filepath.Join(home, ".veda")
}

func DBPath() string     { return filepath.Join(Home(), "veda.db") }
func WALPath() string    { return filepath.Join(Home(), "wal.ndjson") }
func ConfigPath() string { return filepath.Join(Home(), "config.toml") }

// Default returns the default configuration.
func Default() *Config {
	return &Config{
		LLM: LLMConfig{
			BaseURL:   "https://api.openai.com/v1",
			Model:     "gpt-4o-mini",
			APIKeyEnv: "VEDA_LLM_API_KEY",
		},
		Worker: WorkerConfig{IntervalMinutes: 15, BatchSize: 20},
		UI:     UIConfig{Bind: "127.0.0.1:7331"},
		Embed: EmbedConfig{
			Enabled:   false,
			BaseURL:   "http://localhost:11434/v1", // Ollama default
			Model:     "bge-m3",
			APIKeyEnv: "VEDA_EMBED_API_KEY",
		},
		Telemetry: TelemetryConfig{
			Enabled:    false,
			URL:        "", // set after the backend repo is deployed
			FlushHours: 24,
		},
	}
}

// APIKey resolves the user-supplied LLM key from the environment.
func (c *Config) APIKey() string {
	if c.LLM.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.LLM.APIKeyEnv)
}

// EmbedAPIKey resolves the optional embedding key from the environment.
func (c *Config) EmbedAPIKey() string {
	if c.Embed.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.Embed.APIKeyEnv)
}

// Load reads config.toml from the Veda home. A missing file yields Default.
func Load() (*Config, error) {
	c := Default()
	data, err := os.ReadFile(ConfigPath())
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := toml.Decode(string(data), c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigPath(), err)
	}
	return c, nil
}

// Save writes config.toml (mode 0600: it names the telemetry install id).
func Save(c *Config) error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(ConfigPath(), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := toml.NewEncoder(f)
	return enc.Encode(c)
}

// NewInstallID returns a random identifier for telemetry pseudonymity.
func NewInstallID() string {
	b := make([]byte, 16)
	if _, err := randRead(b); err != nil {
		return "install-unknown"
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
