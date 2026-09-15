package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teochenglim/veda/internal/store"
)

func storeTest(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/veda.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// AC9/AC8: (default-off is asserted in the config package test.)

// AC8/AC9: preview shows the exact payload — counts only — and flush POSTs
// exactly the queued payloads to the endpoint.
func TestAC9_PreviewNoContentAndFlush(t *testing.T) {
	s := storeTest(t)
	// sensitive content goes into the DB; it must never appear in telemetry
	s.Remember(&store.Memory{Content: "my SECRET memory content about coffee"}, "mcp")
	s.RecordRecall(true)

	payload, err := Preview(s, "install-abc", "v0.1.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "SECRET") || strings.Contains(payload, "coffee") {
		t.Fatalf("preview leaks content: %s", payload)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(payload), &st); err != nil {
		t.Fatal(err)
	}
	if st["install_id"] != "install-abc" || st["memories"].(float64) != 1 {
		t.Fatalf("preview payload wrong: %v", st)
	}

	if err := QueueNow(s, "install-abc", "v0.1.0", nil); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("flush must POST, got %s", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
	}))
	defer srv.Close()
	if err := Flush(context.Background(), s, srv.URL); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("expected 1 flushed payload, got %d", len(bodies))
	}
	if queued, _ := s.ListTelemetry(); len(queued) != 0 {
		t.Fatal("successful flush must clear the queue")
	}
}

// AC9: GDPR erasure issues DELETE /?install_id=<id> against the worker.
func TestAC9_DeleteErasure(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.RawQuery
	}))
	defer srv.Close()
	if err := Delete(context.Background(), srv.URL, "install-xyz"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "install_id=install-xyz" {
		t.Fatalf("erasure request wrong: %s %s", gotMethod, gotPath)
	}
}
