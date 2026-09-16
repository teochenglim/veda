package conformance

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teochenglim/veda/internal/store"
	_ "modernc.org/sqlite"
)

// newStore creates a fresh store via the real writer (the v0.7 code paths)
// in a temp dir and returns the dir plus the open store.
func newStore(t *testing.T) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "veda.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return dir, s
}

func remember(t *testing.T, s *store.Store, content string) string {
	t.Helper()
	id, err := s.Remember(&store.Memory{Type: "preference", Content: content}, "test")
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	return id
}

func result(t *testing.T, rep *Report, name string) Result {
	t.Helper()
	for _, res := range rep.Results {
		if res.Name == name {
			return res
		}
	}
	t.Fatalf("no %q check in report: %+v", name, rep.Results)
	return Result{}
}

func rawDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "veda.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// AC3: a store created by the current writer (v0.7.x code paths — including
// soft delete and a superseded row) validates as draft-01 compliant.
func TestAC3_ConformanceValidatesV07Store(t *testing.T) {
	dir, s := newStore(t)
	remember(t, s, "User prefers window seats on flights")
	remember(t, s, "User drinks tea, not coffee")
	id := remember(t, s, "User is learning Go")
	if ok, err := s.Forget(id, "test", "ac3 fixture"); err != nil || !ok {
		t.Fatalf("forget: %v %v", ok, err)
	}
	// a superseded row exercises the status/superseded_by columns
	db := rawDB(t, dir)
	if _, err := db.Exec(`UPDATE memories SET status = 'superseded', superseded_by = (SELECT id FROM memories LIMIT 1)
		WHERE id = (SELECT id FROM memories WHERE deleted = 0 LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	rep := RunStorage(dir)
	if !rep.OK {
		t.Fatalf("expected draft-01 compliant, got: %+v", rep.Results)
	}
	for _, name := range []string{"db-open", "schema-tables", "schema-columns", "fts-triggers", "fts-sync", "export-shape"} {
		if res := result(t, rep, name); !res.OK {
			t.Fatalf("%s unexpectedly failed: %+v", name, res)
		}
	}
}

// AC4: an export document produced by the current writer validates against
// the draft-01 export shape — including an empty store (arrays, never null).
func TestAC4_ExportMatchesDraft01Shape(t *testing.T) {
	_, empty := newStore(t)
	data, err := empty.Export()
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"memories", "turns", "audit"} {
		if strings.TrimSpace(string(top[key])) == "null" {
			t.Fatalf("empty store serialized %q as null — draft-01 requires arrays", key)
		}
	}
	if viols := ValidateExport(data); len(viols) > 0 {
		t.Fatalf("empty export violates draft-01: %v", viols)
	}

	dir, s := newStore(t)
	remember(t, s, "User prefers window seats on flights")
	keep := remember(t, s, "User drinks tea, not coffee")
	if ok, err := s.Forget(keep, "test", ""); err != nil || !ok {
		t.Fatalf("forget: %v %v", ok, err)
	}
	_ = dir
	populated, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	if viols := ValidateExport(populated); len(viols) > 0 {
		t.Fatalf("populated export violates draft-01: %v", viols)
	}

	// negatives: wrong version, wrong types
	if viols := ValidateExport([]byte(`{"version":"2","exported_at":1,"memories":[],"turns":[],"audit":[]}`)); len(viols) == 0 {
		t.Fatal("expected version violation")
	}
	if viols := ValidateExport([]byte(`{"version":"1","exported_at":"soon","memories":[],"turns":[],"audit":[]}`)); len(viols) == 0 {
		t.Fatal("expected exported_at type violation")
	}
	if viols := ValidateExport([]byte(`{"version":"1","exported_at":1,"memories":[{"id":"mem_x"}],"turns":[],"audit":[]}`)); len(viols) == 0 {
		t.Fatal("expected memory field violations")
	}
}

// AC5: deliberately corrupted stores fail conformance with the reason named.
func TestAC5_CorruptStoreFailsNamed(t *testing.T) {
	t.Run("missing trigger", func(t *testing.T) {
		dir, s := newStore(t)
		remember(t, s, "User prefers window seats on flights")
		s.Close()
		db := rawDB(t, dir)
		if _, err := db.Exec(`DROP TRIGGER memories_au`); err != nil {
			t.Fatal(err)
		}
		db.Close()

		rep := RunStorage(dir)
		if rep.OK {
			t.Fatal("expected conformance to fail")
		}
		if res := result(t, rep, "fts-triggers"); res.OK || !strings.Contains(res.Detail, "memories_au") {
			t.Fatalf("fts-triggers should name the dropped trigger, got: %+v", res)
		}
	})

	t.Run("drifted FTS mirror", func(t *testing.T) {
		dir, s := newStore(t)
		remember(t, s, "User prefers window seats on flights")
		s.Close()
		db := rawDB(t, dir)
		var rowid int64
		var content, typ string
		if err := db.QueryRow(`SELECT rowid, content, type FROM memories LIMIT 1`).Scan(&rowid, &content, &typ); err != nil {
			t.Fatal(err)
		}
		// remove the index row the way real-world drift happens (hand edits)
		if _, err := db.Exec(`INSERT INTO memories_fts(memories_fts, rowid, content, type) VALUES('delete', ?, ?, ?)`,
			rowid, content, typ); err != nil {
			t.Fatal(err)
		}
		db.Close()

		rep := RunStorage(dir)
		if rep.OK {
			t.Fatal("expected conformance to fail")
		}
		if res := result(t, rep, "fts-sync"); res.OK || !strings.Contains(res.Detail, "drift") {
			t.Fatalf("fts-sync should name the drift, got: %+v", res)
		}
	})
}
