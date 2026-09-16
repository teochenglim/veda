package conformance

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/teochenglim/veda/internal/store"
)

// TestAC5_NoUndocumentedDrift is the CI spec-review gate: Veda must pass all
// of its own conformance suites on every change. Any drift between the drafts
// and behavior fails here — and must then be either fixed in the same release
// or, if externally visible, registered as a warning in
// spec/DEPRECATIONS.md before it can break anything.
func TestAC5_NoUndocumentedDrift(t *testing.T) {
	// draft-01: a fresh store satisfies storage conformance
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "veda.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if rep := RunStorage(dir); !rep.OK {
		t.Fatalf("storage conformance drift: %+v", rep.Results)
	}

	// draft-02 tools: veda's own MCP layer over stdio
	if rep := RunTools(helperCommand(t, "conform")); !rep.OK {
		t.Fatalf("tool-contract drift: %+v", rep.Results)
	}

	// draft-02 sync: the reference backend satisfies the wire contract
	if rep := RunSync(referenceEndpoint(t), SyncOptions{Token: "test-token"}); !rep.OK {
		t.Fatalf("sync-wire drift: %+v", rep.Results)
	}

	// the deprecation register must exist and carry the policy
	if _, err := os.Stat(filepath.Join("..", "..", "spec", "DEPRECATIONS.md")); err != nil {
		t.Fatal("spec/DEPRECATIONS.md is missing — the deprecation policy is part of the drift gate")
	}
}
