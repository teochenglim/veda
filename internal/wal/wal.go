// Package wal implements Veda's write-ahead log: an append-only NDJSON file
// at ~/.veda/wal.ndjson. Writes go through a buffered channel so the MCP
// response path is never blocked on disk I/O; the async worker drains the
// file into SQLite on its next pass.
package wal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/teochenglim/veda/internal/store"
)

// Entry is one line of the WAL.
type Entry struct {
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	TS        int64  `json:"ts"`
}

// WAL is an append-only NDJSON log with a non-blocking Append.
type WAL struct {
	path string
	ch   chan Entry
	wg   sync.WaitGroup

	mu      sync.Mutex
	closing bool
}

// Open starts a WAL writer at path with the given flush-buffer depth.
func Open(path string, buf int) (*WAL, error) {
	if buf <= 0 {
		buf = 1024
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	w := &WAL{path: path, ch: make(chan Entry, buf)}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer f.Close()
		bw := bufio.NewWriter(f)
		for e := range w.ch {
			line, err := json.Marshal(e)
			if err != nil {
				continue
			}
			bw.Write(line)
			bw.WriteByte('\n')
			bw.Flush() // append-only: every entry is durable as soon as we take it
		}
	}()
	return w, nil
}

// Append queues an entry without blocking on disk. If the buffer is full the
// entry is dropped rather than stalling the MCP response — the WAL is a
// durability optimization, not a hard guarantee, in MVP.
func (w *WAL) Append(e Entry) {
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	select {
	case w.ch <- e:
	default:
	}
}

// Close drains pending entries and stops the writer goroutine.
func (w *WAL) Close() error {
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		return nil
	}
	w.closing = true
	w.mu.Unlock()
	close(w.ch)
	w.wg.Wait()
	return nil
}

// Drain reads every entry currently in the file, then atomically truncates
// it (rotate-write: rename away, recreate). Returns the drained entries.
func (w *WAL) Drain() ([]store.Turn, error) {
	f, err := os.Open(w.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var turns []store.Turn
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Content == "" {
			continue
		}
		turns = append(turns, store.Turn{
			SessionID: e.SessionID, Role: e.Role, Content: e.Content, TS: e.TS,
		})
	}
	f.Close()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(turns) == 0 {
		return nil, nil
	}
	tmp := w.path + ".draining"
	if err := os.Rename(w.path, tmp); err != nil {
		return nil, fmt.Errorf("rotate wal: %w", err)
	}
	nf, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Rename(tmp, w.path)
		return nil, err
	}
	nf.Close()
	os.Remove(tmp)
	return turns, nil
}
