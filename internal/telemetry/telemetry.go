// Package telemetry implements Veda's strictly opt-in anonymous usage
// reporting. Default is OFF; there is no auto-consent anywhere. Payloads
// contain counts only — never memories, conversations, keys, or identity.
//
// Flushes POST queued JSON payloads to the configured Cloudflare Worker
// endpoint (a separate repository). GDPR erasure is a DELETE to the same
// endpoint with ?install_id=<id>; `veda telemetry export` prints the local
// install id so a user can exercise that right.
package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/teochenglim/veda/internal/store"
)

// Preview returns the exact payload that would be sent on the next flush,
// without sending anything and without requiring consent.
func Preview(s *store.Store, installID, version string) (string, error) {
	st, err := s.CollectStats()
	if err != nil {
		return "", err
	}
	st.InstallID = installID
	st.Version = version
	st.OS = osName()
	st.Arch = archName()
	b, err := jsonMarshalIndent(st)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// QueueNow snapshots stats and stores the payload in the telemetry_queue
// table, ready for the next flush. Called by the serve loop.
func QueueNow(s *store.Store, installID, version string) error {
	payload, err := Preview(s, installID, version)
	if err != nil {
		return err
	}
	return s.QueueTelemetry(payload)
}

// Flush POSTs all queued payloads to endpoint and clears the queue on
// success. GDPR erasure (Delete) is a separate action.
func Flush(ctx context.Context, s *store.Store, endpoint string) error {
	payloads, err := s.ListTelemetry()
	if err != nil {
		return err
	}
	if len(payloads) == 0 {
		return nil
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, p := range payloads {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(p)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err // keep the queue; retry next cycle
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("telemetry endpoint returned %d", resp.StatusCode)
		}
	}
	return s.ClearTelemetry()
}

// Delete asks the endpoint to erase everything associated with installID
// (GDPR erasure), mirroring the Cloudflare Worker's DELETE handler.
func Delete(ctx context.Context, endpoint, installID string) error {
	url := strings.TrimRight(endpoint, "/") + "/?install_id=" + installID
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telemetry delete returned %d", resp.StatusCode)
	}
	log.Printf("veda: remote telemetry erased for install id %s", installID)
	return nil
}

// StartFlusher queues a payload every interval/2 and flushes every interval
// (default 24h) while `veda serve` runs. It does nothing when telemetry is
// disabled — the normal case.
func StartFlusher(ctx context.Context, s *store.Store, endpoint string, interval time.Duration, installID, version string) {
	if endpoint == "" {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
				if err := QueueNow(s, installID, version); err != nil {
					log.Printf("veda telemetry: queue: %v", err)
					continue
				}
				if err := Flush(ctx, s, endpoint); err != nil {
					log.Printf("veda telemetry: flush: %v", err)
				}
			}
		}
	}()
}
