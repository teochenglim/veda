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
	TTL           int64   `json:"ttl,omitempty"` // unix seconds; 0 = never expires
	Deleted       bool    `json:"-"`
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
  ttl INTEGER, deleted INTEGER DEFAULT 0
);
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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return s, nil
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
	rows, err := s.db.Query(`SELECT `+memCols+` WHERE deleted = 0 AND (ttl = 0 OR ttl > ?)
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
		JOIN memories m ON m.id = v.memory_id WHERE m.deleted = 0`).Scan(&n)
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
		WHERE m.deleted = 0 AND (m.ttl = 0 OR m.ttl > ?) AND m.confidence >= 0`,
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
		if len(fts) > limit {
			fts = fts[:limit]
		}
		return fts, nil
	}

	qvec, err := s.embedQuery(ctx, em, query)
	if err != nil {
		if len(fts) > limit {
			fts = fts[:limit]
		}
		return fts, nil // provider failure ⇒ keyword-only, never an error
	}
	rows, err := s.loadVectors()
	if err != nil {
		if len(fts) > limit {
			fts = fts[:limit]
		}
		return fts, nil
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
			if len(fts) > limit {
				fts = fts[:limit]
			}
			return fts, nil // no vectors yet ⇒ keyword-only fallback
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
	return m.ID, nil
}

func scanMemory(row interface{ Scan(...any) error }) (*Memory, error) {
	var m Memory
	var deleted int
	err := row.Scan(&m.ID, &m.Type, &m.Content, &m.SourceTurnIDs, &m.AgentID,
		&m.Confidence, &m.Salience, &m.CreatedAt, &m.UpdatedAt, &m.TTL, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.Deleted = deleted != 0
	return &m, nil
}

const memCols = `id, type, content, source_turn_ids, agent_id, confidence, salience, created_at, updated_at, ttl, deleted FROM memories`

// Get fetches one live (non-deleted, non-expired) memory by id.
func (s *Store) Get(id string) (*Memory, error) {
	m, err := scanMemory(s.db.QueryRow(`SELECT `+memCols+` WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if m.Deleted || (m.TTL > 0 && m.TTL <= time.Now().Unix()) {
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
		return s.List("", "", time.Time{}, time.Time{}, limit)
	}
	match := ftsQuery(query)
	rows, err := s.db.Query(`SELECT m.id, m.type, m.content, m.source_turn_ids, m.agent_id,
		m.confidence, m.salience, m.created_at, m.updated_at, m.ttl, m.deleted
		FROM memories m
		JOIN memories_fts f ON f.rowid = m.rowid
		WHERE memories_fts MATCH ? AND m.deleted = 0 AND m.confidence >= ?
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
	rows, err = s.db.Query(`SELECT `+memCols+` WHERE deleted = 0 AND confidence >= ?
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
// ignored. limit <= 0 means no limit.
func (s *Store) List(memType, agentID string, since, until time.Time, limit int) ([]*Memory, error) {
	where := []string{"deleted = 0", "(ttl = 0 OR ttl > ?)"}
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
	res, err := s.db.Exec(`UPDATE memories SET content = ?, updated_at = ? WHERE id = ? AND deleted = 0`,
		content, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.Audit(id, "update", "ui", "edited in review UI")
}

// Forget soft-deletes a memory and audits it.
func (s *Store) Forget(id, actor, reason string) (bool, error) {
	res, err := s.db.Exec(`UPDATE memories SET deleted = 1, updated_at = ? WHERE id = ? AND deleted = 0`,
		time.Now().Unix(), id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
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
	memRows, err := s.db.Query(`SELECT ` + memCols + ` WHERE deleted = 0 ORDER BY created_at`)
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
	Turns       int64  `json:"turns"`
	Sessions    int64  `json:"sessions"`
	RecallCalls int64  `json:"recall_calls"`
	RecallHits  int64  `json:"recall_hits"`
	ForgetCount int64  `json:"forget_count"`
}

// CollectStats computes anonymous counts from the DB.
func (s *Store) CollectStats() (*Stats, error) {
	st := &Stats{TS: time.Now().Unix()}
	var misses int64
	for _, q := range []struct {
		sql  string
		dest *int64
	}{
		{`SELECT COUNT(*) FROM memories WHERE deleted = 0`, &st.Memories},
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
