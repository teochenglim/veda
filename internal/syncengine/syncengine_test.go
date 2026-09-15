package syncengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teochenglim/veda/internal/config"
	"github.com/teochenglim/veda/internal/store"
)

// fakeBackend implements the sync REST contract in memory (the external
// Supabase repo's behavior): POST assigns a globally monotonic id; GET
// returns other devices' bundles with id > since_id.
type fakeBackend struct {
	mu      sync.Mutex
	bodies  []string // raw POST bodies (for ciphertext assertions)
	bundles []struct {
		id       int64
		deviceID string
		env      Envelope
	}
	nextID int64
}

func (f *fakeBackend) handler() http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var env Envelope
			buf := make([]byte, r.ContentLength)
			r.Body.Read(buf)
			f.bodies = append(f.bodies, string(buf))
			if err := json.Unmarshal(buf, &env); err != nil {
				http.Error(rw, "bad envelope", 400)
				return
			}
			f.nextID++
			f.bundles = append(f.bundles, struct {
				id       int64
				deviceID string
				env      Envelope
			}{f.nextID, env.DeviceID, env})
			json.NewEncoder(rw).Encode(map[string]int64{"id": f.nextID})
		case http.MethodGet:
			var since int64
			fmt.Sscanf(r.URL.Query().Get("since_id"), "%d", &since)
			device := r.URL.Query().Get("device_id")
			out := []Received{}
			for _, b := range f.bundles {
				if b.id > since && b.deviceID != device {
					out = append(out, Received{ID: b.id, DeviceID: b.deviceID, Envelope: b.env})
				}
			}
			json.NewEncoder(rw).Encode(map[string][]Received{"bundles": out})
		}
	}
}

func engineFor(t *testing.T, s *store.Store, url string) *Engine {
	t.Helper()
	return &Engine{Store: s, Cfg: config.SyncConfig{
		Enabled: true, URL: url,
		TokenEnv: "VEDA_SYNC_TOKEN", PassphraseEnv: "VEDA_SYNC_PASSPHRASE",
		IntervalMinutes: 15,
	}}
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/veda.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func snapshot(s *store.Store) ([]string, []store.Tombstone) {
	var sig []string
	mems, _ := s.List("", "", zeroTime(), zeroTime(), 0, true)
	for _, m := range mems {
		sig = append(sig, fmt.Sprintf("%s|%s|%s|%v|%s", m.ID, m.Content, m.Status, m.Deleted, m.SupersededBy))
	}
	sort.Strings(sig)
	tombs, _ := s.AllTombstones()
	return sig, tombs
}

func zeroTime() time.Time { return time.Time{} }

// AC1: the server-side storage of a sync bundle contains no plaintext —
// only the routing envelope plus opaque ciphertext.
func TestAC1_ServerSeesNoPlaintext(t *testing.T) {
	fb := &fakeBackend{}
	srv := httptest.NewServer(fb.handler())
	defer srv.Close()

	s := newStore(t)
	s.Remember(&store.Memory{Content: "User's secret home address is 1 Hidden Lane"}, "cursor")
	t.Setenv("VEDA_SYNC_PASSPHRASE", "correct horse battery staple")
	t.Setenv("VEDA_SYNC_TOKEN", "paid-plan-token")
	eng := engineFor(t, s, srv.URL)
	if _, err := eng.Push(context.Background()); err != nil {
		t.Fatal(err)
	}

	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.bodies) != 1 {
		t.Fatalf("expected 1 pushed envelope, got %d", len(fb.bodies))
	}
	body := fb.bodies[0]
	if strings.Contains(body, "Hidden Lane") || strings.Contains(body, "secret home") {
		t.Fatal("sync bundle leaked plaintext to the server")
	}
	var env Envelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	if env.Ciphertext == "" || env.Crypto.Salt == "" || env.Nonce == "" {
		t.Fatalf("envelope missing crypto fields: %+v", env)
	}
	// decryptability round-trip: same passphrase, any device
	b, err := OpenEnvelope(env, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Memories) != 1 || !strings.Contains(b.Memories[0].Content, "Hidden Lane") {
		t.Fatalf("bundle round-trip failed: %+v", b)
	}
	// wrong passphrase can never open it
	if _, err := OpenEnvelope(env, "wrong passphrase"); err == nil {
		t.Fatal("wrong passphrase must fail")
	}
}

