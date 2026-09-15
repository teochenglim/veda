// Ext-block support for telemetry (v0.5.0 "telemetry you can audit").
//
// The ext block widens the counts-only payload with adoption and quality
// signals — still counts and enums only. Sanitization is enforced here, in
// one place: agent/client names are lowercased to [a-z0-9._-] (≤ 32 chars,
// unknown → "other"), embed providers collapse to a fixed enum that can
// never carry a URL, and the maps are capped. What must never appear —
// URLs, paths, locales, free text — cannot, because no API exists to set it.
package telemetry

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Ext is the optional "extended signals" block of a telemetry payload.
type Ext struct {
	Agent           string           `json:"agent,omitempty"`
	Clients         []string         `json:"clients,omitempty"`
	SemanticEnabled bool             `json:"semantic_enabled,omitempty"`
	SyncEnabled     bool             `json:"sync_enabled,omitempty"`
	EmbedProvider   string           `json:"embed_provider,omitempty"`
	GateRejected    int64            `json:"gate_rejected,omitempty"`
	UIOpens         int64            `json:"ui_opens,omitempty"`
	ServeMinutes    int64            `json:"serve_minutes,omitempty"`
	DBSizeKB        int64            `json:"db_size_kb,omitempty"`
	Errors          map[string]int64 `json:"errors,omitempty"`
}

const (
	maxNameLen    = 32
	maxClients    = 8
	maxErrorCodes = 16
)

var nameStrip = regexp.MustCompile(`[^a-z0-9._-]`)

// sanitizeName normalizes an agent/client name for the payload:
// lowercase, [a-z0-9._-] only, ≤ 32 chars, empty/unknown → "other".
func sanitizeName(name string) string {
	n := nameStrip.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "")
	if len(n) > maxNameLen {
		n = n[:maxNameLen]
	}
	if n == "" || n == "unknown" {
		return "other"
	}
	return n
}

// ProviderFromURL collapses an embedding endpoint URL into a fixed enum.
// The URL itself must never reach the payload.
func ProviderFromURL(baseURL string) string {
	u := strings.ToLower(baseURL)
	switch {
	case u == "":
		return ""
	case strings.Contains(u, "11434") || strings.Contains(u, "ollama"):
		return "ollama"
	case strings.Contains(u, "lmstudio") || strings.Contains(u, ":1234"):
		return "lmstudio"
	case strings.Contains(u, "api.openai.com"):
		return "openai"
	default:
		return "other"
	}
}

var errorAllowlist = map[string]bool{
	"embed_fail": true, "flush_fail": true, "llm_fail": true,
	"wal_fail": true, "sync_fail": true,
}

func sanitizeErrorCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if !errorAllowlist[code] {
		return ""
	}
	return code
}

// Collector aggregates serve-side ext signals. Safe for concurrent use by
// the MCP handlers, the worker and the UI.
type Collector struct {
	mu      sync.Mutex
	agent   string
	clients map[string]bool
	errors  map[string]int64
	gate    int64
	uiOpens int64
	started time.Time
}

func NewCollector() *Collector {
	return &Collector{clients: map[string]bool{}, errors: map[string]int64{}, started: time.Now()}
}

// AddClient records an MCP client (from clientInfo.name at initialize).
// The first distinct client seen also becomes the agent id.
func (c *Collector) AddClient(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(name) == "" {
		return
	}
	n := sanitizeName(name)
	c.clients[n] = true
	if c.agent == "" {
		c.agent = n
	}
}

// RecordError counts one allowlisted failure code; unknown codes are dropped.
func (c *Collector) RecordError(code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	code = sanitizeErrorCode(code)
	if code == "" {
		return
	}
	c.errors[code]++
}

// AddGateRejected counts turns the cheap gate dropped.
func (c *Collector) AddGateRejected(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gate += n
}

// AddUIOpen counts one review-UI load.
func (c *Collector) AddUIOpen() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uiOpens++
}

// Snapshot renders the ext block. dbPath is stat-ed for size only — the
// path itself never enters the payload.
func (c *Collector) Snapshot(dbPath string, semantic, syncEnabled bool, embedProvider string) *Ext {
	c.mu.Lock()
	defer c.mu.Unlock()
	ext := &Ext{
		SemanticEnabled: semantic,
		SyncEnabled:     syncEnabled,
		EmbedProvider:   embedProvider,
		GateRejected:    c.gate,
		UIOpens:         c.uiOpens,
		ServeMinutes:    int64(time.Since(c.started).Minutes()),
	}
	ext.Agent = c.agent
	for n := range c.clients {
		ext.Clients = append(ext.Clients, n)
	}
	sort.Strings(ext.Clients)
	if len(ext.Clients) > maxClients {
		ext.Clients = ext.Clients[:maxClients]
	}
	for _, code := range sortedErrorCodes(c.errors) {
		if len(ext.Errors) >= maxErrorCodes {
			break
		}
		if ext.Errors == nil {
			ext.Errors = map[string]int64{}
		}
		ext.Errors[code] = c.errors[code]
	}
	if fi, err := os.Stat(dbPath); err == nil {
		ext.DBSizeKB = fi.Size() / 1024
	}
	return ext
}

func sortedErrorCodes(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
