// Package store implements the Veda persistence layer: SQLite with FTS5,
// using the pure-Go modernc.org/sqlite driver (no CGO).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/teochenglim/veda/internal/conflict"
	"github.com/teochenglim/veda/internal/embed"
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a memory id does not exist.
var ErrNotFound = errors.New("memory not found")

// Memory is one distilled fact about the user.
type Memory struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	Content       string  `json:"content"`
	SourceTurnIDs string  `json:"source_turn_ids,omitempty"`
	AgentID       string  `json:"agent_id,omitempty"`
	Confidence    float64 `json:"confidence"`
	Salience      float64 `json:"salience"`
	CreatedAt     int64   `json:"created_at"`
	UpdatedAt     int64   `json:"updated_at"`
	TTL           int64   `json:"ttl,omitempty"`           // unix seconds; 0 = never expires
	Deleted       bool    `json:"deleted,omitempty"`       // carries across sync bundles
	Status        string  `json:"status,omitempty"`        // active | superseded
	SupersededBy  string  `json:"superseded_by,omitempty"` // memory id that replaced this one
}

// AuditEntry records who did what to which memory.
type AuditEntry struct {
	ID       int64  `json:"id"`
	MemoryID string `json:"memory_id"`
	Action   string `json:"action"`
	Actor    string `json:"actor"`
	Reason   string `json:"reason,omitempty"`
	TS       int64  `json:"ts"`
}

// Turn is one captured conversation turn (drained from the WAL).
type Turn struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	TS        int64  `json:"ts"`
}

// PendingTurn is a turn the cheap gate flagged as a summarization candidate.
type PendingTurn struct {
	ID        int64   `json:"id"`
	SessionID string  `json:"session_id"`
	Role      string  `json:"role"`
	Content   string  `json:"content"`
	TS        int64   `json:"ts"`
	GateScore float64 `json:"gate_score"`
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY, agent_id TEXT,
  started_at INTEGER, ended_at INTEGER
);
CREATE TABLE IF NOT EXISTS turns (
  id INTEGER PRIMARY KEY, session_id TEXT,
  role TEXT, content TEXT, ts INTEGER
);
CREATE TABLE IF NOT EXISTS memories (
  id TEXT PRIMARY KEY, type TEXT, content TEXT,
  source_turn_ids TEXT, agent_id TEXT,
  confidence REAL, salience REAL,
  created_at INTEGER, updated_at INTEGER,
  ttl INTEGER, deleted INTEGER DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'active',
  superseded_by TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS conflicts (
  id INTEGER PRIMARY KEY, old_id TEXT NOT NULL, new_id TEXT NOT NULL,
  detected_at INTEGER, resolution TEXT NOT NULL DEFAULT 'new'
);
CREATE TABLE IF NOT EXISTS tombstones (
  memory_id TEXT PRIMARY KEY, deleted_at INTEGER NOT NULL, device_id TEXT
);
CREATE TABLE IF NOT EXISTS sync_state (
  key TEXT PRIMARY KEY, value TEXT
);
CREATE TABLE IF NOT EXISTS change_log (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  memory_id TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_change_log_mem ON change_log(memory_id);
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
  content, type, content='memories', content_rowid='rowid'
);
CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY, memory_id TEXT,
  action TEXT, actor TEXT, reason TEXT, ts INTEGER
);
CREATE TABLE IF NOT EXISTS telemetry_queue (
  id INTEGER PRIMARY KEY, payload TEXT, queued_at INTEGER
);
CREATE TABLE IF NOT EXISTS pending_turns (
  id INTEGER PRIMARY KEY, session_id TEXT,
  role TEXT, content TEXT, ts INTEGER, gate_score REAL
);
CREATE TABLE IF NOT EXISTS memories_vec (
  memory_id TEXT PRIMARY KEY, vec BLOB NOT NULL, dims INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memories_vec_memory ON memories_vec(memory_id);
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, content, type) VALUES (new.rowid, new.content, new.type);
END;
CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, content, type) VALUES('delete', old.rowid, old.content, old.type);
END;
CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, content, type) VALUES('delete', old.rowid, old.content, old.type);
  INSERT INTO memories_fts(rowid, content, type) VALUES (new.rowid, new.content, new.type);