// AC2: two devices making independent edits converge to identical state.
func TestAC2_TwoDevicesConverge(t *testing.T) {
	fb := &fakeBackend{}
	srv := httptest.NewServer(fb.handler())
	defer srv.Close()

	a, b := newStore(t), newStore(t)
	t.Setenv("VEDA_SYNC_PASSPHRASE", "shared-passphrase")
	t.Setenv("VEDA_SYNC_TOKEN", "tok")
	ea, eb := engineFor(t, a, srv.URL), engineFor(t, b, srv.URL)
	ctx := context.Background()

	// independent writes on both devices
	a.Remember(&store.Memory{Content: "User prefers aisle seats on long flights"}, "cursor")
	b.Remember(&store.Memory{Content: "User runs marathons on weekends"}, "claude")

	// A pushes, B pulls; B pushes, A pulls
	if _, err := ea.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := eb.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := eb.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ea.Pull(ctx); err != nil {
		t.Fatal(err)
	}

	// both edits exist on both devices
	for _, s := range []*store.Store{a, b} {
		mems, _ := s.List("", "", zeroTime(), zeroTime(), 0, false)
		if len(mems) != 2 {
			t.Fatalf("device should hold both memories, has %d", len(mems))
		}
	}

	// conflicting writes converge too: A supersedes on its side
	a.Remember(&store.Memory{Content: "User moved to Tokyo last month for work"}, "cursor") // A has no Singapore memory — plain write
	b.Remember(&store.Memory{Content: "User lives in Singapore with family"}, "claude")
	if _, err := eb.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ea.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ea.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := eb.Pull(ctx); err != nil {
		t.Fatal(err)
	}

	// convergence: identical snapshots (ids, contents, statuses, deletions)
	sigA, tombsA := snapshot(a)
	sigB, tombsB := snapshot(b)
	if strings.Join(sigA, ";") != strings.Join(sigB, ";") {
		t.Fatalf("devices diverged:\nA: %v\nB: %v", sigA, sigB)
	}
	if len(tombsA) != len(tombsB) {
		t.Fatalf("tombstone sets diverged: %d vs %d", len(tombsA), len(tombsB))
	}
}

// AC3: deleting on device A removes the memory on device B.
func TestAC3_TombstoneRemovesOnOtherDevice(t *testing.T) {
	fb := &fakeBackend{}
	srv := httptest.NewServer(fb.handler())
	defer srv.Close()

	a, b := newStore(t), newStore(t)
	t.Setenv("VEDA_SYNC_PASSPHRASE", "shared-passphrase")
	ea, eb := engineFor(t, a, srv.URL), engineFor(t, b, srv.URL)
	ctx := context.Background()

	// seed and sync a memory to both devices
	a.Remember(&store.Memory{Content: "User's home wifi password is hunter2"}, "cursor")
	if _, err := ea.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := eb.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if mems, _ := b.List("", "", zeroTime(), zeroTime(), 0, true); len(mems) != 1 {
		t.Fatal("seed failed")
	}

	// delete on A, push, pull on B
	ok, err := a.Forget(mustFirst(t, a), "cursor", "left home")
	if err != nil || !ok {
		t.Fatalf("forget: %v %v", ok, err)
	}
	if _, err := ea.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := eb.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if mems, _ := b.List("", "", zeroTime(), zeroTime(), 0, true); len(mems) != 0 {
		t.Fatal("deleted memory must be removed on device B")
	}
	// and B records the tombstone locally
	tombs, _ := b.AllTombstones()
	if len(tombs) != 1 {
		t.Fatalf("device B must record the tombstone, has %d", len(tombs))
	}
}

func mustFirst(t *testing.T, s *store.Store) string {
	t.Helper()
	mems, err := s.List("", "", zeroTime(), zeroTime(), 0, false)
	if err != nil || len(mems) == 0 {
		t.Fatal("no memories")
	}
	return mems[0].ID
}

