// Package conformance validates Veda data directories and export documents
// against UOMP draft-01 (spec/uomp-draft-01.md). It is deliberately
// read-only: a validator that repairs what it validates proves nothing.
package conformance

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/teochenglim/veda/internal/store"
	_ "modernc.org/sqlite"
)

// Result is one named check; Detail carries the human-readable reason on
// failure (AC5: a corrupted store fails with the reason named).
type Result struct {
	Name   string
	OK     bool
	Detail string
}

// Report is the outcome of a storage conformance run.
type Report struct {
	Dir     string
	Results []Result
	OK      bool
}

// requiredTables is the draft-01 table set. memories_fts is the FTS5
// external-content mirror and appears as a table in sqlite_master.
var requiredTables = []string{
	"audit", "change_log", "conflicts", "memories", "memories_fts",
	"memories_vec", "pending_turns", "sessions", "sync_state",
	"telemetry_queue", "tombstones", "turns",
}

// requiredTriggers is the draft-01 FTS mirror trigger set.
var requiredTriggers = []string{"memories_ai", "memories_ad", "memories_au"}

// requiredMemoryCols is the draft-01 memories column set (v0.3 added
// status + superseded_by; both must exist post-migration).
var requiredMemoryCols = []string{
	"id", "type", "content", "source_turn_ids", "agent_id", "confidence",
	"salience", "created_at", "updated_at", "ttl", "deleted", "status",
	"superseded_by",
}

// RunStorage validates the Veda home directory dir against UOMP draft-01.
// The database is opened read-only so conformance never mutates what it
// judges; the export check alone opens the store through Veda's own code,
// which is a no-op on an already-compliant store (no columns to add, no
// change log to backfill) and is skipped entirely when the schema checks
// fail — otherwise a migrated store would validate rules it never met.
func RunStorage(dir string) *Report {
	rep := &Report{Dir: dir}
	add := func(name string, ok bool, format string, args ...any) {
		detail := ""
		if !ok {
			detail = fmt.Sprintf(format, args...)
		}
		rep.Results = append(rep.Results, Result{Name: name, OK: ok, Detail: detail})
	}
	skip := func(name string) {
		rep.Results = append(rep.Results, Result{Name: name, OK: false, Detail: "skipped: earlier check failed"})
	}

	dbPath := filepath.Join(dir, "veda.db")
	if _, err := os.Stat(dbPath); err != nil {
		add("db-open", false, "no veda.db in %s — run `veda init` first", dir)
		return rep
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		add("db-open", false, "open failed: %v", err)
		return rep
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		add("db-open", false, "open failed: %v", err)
		return rep
	}
	add("db-open", true, "")

	present := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		add("schema-tables", false, "sqlite_master query failed: %v", err)
		return rep
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			present[name] = true
		}
	}
	rows.Close()
	var missing []string
	for _, t := range requiredTables {
		if !present[t] {
			missing = append(missing, t)
		}
	}
	schemaOK := len(missing) == 0
	if schemaOK {
		add("schema-tables", true, "")
	} else {
		add("schema-tables", false, "missing tables: %s", strings.Join(missing, ", "))
	}

	memCols := map[string]bool{}
	colRows, err := db.Query(`PRAGMA table_info(memories)`)
	if err != nil {
		add("schema-columns", false, "table_info failed: %v", err)
		schemaOK = false
	} else {
		for colRows.Next() {
			var cid, notNull, pk int
			var name, ctype string
			var dflt any
			if err := colRows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err == nil {
				memCols[name] = true
			}
		}
		colRows.Close()
		var missingCols []string
		for _, c := range requiredMemoryCols {
			if !memCols[c] {
				missingCols = append(missingCols, c)
			}
		}
		if len(missingCols) == 0 {
			add("schema-columns", true, "")
		} else {
			add("schema-columns", false, "memories missing columns: %s", strings.Join(missingCols, ", "))
			schemaOK = false
		}
	}

	have := map[string]bool{}
	trigRows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'trigger'`)
	if err != nil {
		add("fts-triggers", false, "sqlite_master query failed: %v", err)
	} else {
		for trigRows.Next() {
			var name string
			if err := trigRows.Scan(&name); err == nil {
				have[name] = true
			}
		}
		trigRows.Close()
		var missingTriggers []string
		for _, t := range requiredTriggers {
			if !have[t] {
				missingTriggers = append(missingTriggers, t)
			}
		}
		if len(missingTriggers) == 0 {
			add("fts-triggers", true, "")
		} else {
			add("fts-triggers", false, "missing FTS mirror triggers: %s", strings.Join(missingTriggers, ", "))
		}
	}

	ftsOK, ftsDetail := ftsInSync(db)
	add("fts-sync", ftsOK, "%s", ftsDetail)

	if !schemaOK {
		skip("export-shape")
		return rep
	}
	s, err := store.Open(dbPath)
	if err != nil {
		add("export-shape", false, "store open failed: %v", err)
		return rep
	}
	data, exportErr := s.Export()
	s.Close()
	if exportErr != nil {
		add("export-shape", false, "export failed: %v", exportErr)
		return rep
	}
	if viols := ValidateExport(data); len(viols) > 0 {
		add("export-shape", false, "export violates draft-01: %s", strings.Join(viols, "; "))
	} else {
		add("export-shape", true, "")
	}

	rep.OK = true
	for _, res := range rep.Results {
		if !res.OK {
			rep.OK = false
		}
	}
	return rep
}

// ftsInSync verifies the draft-01 mirror invariant: memories_fts holds
// exactly one indexed row per memories row, with identical content.
//
// External-content FTS5 answers COUNT(*) and full scans through the content
// table, so neither can see index drift (and integrity-check does not verify
// external content). What CAN see the index: the %_docsize shadow table
// (cardinality) and MATCH queries (per-row content). Drift fails with the
// first divergent rowid named.
func ftsInSync(db *sql.DB) (ok bool, detail string) {
	var memCount, ftsCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&memCount); err != nil {
		return false, fmt.Sprintf("memories count failed: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories_fts_docsize`).Scan(&ftsCount); err == nil && ftsCount != memCount {
		return false, fmt.Sprintf("mirror drift: memories has %d rows but the FTS index holds %d", memCount, ftsCount)
	}

	rows, err := db.Query(`SELECT rowid, content FROM memories ORDER BY rowid`)
	if err != nil {
		return false, fmt.Sprintf("memories scan failed: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rowid int64
		var content string
		if err := rows.Scan(&rowid, &content); err != nil {
			return false, fmt.Sprintf("memories scan failed: %v", err)
		}
		if strings.TrimSpace(content) == "" {
			continue // zero-token content has no index footprint to probe
		}
		hits, err := db.Query(`SELECT rowid FROM memories_fts WHERE memories_fts MATCH ?`, phraseOf(content))
		if err != nil {
			return false, fmt.Sprintf("FTS probe failed at rowid %d: %v", rowid, err)
		}
		found := false
		for hits.Next() {
			var hit int64
			if err := hits.Scan(&hit); err == nil && hit == rowid {
				found = true
				break
			}
		}
		hits.Close()
		if !found {
			return false, fmt.Sprintf("mirror drift at rowid %d: its content is not findable in the FTS index", rowid)
		}
	}
	return true, ""
}