END;
`

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	// modernc sqlite: single writer; bound the pool to avoid
	// SQLITE_BUSY churn between the MCP server, the worker and the UI.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	// does this database predate the sync change log? (checked before the
	// schema exec creates it — the answer decides whether the backfill runs)
	var changeLogExisted bool
	_ = db.QueryRow(`SELECT COUNT(*) > 0 FROM sqlite_master WHERE type = 'table' AND name = 'change_log'`).Scan(&changeLogExisted)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := s.migrate(changeLogExisted); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// migrate upgrades databases created before the current schema version.
// CREATE TABLE IF NOT EXISTS does not touch an existing table, so v0.1/v0.2
// databases need explicit ALTERs. New columns default their rows into the
// right state: status 'active', superseded_by ”.
func (s *Store) migrate(changeLogExisted bool) error {
	cols := map[string]bool{}
	rows, err := s.db.Query(`PRAGMA table_info(memories)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	rows.Close()
	needsRebuild := false
	for _, m := range []struct{ col, ddl string }{
		{"status", `ALTER TABLE memories ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`},
		{"superseded_by", `ALTER TABLE memories ADD COLUMN superseded_by TEXT NOT NULL DEFAULT ''`},
	} {
		if !cols[m.col] {
			if _, err := s.db.Exec(m.ddl); err != nil {
				return fmt.Errorf("add column %s: %w", m.col, err)
			}
			needsRebuild = true
		}
	}
	if needsRebuild {
		// an upgraded DB may carry FTS drift (rows never indexed, hand-edited
		// rows, restored files); rebuild the external-content index from the
		// memories table so triggers and recall behave on migrated rows.
		if _, err := s.db.Exec(`INSERT INTO memories_fts(memories_fts) VALUES('rebuild')`); err != nil {
			return fmt.Errorf("rebuild fts: %w", err)
		}
	}
	// change-log backfill, first open after a pre-v0.4 upgrade only: those
	// rows must land in the first v0.4 push (watermark starts at 0). Running
	// this on every open would re-mark pulled rows as pending push.
	if !changeLogExisted {
		if _, err := s.db.Exec(`INSERT INTO change_log (memory_id)
			SELECT id FROM memories
			WHERE id NOT IN (SELECT memory_id FROM change_log)`); err != nil {
			return fmt.Errorf("backfill change log: %w", err)
		}
	}
	return nil
}

// bumpChange records that a memory changed, for the sync push feed. Called
// explicitly on every local write path — deliberately NOT on MergeIncoming,
// so pulled rows never ping-pong back to their origin device.
func (s *Store) bumpChange(tx *sql.Tx, memoryID string) error {
	_, err := tx.Exec(`INSERT INTO change_log (memory_id) VALUES (?)`, memoryID)
	return err
}

// Embedder produces one vector per input text. Implemented by the
// OpenAI-compatible client in internal/embed; tests supply fakes.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// SetEmbedding upserts the vector for a memory. v0.2 migration path: rows
// are added lazily by the worker's backfill, so upgrading an existing
// v0.1 database needs no schema migration beyond this table.
func (s *Store) SetEmbedding(memoryID string, vec []float32) error {
	b, err := json.Marshal(vec)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO memories_vec (memory_id, vec, dims) VALUES (?,?,?)
		ON CONFLICT(memory_id) DO UPDATE SET vec = excluded.vec, dims = excluded.dims`,
		memoryID, b, len(vec))
	return err
}

// MissingEmbeddings returns live memories that have no vector yet — the
// worker's backfill queue.
func (s *Store) MissingEmbeddings(limit int) ([]*Memory, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+memCols+` WHERE deleted = 0 AND status = 'active' AND (ttl = 0 OR ttl > ?)
		AND id NOT IN (SELECT memory_id FROM memories_vec) ORDER BY updated_at DESC LIMIT ?`,
		time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	return collectMemories(rows)
}

// CountEmbedded reports how many live memories carry a vector (Digest stat).
func (s *Store) CountEmbedded() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM memories_vec v
		JOIN memories m ON m.id = v.memory_id WHERE m.deleted = 0 AND m.status = 'active'`).Scan(&n)
	return n, err
}

// vecRow is a live memory joined with its vector.
type vecRow struct {
	*Memory
	vec []float32
}