// AC4: sync fully disabled ⇒ zero network calls.
func TestAC4_DisabledSyncNeverTouchesNetwork(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	s := newStore(t)
	s.Remember(&store.Memory{Content: "local only memory"}, "cursor")
	t.Setenv("VEDA_SYNC_PASSPHRASE", "pw")
	// Enabled: false — the default
	eng := &Engine{Store: s, Cfg: config.SyncConfig{Enabled: false, URL: srv.URL}}
	if _, err := eng.Push(context.Background()); err == nil {
		t.Fatal("push on disabled sync must error")
	}
	if _, err := eng.Pull(context.Background()); err == nil {
		t.Fatal("pull on disabled sync must error")
	}
	if _, err := eng.Status(); err != nil {
		t.Fatal(err) // status is local-only by design
	}
	if hits != 0 {
		t.Fatalf("disabled sync made %d network calls", hits)
	}
}

// Tombstone vs newer edit: last-writer-wins — an edit after the delete wins.
func TestMergeTombstoneLosesToNewerEdit(t *testing.T) {
	s := newStore(t)
	id, _ := s.Remember(&store.Memory{Content: "original content of the memory"}, "cursor")
	mem, _ := s.Get(id)
	future := mem.UpdatedAt + 100
	applied, err := s.MergeIncoming(
		[]*store.Memory{{ID: id, Type: "fact", Content: "edited on another device",
			UpdatedAt: future, CreatedAt: mem.CreatedAt, Confidence: 1, Salience: 0.5, Status: "active"}},
		[]store.Tombstone{{MemoryID: id, DeletedAt: mem.UpdatedAt + 50}})
	if err != nil {
		t.Fatal(err)
	}
	if applied == 0 {
		t.Fatal("newer edit must apply over the older tombstone")
	}
	m, err := s.Get(id)
	if err != nil || m.Content != "edited on another device" {
		t.Fatalf("newer edit must survive the tombstone: %+v (%v)", m, err)
	}
}

// Bundle order must not matter: merging is per-row LWW, deterministic.
func TestMergeIsOrderIndependent(t *testing.T) {
	mk := func() *store.Store { return newStore(t) }
	incoming := []*store.Memory{
		{ID: "m1", Content: "newer version of m1", UpdatedAt: 200, CreatedAt: 100, Status: "active"},
		{ID: "m2", Content: "older version of m2", UpdatedAt: 120, CreatedAt: 100, Status: "active"},
	}
	s1, s2 := mk(), mk()
	s1.MergeIncoming(incoming, nil)
	// reversed order
	s2.MergeIncoming([]*store.Memory{incoming[1], incoming[0]}, nil)
	a1, _ := s1.List("", "", zeroTime(), zeroTime(), 0, true)
	a2, _ := s2.List("", "", zeroTime(), zeroTime(), 0, true)
	if len(a1) != len(a2) {
		t.Fatal("order changed the outcome")
	}
	for i := range a1 {
		if a1[i].ID != a2[i].ID || a1[i].Content != a2[i].Content || a1[i].UpdatedAt != a2[i].UpdatedAt {
			t.Fatalf("order changed the outcome at %d: %+v vs %+v", i, a1[i], a2[i])
		}
	}
}

// Paid-tier gate: the backend's 402 surfaces as ErrPaymentRequired.
func TestPaymentGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, "upgrade", http.StatusPaymentRequired)
	}))
	defer srv.Close()
	s := newStore(t)
	s.Remember(&store.Memory{Content: "some local memory to push"}, "cursor")
	t.Setenv("VEDA_SYNC_PASSPHRASE", "pw")
	t.Setenv("VEDA_SYNC_TOKEN", "expired")
	eng := engineFor(t, s, srv.URL)
	_, err := eng.Push(context.Background())
	if !errors.Is(err, ErrPaymentRequired) {
		t.Fatalf("expected ErrPaymentRequired, got %v", err)
	}
}

// Envelope honesty: passphrase lives only in the env var, never on disk.
func TestPassphraseNeverPersisted(t *testing.T) {
	s := newStore(t)
	t.Setenv("VEDA_SYNC_PASSPHRASE", "top-secret-passphrase")
	eng := engineFor(t, s, "http://unused.invalid")
	if _, err := eng.deviceID(); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.salt(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"device_id", "kdf_salt"} {
		v, _ := s.SyncGet(key)
		if strings.Contains(v, "top-secret") {
			t.Fatal("passphrase material leaked into sync_state")
		}
	}
	if os.Getenv("VEDA_SYNC_PASSPHRASE") == "" {
		t.Skip("env required for this test")
	}
}
