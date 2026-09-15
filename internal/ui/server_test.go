package ui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teochenglim/veda/internal/store"
)

func uiTest(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/veda.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	sv := &Server{Store: s}
	ts := httptest.NewServer(sv.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func get(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", url, resp.StatusCode)
	}
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
}

func post(t *testing.T, url string, body any, want int) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("POST %s: got %d want %d", url, resp.StatusCode, want)
	}
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return m
}

// AC7: the review UI serves the four tabs' data: Pending, All, Audit,
// Digest — and supports approve / edit / delete from the browser.
func TestAC7_ReviewUITabsAndActions(t *testing.T) {
	ts, s := uiTest(t)

	// seed: one memory, one pending candidate, one audit entry
	id, _ := s.Remember(&store.Memory{Content: "User runs marathons", Type: "fact"}, "mcp")
	s.InsertPending([]store.PendingTurn{{SessionID: "s", Role: "user", Content: "I prefer morning runs before 7am", GateScore: 0.5}})

	// index page (the SPA with the four tabs)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	for _, tab := range []string{"Pending", "All", "Audit", "Digest"} {
		if !strings.Contains(buf.String(), tab) {
			t.Errorf("index page missing tab %q", tab)
		}
	}

	// Pending tab data
	var pending []*store.PendingTurn
	get(t, ts.URL+"/api/pending", &pending)
	if len(pending) != 1 {
		t.Fatalf("pending tab: %d", len(pending))
	}

	// approve a pending turn (with human-edited text)
	post(t, ts.URL+"/api/pending/approve", map[string]any{
		"id": pending[0].ID, "content": "User prefers morning runs before 7am",
	}, 200)
	var mems []*store.Memory
	get(t, ts.URL+"/api/memories", &mems)
	if len(mems) != 2 {
		t.Fatalf("approve must create a memory, have %d", len(mems))
	}

	// All tab: edit a memory
	post(t, ts.URL+"/api/memories/update", map[string]any{"id": id, "content": "User runs ultramarathons"}, 200)
	var after []*store.Memory
	get(t, ts.URL+"/api/memories", &after)
	for _, m := range after {
		if m.ID == id && m.Content != "User runs ultramarathons" {
			t.Fatal("edit did not stick")
		}
	}

	// delete from the All tab
	post(t, ts.URL+"/api/memories/delete", map[string]any{"id": id}, 200)
	get(t, ts.URL+"/api/memories", &after)
	if len(after) != 1 {
		t.Fatalf("delete must remove from All, have %d", len(after))
	}

	// Audit tab reflects everything
	var audit []*store.AuditEntry
	get(t, ts.URL+"/api/audit", &audit)
	if len(audit) < 3 {
		t.Fatalf("audit must show create/update/forget, have %d entries", len(audit))
	}

	// Digest tab: counts only
	var digest map[string]any
	get(t, ts.URL+"/api/digest", &digest)
	if digest["memories"].(float64) != 1 {
		t.Fatalf("digest wrong: %v", digest)
	}

	// reject path
	s.InsertPending([]store.PendingTurn{{Role: "user", Content: "junk candidate"}})
	var pend2 []*store.PendingTurn
	get(t, ts.URL+"/api/pending", &pend2)
	post(t, ts.URL+"/api/pending/reject", map[string]any{"id": pend2[0].ID}, 200)
	get(t, ts.URL+"/api/pending", &pend2)
	if len(pend2) != 0 {
		t.Fatal("reject must discard the candidate")
	}
}

// AC3 (v0.3): the Conflicts tab lists detected supersession pairs and the
// keep-both / prefer-old / prefer-new resolutions are honored and audited.
func TestAC3_ConflictsTabAndResolutions(t *testing.T) {
	ts, s := uiTest(t)

	// a contradicting write creates a conflict automatically
	s.Remember(&store.Memory{Content: "User lives in Singapore with their family"}, "cursor")
	s.Remember(&store.Memory{Content: "User moved to Tokyo last month for work"}, "claude")

	var conflicts []*store.Conflict
	get(t, ts.URL+"/api/conflicts", &conflicts)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict pair, got %d", len(conflicts))
	}
	if conflicts[0].Resolution != "new" {
		t.Fatalf("auto-resolution must default to new, got %q", conflicts[0].Resolution)
	}

	// keep both: both memories become active again
	post(t, ts.URL+"/api/conflicts/resolve", map[string]any{"id": conflicts[0].ID, "resolution": "both"}, 200)
	var mems []*store.Memory
	get(t, ts.URL+"/api/memories", &mems)
	if len(mems) != 2 {
		t.Fatalf("keep-both must reinstate the old memory, %d active", len(mems))
	}
	get(t, ts.URL+"/api/conflicts", &conflicts)
	if conflicts[0].Resolution != "both" {
		t.Fatalf("resolution not recorded: %q", conflicts[0].Resolution)
	}

	// prefer-old: the new memory is superseded instead
	post(t, ts.URL+"/api/conflicts/resolve", map[string]any{"id": conflicts[0].ID, "resolution": "old"}, 200)
	get(t, ts.URL+"/api/memories", &mems)
	if len(mems) != 1 || !strings.Contains(mems[0].Content, "Singapore") {
		t.Fatalf("prefer-old must reinstate Singapore, got %+v", mems)
	}

	// invalid resolution is a 400
	post(t, ts.URL+"/api/conflicts/resolve", map[string]any{"id": conflicts[0].ID, "resolution": "nuke"}, 400)

	// resolution audited
	var audit []*store.AuditEntry
	get(t, ts.URL+"/api/audit", &audit)
	resolves := 0
	for _, a := range audit {
		if a.Action == "resolve" {
			resolves++
		}
	}
	if resolves < 2 {
		t.Fatalf("resolutions must be audited, got %d", resolves)
	}
}