func (s *Store) loadVectors() ([]*vecRow, error) {
	rows, err := s.db.Query(`SELECT m.id, m.type, m.content, m.source_turn_ids, m.agent_id,
		m.confidence, m.salience, m.created_at, m.updated_at, m.ttl, m.deleted, v.vec
		FROM memories m JOIN memories_vec v ON v.memory_id = m.id
		WHERE m.deleted = 0 AND m.status = 'active' AND (m.ttl = 0 OR m.ttl > ?) AND m.confidence >= 0`,
		time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*vecRow
	for rows.Next() {
		var m Memory
		var deleted int
		var raw []byte
		if err := rows.Scan(&m.ID, &m.Type, &m.Content, &m.SourceTurnIDs, &m.AgentID,
			&m.Confidence, &m.Salience, &m.CreatedAt, &m.UpdatedAt, &m.TTL, &deleted, &raw); err != nil {
			return nil, err
		}
		var vec []float32
		if json.Unmarshal(raw, &vec) != nil || len(vec) == 0 {
			continue
		}
		m.Deleted = deleted != 0
		out = append(out, &vecRow{Memory: &m, vec: vec})
	}
	return out, rows.Err()
}

// RecallHybrid is v0.2's recall: weighted reciprocal-rank fusion of the FTS5
// keyword path (weight 0.6) with embedding cosine similarity (weight 0.4).
//
// Degrades gracefully by design (AC3): a nil embedder, an embedding error,
// or an empty vector table all fall back to keyword-only — never an error.
// semanticOnly skips the fusion and returns the pure semantic ranking.
func (s *Store) RecallHybrid(ctx context.Context, query string, limit int, minConfidence float64, em Embedder, semanticOnly bool) ([]*Memory, error) {
	if limit <= 0 {
		limit = 5
	}
	// Keyword pool: deeper than the final limit so fusion can re-rank.
	pool := limit * 4
	if pool < 20 {
		pool = 20
	}
	fts, err := s.Recall(query, pool, minConfidence)
	if err != nil {
		return nil, err
	}
	if em == nil {
		return trimOrNil(fts, limit), nil
	}

	qvec, err := s.embedQuery(ctx, em, query)
	if err != nil {
		return trimOrNil(fts, limit), nil // provider failure ⇒ keyword-only, never an error
	}
	rows, err := s.loadVectors()
	if err != nil {
		return trimOrNil(fts, limit), nil
	}

	type ranked struct {
		m    *Memory
		rank int
	}
	sims := make(map[string]float32, len(rows))
	var sem []ranked
	for _, r := range rows {
		if r.Confidence < minConfidence {
			continue
		}
		sims[r.ID] = embed.Cosine(qvec, r.vec)
		sem = append(sem, ranked{m: r.Memory})
	}
	sort.SliceStable(sem, func(i, j int) bool {
		return sims[sem[i].m.ID] > sims[sem[j].m.ID]
	})
	for i := range sem {
		sem[i].rank = i
	}

	if semanticOnly {
		if len(sem) == 0 {
			return trimOrNil(fts, limit), nil // no vectors yet ⇒ keyword-only fallback
		}
		out := make([]*Memory, 0, limit)
		for _, r := range sem {
			if len(out) == limit {
				break
			}
			out = append(out, r.m)
		}
		return out, nil
	}

	// Weighted RRF fusion. FTS carries the higher weight so an exact keyword
	// match can never rank below a semantic-only neighbor (AC2).
	const wFTS, wSem, k = 0.6, 0.4, 60.0
	score := func(r int, w float64) float64 {
		if r < 0 {
			return 0
		}
		return w / (k + float64(r))
	}
	ftsRank := make(map[string]int, len(fts))
	for i, m := range fts {
		ftsRank[m.ID] = i
	}
	merged := make(map[string]*Memory)
	type scored struct {
		m     *Memory
		score float64
	}
	var all []scored
	for i, m := range fts {
		s := score(i, wFTS) + score(-1, wSem)
		merged[m.ID] = m
		all = append(all, scored{m, s})
	}
	for _, r := range sem {
		if _, ftsHit := ftsRank[r.m.ID]; ftsHit {
			all[ftsRank[r.m.ID]].score += score(r.rank, wSem)
			continue
		}
		if _, seen := merged[r.m.ID]; seen {
			continue
		}
		merged[r.m.ID] = r.m
		all = append(all, scored{r.m, score(-1, wFTS) + score(r.rank, wSem)})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	out := make([]*Memory, 0, limit)
	for _, sc := range all {
		if len(out) == limit {
			break
		}
		out = append(out, sc.m)
	}
	return out, nil
}

// trimOrNil caps a result at limit; nil slices become empty ones so MCP
// responses carry [] rather than null.
func trimOrNil(m []*Memory, limit int) []*Memory {
	if len(m) > limit {
		m = m[:limit]
	}
	if m == nil {
		return []*Memory{}
	}
	return m
}

func (s *Store) embedQuery(ctx context.Context, em Embedder, query string) ([]float32, error) {
	vecs, err := em.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, fmt.Errorf("embedder returned no query vector")
	}
	return vecs[0], nil
}

func (s *Store) Close() error { return s.db.Close() }

// --- memories -------------------------------------------------------------

// Remember inserts a memory and writes an audit entry. It returns the new id.
func (s *Store) Remember(m *Memory, actor string) (string, error) {
	now := time.Now().Unix()
	if m.ID == "" {
		m.ID = newID()
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	if m.Confidence == 0 {
		m.Confidence = 1.0
	}
	if m.Salience == 0 {
		m.Salience = 0.5
	}
	_, err := s.db.Exec(`INSERT INTO memories
		(id, type, content, source_turn_ids, agent_id, confidence, salience, created_at, updated_at, ttl, deleted)
		VALUES (?,?,?,?,?,?,?,?,?,?,0)`,
		m.ID, m.Type, m.Content, m.SourceTurnIDs, m.AgentID,
		m.Confidence, m.Salience, m.CreatedAt, m.UpdatedAt, m.TTL)
	if err != nil {
		return "", err
	}
	s.Audit(m.ID, "create", actor, "remember")
	if tx, err := s.db.Begin(); err == nil {
		_ = s.bumpChange(tx, m.ID)
		_ = tx.Commit()
	}
	s.detectConflicts(m.ID, m.Content, actor)
	return m.ID, nil
}

// detectConflicts implements v0.3 auto-supersession: a new memory that
// contradicts an active one (same life slot, different value, or an explicit
// negation) marks the older entry superseded. Resolutions stay human-overrideable
// via ResolveConflict. Best-effort: detection never fails the write.
func (s *Store) detectConflicts(newID, content, actor string) {
	defer func() {
		if r := recover(); r != nil {
			// heuristic layer must never take the write path down
		}
	}()
	ia := conflict.Analyze(content)
	if len(ia.Slots) == 0 && !ia.Negation {
		return // nothing contradiction-bearing about this write
	}
	rows, err := s.db.Query(`SELECT id, content FROM memories
		WHERE deleted = 0 AND status = 'active' AND id != ?`, newID)
	if err != nil {
		return
	}
	type pair struct{ id, content string }
	var candidates []pair
	for rows.Next() {
		var p pair
		if rows.Scan(&p.id, &p.content) == nil {
			candidates = append(candidates, p)
		}
	}
	rows.Close()
	for _, c := range candidates {
		if v := conflict.Detect(c.content, content); v.Conflict {
			if err := s.supersede(c.id, newID, actor, v.Reason); err != nil {
				return
			}
		}
	}
}

// supersede marks oldID as replaced by newID and records the conflict pair
// for UI review. Atomic so recall never observes a half-state.
func (s *Store) supersede(oldID, newID, actor, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE memories SET status = 'superseded', superseded_by = ?, updated_at = ?
		WHERE id = ? AND status = 'active'`, newID, time.Now().Unix(), oldID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO conflicts (old_id, new_id, detected_at, resolution) VALUES (?,?,?, 'new')`,
		oldID, newID, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO audit (memory_id, action, actor, reason, ts) VALUES (?,?,?,?,?)`,
		oldID, "supersede", actor, reason+" (superseded by "+newID+")", time.Now().Unix()); err != nil {
		return err
	}
	if err := s.bumpChange(tx, oldID); err != nil {
		return err
	}
	return tx.Commit()
}

// --- conflicts (v0.3 review API) -------------------------------------------

// Conflict is one detected supersession pair, for the UI's Conflicts tab.
type Conflict struct {
	ID         int64  `json:"id"`
	OldID      string `json:"old_id"`
	NewID      string `json:"new_id"`
	OldContent string `json:"old_content"`
	NewContent string `json:"new_content"`
	OldStatus  string `json:"old_status"`
	NewStatus  string `json:"new_status"`
	DetectedAt int64  `json:"detected_at"`
	Resolution string `json:"resolution"` // new | old | both
}

// ListConflicts returns detected conflict pairs, newest first.
func (s *Store) ListConflicts(limit int) ([]*Conflict, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT c.id, c.old_id, c.new_id, c.detected_at, c.resolution,
		mo.content, mn.content, mo.status, mn.status
		FROM conflicts c
		JOIN memories mo ON mo.id = c.old_id
		JOIN memories mn ON mn.id = c.new_id
		ORDER BY c.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Conflict
	for rows.Next() {
		var c Conflict
		if err := rows.Scan(&c.ID, &c.OldID, &c.NewID, &c.DetectedAt, &c.Resolution,
			&c.OldContent, &c.NewContent, &c.OldStatus, &c.NewStatus); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// ResolveConflict applies a human decision on a detected conflict:
//   - "new":  prefer the incoming memory (the auto-supersede default)
//   - "old":  reinstate the old memory, supersede the new one
//   - "both": keep both memories active (the detector was wrong)
//
// Every resolution is audited on both sides.
func (s *Store) ResolveConflict(conflictID int64, resolution, actor string) error {
	if resolution != "new" && resolution != "old" && resolution != "both" {
		return fmt.Errorf("resolution must be new, old, or both")
	}
	var oldID, newID string
	err := s.db.QueryRow(`SELECT old_id, new_id FROM conflicts WHERE id = ?`, conflictID).Scan(&oldID, &newID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	switch resolution {
	case "old":
		if _, err := tx.Exec(`UPDATE memories SET status = 'superseded', superseded_by = ?, updated_at = ? WHERE id = ?`, oldID, now, newID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE memories SET status = 'active', superseded_by = '', updated_at = ? WHERE id = ?`, now, oldID); err != nil {
			return err
		}
	case "both":
		if _, err := tx.Exec(`UPDATE memories SET status = 'active', superseded_by = '', updated_at = ? WHERE id = ?`, now, oldID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE conflicts SET resolution = ? WHERE id = ?`, resolution, conflictID); err != nil {
		return err
	}
	for _, id := range []string{oldID, newID} {
		if _, err := tx.Exec(`INSERT INTO audit (memory_id, action, actor, reason, ts) VALUES (?,?,?,?,?)`,
			id, "resolve", actor, "conflict #"+fmt.Sprint(conflictID)+" resolved: "+resolution, now); err != nil {
			return err
		}
		if err := s.bumpChange(tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanMemory(row interface{ Scan(...any) error }) (*Memory, error) {
	var m Memory
	var deleted int
	err := row.Scan(&m.ID, &m.Type, &m.Content, &m.SourceTurnIDs, &m.AgentID,
		&m.Confidence, &m.Salience, &m.CreatedAt, &m.UpdatedAt, &m.TTL, &deleted,
		&m.Status, &m.SupersededBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.Deleted = deleted != 0
	if m.Status == "" {
		m.Status = "active"
	}
	return &m, nil
}

const memCols = `id, type, content, source_turn_ids, agent_id, confidence, salience, created_at, updated_at, ttl, deleted, status, superseded_by FROM memories`

// Get fetches one live (non-deleted, non-expired) memory by id.
func (s *Store) Get(id string) (*Memory, error) {
	m, err := scanMemory(s.db.QueryRow(`SELECT `+memCols+` WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if m.Deleted || m.Status != "active" || (m.TTL > 0 && m.TTL <= time.Now().Unix()) {
		return nil, ErrNotFound
	}
	return m, nil
}

// Recall runs an FTS5 full-text search over live memories. The query is
// treated as a phrase-bag: each whitespace-separated term is required to
// appear, with a LIKE fallback for punctuation-heavy queries.
func (s *Store) Recall(query string, limit int, minConfidence float64) ([]*Memory, error) {
	if limit <= 0 {
		limit = 5
	}
	if strings.TrimSpace(query) == "" {
		return s.List("", "", time.Time{}, time.Time{}, limit, false)
	}
	match := ftsQuery(query)
	rows, err := s.db.Query(`SELECT m.id, m.type, m.content, m.source_turn_ids, m.agent_id,
		m.confidence, m.salience, m.created_at, m.updated_at, m.ttl, m.deleted, m.status, m.superseded_by
		FROM memories m
		JOIN memories_fts f ON f.rowid = m.rowid
		WHERE memories_fts MATCH ? AND m.deleted = 0 AND m.status = 'active' AND m.confidence >= ?
		  AND (m.ttl = 0 OR m.ttl > ?)
		ORDER BY bm25(memories_fts), m.salience DESC LIMIT ?`,
		match, minConfidence, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	out, err := collectMemories(rows)
	if err != nil || len(out) > 0 {
		return out, err
	}
	// LIKE fallback (FTS5 tokenization can miss punctuation-bearing queries).
	like := "%" + strings.TrimSpace(query) + "%"
	rows, err = s.db.Query(`SELECT `+memCols+` WHERE deleted = 0 AND status = 'active' AND confidence >= ?
		AND (ttl = 0 OR ttl > ?) AND content LIKE ? ORDER BY salience DESC LIMIT ?`,
		minConfidence, time.Now().Unix(), like, limit)
	if err != nil {
		return nil, err
	}
	return collectMemories(rows)
}

// ftsQuery turns free text into a safe FTS5 expression: terms ANDed
// together, quotes stripped so user input can't break the syntax.
func ftsQuery(q string) string {
	terms := strings.Fields(strings.Map(func(r rune) rune {
		if r == '"' || r == '(' || r == ')' || r == '*' || r == ':' || r == '^' || r == '-' {
			return ' '
		}
		return r
	}, q))
	for i, t := range terms {
		terms[i] = `"` + t + `"`
	}
	return strings.Join(terms, " ")
}

// List returns live memories, optionally filtered. Zero-valued filters are
// ignored. limit <= 0 means no limit. Superseded memories are excluded
// unless includeSuperseded is set (MCP `list include_superseded: true`).
func (s *Store) List(memType, agentID string, since, until time.Time, limit int, includeSuperseded bool) ([]*Memory, error) {
	where := []string{"deleted = 0", "(ttl = 0 OR ttl > ?)"}
	if !includeSuperseded {
		where = append(where, "status = 'active'")
	}
	args := []any{time.Now().Unix()}
	if memType != "" {
		where = append(where, "type = ?")
		args = append(args, memType)
	}
	if agentID != "" {
		where = append(where, "agent_id = ?")
		args = append(args, agentID)
	}
	if !since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, since.Unix())
	}
	if !until.IsZero() {
		where = append(where, "created_at <= ?")
		args = append(args, until.Unix())
	}
	q := `SELECT ` + memCols + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY updated_at DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return collectMemories(rows)
}

func collectMemories(rows *sql.Rows) ([]*Memory, error) {
	defer rows.Close()
	var out []*Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Update rewrites the content (and optionally type) of a memory; used by the
// review UI's edit action.
func (s *Store) Update(id, content string) error {
	now := time.Now().Unix()
	res, err := s.db.Exec(`UPDATE memories SET content = ?, updated_at = ? WHERE id = ? AND deleted = 0 AND status = 'active'`,
		content, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if tx, err := s.db.Begin(); err == nil {
		_ = s.bumpChange(tx, id)
		_ = tx.Commit()
	}
	return s.Audit(id, "update", "ui", "edited in review UI")
}

// Forget soft-deletes a memory and audits it.
func (s *Store) Forget(id, actor, reason string) (bool, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`UPDATE memories SET deleted = 1, updated_at = ? WHERE id = ? AND deleted = 0 AND status = 'active'`,
		now, id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	deviceID, _ := s.SyncGet("device_id")
	if tx, err := s.db.Begin(); err == nil {
		if _, err := tx.Exec(`INSERT INTO tombstones (memory_id, deleted_at, device_id) VALUES (?,?,?)
			ON CONFLICT(memory_id) DO UPDATE SET deleted_at = MAX(deleted_at, excluded.deleted_at)`,
			id, now, deviceID); err != nil {
			tx.Rollback()
			return false, err
		}
		if err := s.bumpChange(tx, id); err != nil {
			tx.Rollback()
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
	}
	return true, s.Audit(id, "forget", actor, reason)
}

// --- audit ----------------------------------------------------------------

func (s *Store) Audit(memoryID, action, actor, reason string) error {
	_, err := s.db.Exec(`INSERT INTO audit (memory_id, action, actor, reason, ts) VALUES (?,?,?,?,?)`,
		memoryID, action, actor, reason, time.Now().Unix())
	return err
}

func (s *Store) ListAudit(limit int) ([]*AuditEntry, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT id, memory_id, action, actor, reason, ts FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.ID, &a.MemoryID, &a.Action, &a.Actor, &a.Reason, &a.TS); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// --- turns / pending ------------------------------------------------------

// InsertTurns persists drained WAL turns in one transaction.
func (s *Store) InsertTurns(turns []Turn) error {
	if len(turns) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range turns {
		if _, err := tx.Exec(`INSERT INTO turns (session_id, role, content, ts) VALUES (?,?,?,?)`,
			t.SessionID, t.Role, t.Content, t.TS); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// InsertPending queues gate-approved candidate turns.
func (s *Store) InsertPending(p []PendingTurn) error {
	if len(p) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range p {
		if _, err := tx.Exec(`INSERT INTO pending_turns (session_id, role, content, ts, gate_score) VALUES (?,?,?,?,?)`,
			t.SessionID, t.Role, t.Content, t.TS, t.GateScore); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListPending returns candidate turns awaiting summarization (the UI's
// Pending tab), oldest first.
func (s *Store) ListPending(limit int) ([]*PendingTurn, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id, session_id, role, content, ts, gate_score
		FROM pending_turns ORDER BY id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PendingTurn
	for rows.Next() {
		var p PendingTurn
		if err := rows.Scan(&p.ID, &p.SessionID, &p.Role, &p.Content, &p.TS, &p.GateScore); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// TakePending atomically fetches and removes up to n pending turns, so the
// worker never summarizes the same batch twice.
func (s *Store) TakePending(n int) ([]*PendingTurn, error) {
	rows, err := s.db.Query(`SELECT id, session_id, role, content, ts, gate_score
		FROM pending_turns ORDER BY id ASC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PendingTurn
	var ids []any
	for rows.Next() {
		var p PendingTurn
		if err := rows.Scan(&p.ID, &p.SessionID, &p.Role, &p.Content, &p.TS, &p.GateScore); err != nil {
			return nil, err
		}
		out = append(out, &p)
		ids = append(ids, p.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM pending_turns WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// DropPending discards a pending turn without promoting it (UI "reject").
func (s *Store) DropPending(id int64) error {
	_, err := s.db.Exec(`DELETE FROM pending_turns WHERE id = ?`, id)
	return err
}

// --- export / import ------------------------------------------------------

type exportDoc struct {
	Version  string        `json:"version"`
	Exported int64         `json:"exported_at"`
	Memories []*Memory     `json:"memories"`
	Turns    []Turn        `json:"turns"`
	Audit    []*AuditEntry `json:"audit"`
}

// Export renders the user's full memory store as JSON: portable by design.
func (s *Store) Export() ([]byte, error) {
	memRows, err := s.db.Query(`SELECT ` + memCols + ` WHERE deleted = 0 AND status = 'active' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	memories, err := collectMemories(memRows)
	if err != nil {
		return nil, err
	}
	turnRows, err := s.db.Query(`SELECT id, session_id, role, content, ts FROM turns ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var turns []Turn
	for turnRows.Next() {
		var t Turn
		if err := turnRows.Scan(&t.ID, &t.SessionID, &t.Role, &t.Content, &t.TS); err != nil {
			turnRows.Close()
			return nil, err
		}
		turns = append(turns, t)
	}
	turnRows.Close()
	audit, err := s.ListAudit(0)
	if err != nil {
		return nil, err
	}
	doc := exportDoc{Version: "1", Exported: time.Now().Unix(), Memories: memories, Turns: turns, Audit: audit}
	if doc.Turns == nil {
		doc.Turns = []Turn{}
	}
	return json.MarshalIndent(doc, "", "  ")
}

// Import loads an export document back into the store, skipping memories
// whose id already exists. It returns the number imported.
func (s *Store) Import(data []byte) (int, error) {
	var doc exportDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return 0, fmt.Errorf("parse export: %w", err)
	}
	imported := 0
	for _, m := range doc.Memories {
		if _, err := s.Get(m.ID); err == nil {
			continue
		}
		_, err := s.db.Exec(`INSERT INTO memories
			(id, type, content, source_turn_ids, agent_id, confidence, salience, created_at, updated_at, ttl, deleted)
			VALUES (?,?,?,?,?,?,?,?,?,?,0)`,
			m.ID, m.Type, m.Content, m.SourceTurnIDs, m.AgentID,
			m.Confidence, m.Salience, m.CreatedAt, m.UpdatedAt, m.TTL)
		if err != nil {
			return imported, err
		}
		imported++
	}
	return imported, nil
}

// --- sync state / change feeds / merge (v0.4) -------------------------------

// SyncGet / SyncSet store small sync bookkeeping values (device id, KDF
// salt, push watermark, pull cursor).
func (s *Store) SyncGet(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM sync_state WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SyncSet(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO sync_state (key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// MemoriesSince returns full rows changed since the change-log sequence —
// the push feed. Includes superseded rows (their status is part of
// convergence). The sequence (not wall-clock time) is the watermark, so
// same-second writes after a push are never missed.
func (s *Store) MemoriesSince(sinceSeq int64) ([]*Memory, error) {
	rows, err := s.db.Query(`SELECT `+memCols+` WHERE id IN
		(SELECT DISTINCT memory_id FROM change_log WHERE seq > ?)`, sinceSeq)
	if err != nil {
		return nil, err
	}
	return collectMemories(rows)
}

// Tombstone is one cross-device delete.
type Tombstone struct {
	MemoryID  string `json:"memory_id"`
	DeletedAt int64  `json:"deleted_at"`
	DeviceID  string `json:"device_id,omitempty"`
}

// TombstonesSince returns the tombstone push feed: deletes recorded since
// the change-log sequence. Merged-in tombstones have no change-log entry —
// the origin device already pushed them, so they never re-propagate.
func (s *Store) TombstonesSince(sinceSeq int64) ([]Tombstone, error) {
	rows, err := s.db.Query(`SELECT memory_id, deleted_at, device_id FROM tombstones
		WHERE memory_id IN (SELECT DISTINCT memory_id FROM change_log WHERE seq > ?)
		ORDER BY deleted_at`, sinceSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tombstone
	for rows.Next() {
		var t Tombstone
		if err := rows.Scan(&t.MemoryID, &t.DeletedAt, &t.DeviceID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AllTombstones returns every locally-known tombstone (listing, not the
// push feed).
func (s *Store) AllTombstones() ([]Tombstone, error) {
	rows, err := s.db.Query(`SELECT memory_id, deleted_at, device_id FROM tombstones ORDER BY deleted_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tombstone
	for rows.Next() {
		var t Tombstone
		if err := rows.Scan(&t.MemoryID, &t.DeletedAt, &t.DeviceID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MergeIncoming applies a decrypted bundle from another device.
// Last-writer-wins per memory id on updated_at; a tombstone removes the
// memory unless the memory was edited after the delete. Deterministic in
// bundle order (each row merges on its own merits), so both devices
// converge. Vectors for replaced/removed rows are invalidated — the worker
// re-embeds them. Returns the number of local changes applied.
func (s *Store) MergeIncoming(memories []*Memory, tombs []Tombstone) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	applied := 0
	for _, m := range memories {
		if m == nil || m.ID == "" {
			continue
		}
		var curUpdated int64
		var curDeleted int
		err := tx.QueryRow(`SELECT updated_at, deleted FROM memories WHERE id = ?`, m.ID).Scan(&curUpdated, &curDeleted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.Exec(`INSERT INTO memories
				(id, type, content, source_turn_ids, agent_id, confidence, salience, created_at, updated_at, ttl, deleted, status, superseded_by)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				m.ID, m.Type, m.Content, m.SourceTurnIDs, m.AgentID,
				m.Confidence, m.Salience, m.CreatedAt, m.UpdatedAt, m.TTL,
				boolInt(m.Deleted), m.Status, m.SupersededBy)
			if err != nil {
				return applied, err
			}
			applied++
		case err != nil:
			return applied, err
		case m.UpdatedAt > curUpdated:
			if _, err := tx.Exec(`UPDATE memories SET type=?, content=?, source_turn_ids=?, agent_id=?,
				confidence=?, salience=?, created_at=?, updated_at=?, ttl=?, deleted=?, status=?, superseded_by=?
				WHERE id = ?`,
				m.Type, m.Content, m.SourceTurnIDs, m.AgentID,
				m.Confidence, m.Salience, m.CreatedAt, m.UpdatedAt, m.TTL,
				boolInt(m.Deleted), m.Status, m.SupersededBy, m.ID); err != nil {
				return applied, err
			}
			// derived data for a replaced row is stale
			if _, err := tx.Exec(`DELETE FROM memories_vec WHERE memory_id = ?`, m.ID); err != nil {
				return applied, err
			}
			applied++
		}
	}
	for _, t := range tombs {
		if _, err := tx.Exec(`INSERT INTO tombstones (memory_id, deleted_at, device_id) VALUES (?,?,?)
			ON CONFLICT(memory_id) DO UPDATE SET deleted_at = MAX(deleted_at, excluded.deleted_at)`,
			t.MemoryID, t.DeletedAt, t.DeviceID); err != nil {
			return applied, err
		}
		var curUpdated int64
		var curDeleted int
		err := tx.QueryRow(`SELECT updated_at, deleted FROM memories WHERE id = ?`, t.MemoryID).Scan(&curUpdated, &curDeleted)
		if errors.Is(err, sql.ErrNoRows) {
			continue // nothing local to remove
		}
		if err != nil {
			return applied, err
		}
		if curDeleted == 0 && curUpdated <= t.DeletedAt {
			if _, err := tx.Exec(`UPDATE memories SET deleted = 1, updated_at = ? WHERE id = ?`, t.DeletedAt, t.MemoryID); err != nil {
				return applied, err
			}
			if _, err := tx.Exec(`DELETE FROM memories_vec WHERE memory_id = ?`, t.MemoryID); err != nil {
				return applied, err
			}
			applied++
		}
	}
	return applied, tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SyncPendingCounts reports how many memories and tombstones changed since
// the change-log sequence — the `sync status` pending numbers.
func (s *Store) SyncPendingCounts(sinceSeq int64) (int64, int64, error) {
	var mem, tomb int64
	if err := s.db.QueryRow(`SELECT COUNT(DISTINCT memory_id) FROM change_log WHERE seq > ?`, sinceSeq).Scan(&mem); err != nil {
		return 0, 0, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tombstones
		WHERE memory_id IN (SELECT DISTINCT memory_id FROM change_log WHERE seq > ?)`, sinceSeq).Scan(&tomb); err != nil {
		return 0, 0, err
	}
	return mem, tomb, nil
}

// ChangeLogHead returns the current change-log sequence (the push watermark
// after a successful push).
func (s *Store) ChangeLogHead() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM change_log`).Scan(&n)
	return n, err
}

// --- stats / telemetry queue ----------------------------------------------

// Stats is the anonymous aggregate shape telemetry sends. No content, ever.
type Stats struct {
	InstallID   string `json:"install_id"`
	Version     string `json:"version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	TS          int64  `json:"ts"`
	Memories    int64  `json:"memories"`
	Embedded    int64  `json:"embedded"`
	Superseded  int64  `json:"superseded"`
	Turns       int64  `json:"turns"`
	Sessions    int64  `json:"sessions"`
	RecallCalls int64  `json:"recall_calls"`
	RecallHits  int64  `json:"recall_hits"`
	ForgetCount int64  `json:"forget_count"`

	// Ext is the v0.5 opt-in audit block (counts/enums only); rendered by
	// the telemetry package, never populated by the store.
	Ext any `json:"ext,omitempty"`
}

// CollectStats computes anonymous counts from the DB.
func (s *Store) CollectStats() (*Stats, error) {
	st := &Stats{TS: time.Now().Unix()}
	var misses int64
	for _, q := range []struct {
		sql  string
		dest *int64
	}{
		{`SELECT COUNT(*) FROM memories WHERE deleted = 0 AND status = 'active'`, &st.Memories},
		{`SELECT COUNT(*) FROM memories WHERE status = 'superseded'`, &st.Superseded},
		{`SELECT COUNT(*) FROM memories_vec v JOIN memories m ON m.id = v.memory_id WHERE m.deleted = 0`, &st.Embedded},
		{`SELECT COUNT(*) FROM turns`, &st.Turns},
		{`SELECT COUNT(*) FROM sessions`, &st.Sessions},
		{`SELECT COUNT(*) FROM audit WHERE action = 'recall_hit'`, &st.RecallHits},
		{`SELECT COUNT(*) FROM audit WHERE action = 'recall_miss'`, &misses},
		{`SELECT COUNT(*) FROM audit WHERE action = 'forget'`, &st.ForgetCount},
	} {
		if err := s.db.QueryRow(q.sql).Scan(q.dest); err != nil {
			return nil, err
		}
	}
	st.RecallCalls = st.RecallHits + misses
	return st, nil
}

// RecordRecall writes a recall hit/miss marker for the hit-rate metric.
func (s *Store) RecordRecall(hit bool) {
	action := "recall_miss"
	if hit {
		action = "recall_hit"
	}
	s.Audit("", action, "mcp", "")
}

// QueueTelemetry stores a payload for later flush.
func (s *Store) QueueTelemetry(payload string) error {
	_, err := s.db.Exec(`INSERT INTO telemetry_queue (payload, queued_at) VALUES (?,?)`,
		payload, time.Now().Unix())
	return err
}

// ListTelemetry returns queued payloads.
func (s *Store) ListTelemetry() ([]string, error) {
	rows, err := s.db.Query(`SELECT payload FROM telemetry_queue ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ClearTelemetry removes queued payloads after a successful flush.
func (s *Store) ClearTelemetry() error {
	_, err := s.db.Exec(`DELETE FROM telemetry_queue`)
	return err
}
