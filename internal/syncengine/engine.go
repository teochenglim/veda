package syncengine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/teochenglim/veda/internal/config"
	"github.com/teochenglim/veda/internal/store"
)

// Engine drives hosted sync for one local store: push local changes as a
// sealed bundle, pull and merge other devices' bundles. All orchestration
// lives here; the store only provides change feeds and the deterministic
// merge (MergeIncoming).
type Engine struct {
	Store *store.Store
	Cfg   config.SyncConfig
	HTTP  *http.Client // nil ⇒ default client
}

// Enabled reports whether sync is on. When false, every entry point returns
// an error WITHOUT any network activity (AC4).
func (e *Engine) Enabled() bool { return e.Cfg.Enabled && e.Cfg.URL != "" }

func (e *Engine) client() *Client {
	return NewClient(e.Cfg.URL, os.Getenv(e.Cfg.TokenEnv))
}

func (e *Engine) passphrase() (string, error) {
	p := os.Getenv(e.Cfg.PassphraseEnv)
	if p == "" {
		return "", fmt.Errorf("sync passphrase not set — export %s", e.Cfg.PassphraseEnv)
	}
	return p, nil
}

// deviceID lazily mints and persists this install's sync identity.
func (e *Engine) deviceID() (string, error) {
	id, err := e.Store.SyncGet("device_id")
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id = "dev_" + hex.EncodeToString(b)
	return id, e.Store.SyncSet("device_id", id)
}

func (e *Engine) salt() (string, error) {
	salt, err := e.Store.SyncGet("kdf_salt")
	if err != nil {
		return "", err
	}
	if salt != "" {
		return salt, nil
	}
	salt, err = newSalt()
	if err != nil {
		return "", err
	}
	return salt, e.Store.SyncSet("kdf_salt", salt)
}

// Push seals and uploads local changes since the last successful push.
func (e *Engine) Push(ctx context.Context) (string, error) {
	if !e.Enabled() {
		return "", fmt.Errorf("sync is disabled — enable it in %s first", config.ConfigPath())
	}
	pass, err := e.passphrase()
	if err != nil {
		return "", err
	}
	deviceID, err := e.deviceID()
	if err != nil {
		return "", err
	}
	salt, err := e.salt()
	if err != nil {
		return "", err
	}
	wmStr, err := e.Store.SyncGet("last_push_seq")
	if err != nil {
		return "", err
	}
	var watermark int64
	fmt.Sscanf(wmStr, "%d", &watermark)

	memories, err := e.Store.MemoriesSince(watermark)
	if err != nil {
		return "", err
	}
	tombs, err := e.Store.TombstonesSince(watermark)
	if err != nil {
		return "", err
	}
	if len(memories) == 0 && len(tombs) == 0 {
		return "nothing to push", nil
	}
	env, err := BuildEnvelope(Bundle{DeviceID: deviceID, Memories: memories, Tombstones: tombs}, pass, salt, time.Now().Unix())
	if err != nil {
		return "", err
	}
	id, err := e.client().Push(ctx, env)
	if err != nil {
		return "", err
	}
	head, err := e.Store.ChangeLogHead()
	if err != nil {
		return "", err
	}
	if err := e.Store.SyncSet("last_push_seq", fmt.Sprint(head)); err != nil {
		return "", err
	}
	return fmt.Sprintf("pushed %d memories, %d tombstones (bundle %d)",
		len(memories), len(tombs), id), nil
}

// Pull fetches and merges other devices' bundles, oldest first.
func (e *Engine) Pull(ctx context.Context) (string, error) {
	if !e.Enabled() {
		return "", fmt.Errorf("sync is disabled — enable it in %s first", config.ConfigPath())
	}
	pass, err := e.passphrase()
	if err != nil {
		return "", err
	}
	deviceID, err := e.deviceID()
	if err != nil {
		return "", err
	}
	cursorStr, err := e.Store.SyncGet("last_pulled_id")
	if err != nil {
		return "", err
	}
	var cursor int64
	fmt.Sscanf(cursorStr, "%d", &cursor)

	received, err := e.client().Pull(ctx, cursor, deviceID)
	if err != nil {
		return "", err
	}
	if len(received) == 0 {
		return "already up to date", nil
	}
	applied := 0
	maxID := cursor
	for _, r := range received {
		bundle, err := OpenEnvelope(r.Envelope, pass)
		if err != nil {
			return "", fmt.Errorf("bundle %d: %w", r.ID, err)
		}
		n, err := e.Store.MergeIncoming(bundle.Memories, bundle.Tombstones)
		if err != nil {
			return "", fmt.Errorf("bundle %d: %w", r.ID, err)
		}
		applied += n
		if r.ID > maxID {
			maxID = r.ID
		}
	}
	if err := e.Store.SyncSet("last_pulled_id", fmt.Sprint(maxID)); err != nil {
		return "", err
	}
	return fmt.Sprintf("merged %d changes from %d bundle(s)", applied, len(received)), nil
}

// Status is the `veda sync status` snapshot.
type Status struct {
	Enabled              bool   `json:"enabled"`
	Endpoint             string `json:"endpoint"`
	DeviceID             string `json:"device_id"`
	TokenConfigured      bool   `json:"token_configured"`
	PassphraseConfigured bool   `json:"passphrase_configured"`
	LastPushSeq          int64  `json:"last_push_seq"`
	LastPulledID         int64  `json:"last_pulled_id"`
	PendingMemories      int64  `json:"pending_memories"`
	PendingTombstones    int64  `json:"pending_tombstones"`
}

// Status reports sync bookkeeping. Never touches the network.
func (e *Engine) Status() (Status, error) {
	st := Status{Enabled: e.Enabled(), Endpoint: e.Cfg.URL,
		TokenConfigured: os.Getenv(e.Cfg.TokenEnv) != "", PassphraseConfigured: os.Getenv(e.Cfg.PassphraseEnv) != ""}
	var err error
	if st.DeviceID, err = e.Store.SyncGet("device_id"); err != nil {
		return st, err
	}
	if v, err := e.Store.SyncGet("last_push_seq"); err != nil {
		return st, err
	} else {
		fmt.Sscanf(v, "%d", &st.LastPushSeq)
	}
	if v, err := e.Store.SyncGet("last_pulled_id"); err != nil {
		return st, err
	} else {
		fmt.Sscanf(v, "%d", &st.LastPulledID)
	}
	st.PendingMemories, st.PendingTombstones, err = e.Store.SyncPendingCounts(st.LastPushSeq)
	return st, err
}

// RunBackgroundLoop merges and pushes every interval while `veda serve`
// runs. Does nothing when sync is disabled.
func (e *Engine) RunBackgroundLoop(ctx context.Context) {
	if !e.Enabled() {
		return
	}
	interval := time.Duration(e.Cfg.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
				if _, err := e.Pull(ctx); err != nil {
					log.Printf("veda sync pull: %v", err)
				}
				if _, err := e.Push(ctx); err != nil {
					log.Printf("veda sync push: %v", err)
				}
			}
		}
	}()
}
