package conformance

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/teochenglim/veda/internal/store"
	"github.com/teochenglim/veda/internal/syncengine"
)

// conformancePassphrase seals the suite's envelopes; the reference vector
// (spec/vectors/) documents its own fixed passphrase.
const conformancePassphrase = "uomp-conformance-passphrase"

// SyncOptions tunes the sync wire suite. Token is the Bearer plan token the
// endpoint expects; when UnpaidToken is set, the suite additionally checks
// that the endpoint answers that token with the 402 paid gate.
type SyncOptions struct {
	Token       string
	UnpaidToken string
}

// RunSync validates the sync backend at endpoint against the UOMP draft-02
// wire contract (spec/uomp-draft-02.md §3): ingest with server-assigned
// monotonic ids, since_id paging, device exclusion, envelope fidelity, and
// the auth gate. Failures name the violated rule.
func RunSync(endpoint string, opts SyncOptions) *Report {
	rep := &Report{Dir: endpoint}
	add := func(name string, ok bool, format string, args ...any) {
		detail := ""
		if !ok {
			detail = fmt.Sprintf(format, args...)
		}
		rep.Results = append(rep.Results, Result{Name: name, OK: ok, Detail: detail})
	}

	client := syncengine.NewClient(endpoint, opts.Token)
	ctx := context.Background()

	salt := conformanceSalt()
	mkEnvelope := func(device, content string) syncengine.Envelope {
		env, err := syncengine.BuildEnvelope(syncengine.Bundle{
			DeviceID: device,
			Memories: []*store.Memory{{ID: "mem_" + device + "_" + content[:6], Type: "fact", Content: content, Confidence: 0.9, Salience: 0.8, CreatedAt: 1789440000, UpdatedAt: 1789440000}},
		}, conformancePassphrase, salt, 1789440000)
		if err != nil {
			panic(err) // fixture construction; cannot fail with fixed inputs
		}
		return env
	}

	const deviceA = "uomp-conformance-a"
	const deviceB = "uomp-conformance-b"

	idA, err := client.Push(ctx, mkEnvelope(deviceA, "confa1 memory body"))
	if err != nil {
		add("push-assigns-id", false, "push from device A failed: %v", err)
		return rep
	}
	if idA < 1 {
		add("push-assigns-id", false, "server-assigned bundle ids must be positive, got %d (rule §3.1)", idA)
		return rep
	}
	add("push-assigns-id", true, "")

	idB, err := client.Push(ctx, mkEnvelope(deviceB, "confb1 memory body"))
	if err != nil {
		add("push-monotonic", false, "push from device B failed: %v", err)
		return rep
	}
	if idB <= idA {
		add("push-monotonic", false, "bundle ids must be strictly monotonic: %d then %d (rule §3.1)", idA, idB)
		return rep
	}
	add("push-monotonic", true, "")

	// device exclusion + paging
	received, err := client.Pull(ctx, 0, deviceA)
	if err != nil {
		add("pull-excludes-own", false, "pull as device A failed: %v", err)
		return rep
	}
	ownLeak := false
	hasOther := false
	for _, r := range received {
		if r.DeviceID == deviceA {
			ownLeak = true
		}
		if r.DeviceID == deviceB {
			hasOther = true
		}
	}
	if ownLeak || !hasOther {
		add("pull-excludes-own", false,
			"pull as device A must return other devices' bundles only (own leaked: %v, other present: %v) (rule §3.2)", ownLeak, hasOther)
		return rep
	}
	add("pull-excludes-own", true, "")

	newer, err := client.Pull(ctx, idB, deviceA)
	if err != nil {
		add("pull-since-paging", false, "pull with since_id failed: %v", err)
		return rep
	}
	stale := false
	for _, r := range newer {
		if r.ID <= idB {
			stale = true
		}
	}
	if len(newer) != 0 || stale {
		add("pull-since-paging", false, "pull since_id=%d returned %d bundles (rule: only ids strictly newer) (§3.2)", idB, len(newer))
		return rep
	}
	page, err := client.Pull(ctx, idA, deviceA)
	if err != nil || len(page) != 1 || page[0].ID != idB {
		add("pull-since-paging", false, "pull since_id=%d should return exactly device B's bundle %d (got %v, err %v) (§3.2)", idA, idB, page, err)
		return rep
	}
	add("pull-since-paging", true, "")

	// envelope fidelity: decrypt what device B pushed, as device A would
	var fetched *syncengine.Received
	for i := range page {
		if page[i].ID == idB {
			fetched = &page[i]
		}
	}
	if fetched == nil {
		add("envelope-roundtrip", false, "pulled page lost device B's bundle")
		return rep
	}
	if msg := envelopeWireFaults(fetched.Envelope); msg != "" {
		add("envelope-roundtrip", false, "%s (rule §3.3)", msg)
		return rep
	}
	bundle, err := syncengine.OpenEnvelope(fetched.Envelope, conformancePassphrase)
	if err != nil {
		add("envelope-roundtrip", false, "pulled envelope does not decrypt under the shared passphrase: %v (rule §3.3)", err)
		return rep
	}
	if bundle.DeviceID != deviceB || len(bundle.Memories) != 1 || bundle.Memories[0].Content != "confb1 memory body" {
		add("envelope-roundtrip", false, "decrypted bundle does not match what device B pushed (rule §3.3)")
		return rep
	}
	add("envelope-roundtrip", true, "")

	// auth gate: no credentials, no service
	gate := func(method, url string) int {
		req, _ := http.NewRequestWithContext(ctx, method, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return -1
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := gate(http.MethodGet, strings.TrimRight(endpoint, "/")+"/bundles?since_id=0&device_id="+deviceA); code >= 200 && code < 400 {
		add("auth-gate", false, "GET without credentials returned %d — unauthenticated requests must be rejected with 401/403 (rule §3.5)", code)
	} else if code := gate(http.MethodPost, strings.TrimRight(endpoint, "/")+"/bundles"); code >= 200 && code < 400 {
		add("auth-gate", false, "POST without credentials returned %d — unauthenticated requests must be rejected (rule §3.5)", code)
	} else {
		add("auth-gate", true, "")
	}

	// optional 402 paid-gate probe
	if opts.UnpaidToken != "" {
		unpaid := syncengine.NewClient(endpoint, opts.UnpaidToken)
		_, err := unpaid.Push(ctx, mkEnvelope(deviceA, "unpaid probe"))
		switch {
		case err == nil:
			add("paid-gate-402", false, "unentitled token pushed successfully — endpoint must answer 402 for unentitled tokens (rule §3.4)")
		case err == syncengine.ErrPaymentRequired:
			add("paid-gate-402", true, "")
		default:
			add("paid-gate-402", false, "unentitled token got %v; the contract answer is 402 (rule §3.4)", err)
		}
	}

	rep.OK = true
	for _, res := range rep.Results {
		if !res.OK {
			rep.OK = false
		}
	}
	return rep
}

// envelopeWireFaults names the first wire-shape fault in an envelope.
func envelopeWireFaults(env syncengine.Envelope) string {
	if env.DeviceID == "" {
		return "envelope missing device_id"
	}
	if env.CreatedAt <= 0 {
		return "envelope missing created_at"
	}
	if env.Crypto.Algo != "xsalsa20poly1305" || env.Crypto.KDF != "scrypt" {
		return fmt.Sprintf("unexpected crypto parameters algo=%q kdf=%q", env.Crypto.Algo, env.Crypto.KDF)
	}
	if len(env.Crypto.Salt) < 16 { // ≥ 8 decoded bytes ⇒ ≥ 16 hex chars
		return "kdf salt shorter than 8 bytes"
	}
	nonce, err := hex.DecodeString(env.Nonce)
	if err != nil || len(nonce) != 24 {
		return "nonce must be 24 bytes of hex"
	}
	if _, err := base64.StdEncoding.DecodeString(env.Ciphertext); err != nil {
		return "ciphertext must be base64"
	}
	return ""
}

func conformanceSalt() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// syncMem removed — fixtures use store.Memory directly (the wire type is the
// JSON shape of store rows).

// ReferenceBackend is an in-memory implementation of the draft-02 sync wire
// contract — the executable form of spec §3 and the fixture the sync
// conformance suite is validated against. token is the entitled Bearer
// token; the conventional token "unpaid" is answered with the 402 gate.
func ReferenceBackend(token string) http.Handler {
	type stored struct {
		id       int64
		deviceID string
		env      syncengine.Envelope
	}
	var (
		mu      sync.Mutex
		nextID  int64 = 1
		bundles []stored
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case r.Header.Get("Authorization") == "":
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		case bearer == "unpaid":
			// the conventional unentitled token: the contract's 402 gate
			http.Error(w, "sync is a paid feature", http.StatusPaymentRequired)
			return
		case bearer != token:
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var env syncengine.Envelope
			if err := json.NewDecoder(r.Body).Decode(&env); err != nil || env.DeviceID == "" || env.Ciphertext == "" {
				http.Error(w, "malformed envelope", http.StatusBadRequest)
				return
			}
			id := nextID
			nextID++
			bundles = append(bundles, stored{id: id, deviceID: env.DeviceID, env: env})
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":%d}`, id)
		case http.MethodGet:
			var since int64
			fmt.Sscanf(r.URL.Query().Get("since_id"), "%d", &since)
			device := r.URL.Query().Get("device_id")
			out := []map[string]any{}
			for _, b := range bundles {
				if b.id > since && b.deviceID != device {
					out = append(out, map[string]any{"id": b.id, "device_id": b.deviceID, "envelope": b.env})
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"bundles": out})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