// phraseOf renders content as an FTS5 phrase query — exact token sequence,
// with embedded quotes doubled per the FTS5 string syntax.
func phraseOf(content string) string {
	return `"` + strings.ReplaceAll(content, `"`, `""`) + `"`
}

// ValidateExport checks an export document against the draft-01 shape
// (spec/uomp-draft-01.md §3) and returns its violations — empty means valid.
// Field presence and JSON types are both checked (numbers must be numbers,
// not strings; optional fields stay optional).
func ValidateExport(data []byte) []string {
	var viols []string
	violate := func(format string, args ...any) {
		viols = append(viols, fmt.Sprintf(format, args...))
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return []string{fmt.Sprintf("not valid JSON: %v", err)}
	}

	version, ok := decodeString(top, "version")
	switch {
	case !ok:
		violate(`missing "version"`)
	case version != "1":
		violate("unsupported export version %q (draft-01 defines \"1\")", version)
	}
	if _, present := top["exported_at"]; !present {
		violate(`missing "exported_at"`)
	} else if !isNumber(top["exported_at"]) {
		violate(`"exported_at" must be a number`)
	}

	for _, key := range []string{"memories", "turns", "audit"} {
		if _, present := top[key]; !present {
			violate("missing %q", key)
		}
	}
	if len(viols) > 0 {
		return viols
	}

	for _, m := range decodeArray(top["memories"]) {
		requireString(m, "id", violate)
		requireString(m, "type", violate)
		requireString(m, "content", violate)
		requireNumber(m, "confidence", violate)
		requireNumber(m, "salience", violate)
		requireNumber(m, "created_at", violate)
		requireNumber(m, "updated_at", violate)
		optString(m, "source_turn_ids", violate)
		optString(m, "agent_id", violate)
		optString(m, "status", violate)
		optString(m, "superseded_by", violate)
		optNumber(m, "ttl", violate)
		optBool(m, "deleted", violate)
	}
	for _, t := range decodeArray(top["turns"]) {
		requireNumber(t, "id", violate)
		requireString(t, "session_id", violate)
		requireString(t, "role", violate)
		requireString(t, "content", violate)
		requireNumber(t, "ts", violate)
	}
	for _, a := range decodeArray(top["audit"]) {
		requireNumber(a, "id", violate)
		requireString(a, "memory_id", violate)
		requireString(a, "action", violate)
		requireString(a, "actor", violate)
		requireNumber(a, "ts", violate)
		optString(a, "reason", violate)
	}
	return viols
}

func decodeArray(raw json.RawMessage) []map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var out []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func decodeString(top map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := top[key]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func isNumber(raw json.RawMessage) bool {
	var n json.Number
	return json.Unmarshal(raw, &n) == nil
}

func typed(raw json.RawMessage, dst any) bool {
	return raw != nil && json.Unmarshal(raw, dst) == nil
}

func requireString(obj map[string]json.RawMessage, key string, violate func(string, ...any)) {
	raw, ok := obj[key]
	if !ok {
		violate("missing %q", key)
		return
	}
	var s string
	if !typed(raw, &s) {
		violate("%q must be a string", key)
	}
}

func optString(obj map[string]json.RawMessage, key string, violate func(string, ...any)) {
	if raw, present := obj[key]; present && !typed(raw, new(string)) {
		violate("%q must be a string", key)
	}
}

func requireNumber(obj map[string]json.RawMessage, key string, violate func(string, ...any)) {
	raw, ok := obj[key]
	if !ok {
		violate("missing %q", key)
		return
	}
	if !isNumber(raw) {
		violate("%q must be a number", key)
	}
}

func optNumber(obj map[string]json.RawMessage, key string, violate func(string, ...any)) {
	if raw, present := obj[key]; present && !isNumber(raw) {
		violate("%q must be a number", key)
	}
}

func optBool(obj map[string]json.RawMessage, key string, violate func(string, ...any)) {
	if raw, present := obj[key]; present && !typed(raw, new(bool)) {
		violate("%q must be a boolean", key)
	}
}
