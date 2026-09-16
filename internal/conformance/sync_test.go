package conformance

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func referenceEndpoint(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(ReferenceBackend("test-token"))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestAC3_ConformanceSyncPassesReference validates the suite against the
// reference fake backend — the executable form of the wire contract.
func TestAC3_ConformanceSyncPassesReference(t *testing.T) {
	rep := RunSync(referenceEndpoint(t), SyncOptions{Token: "test-token", UnpaidToken: "unpaid"})
	if !rep.OK {
		t.Fatalf("the reference backend violates draft-02 wire: %+v", rep.Results)
	}
	for _, name := range []string{
		"push-assigns-id", "push-monotonic", "pull-excludes-own",
		"pull-since-paging", "envelope-roundtrip", "auth-gate", "paid-gate-402",
	} {
		if res := result(t, rep, name); !res.OK {
			t.Fatalf("%s unexpectedly failed: %+v", name, res)
		}
	}
}

// TestSyncConformanceFailsOpenEndpoint proves the auth-gate check bites: an
// endpoint that serves unauthenticated requests fails with the rule named.
func TestSyncConformanceFailsOpenEndpoint(t *testing.T) {
	// the reference backend, wrapped so a missing Authorization header is
	// silently filled in — i.e., an endpoint that forgot its auth
	ref := ReferenceBackend("test-token")
	lax := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer test-token")
		}
		ref.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(lax)
	t.Cleanup(srv.Close)

	rep := RunSync(srv.URL, SyncOptions{Token: "test-token"})
	if rep.OK {
		t.Fatal("an endpoint without an auth gate must fail sync conformance")
	}
	res := result(t, rep, "auth-gate")
	if res.OK || !strings.Contains(res.Detail, "401/403") {
		t.Fatalf("auth-gate should name the violated rule, got: %+v", res)
	}
}
