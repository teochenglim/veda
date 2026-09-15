// Package ui serves Veda's local review UI at 127.0.0.1:7331 with four tabs:
// Pending, All, Audit, Digest. The static frontend is embedded in the binary.
package ui

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"time"

	"github.com/teochenglim/veda/internal/store"
)

var errNotFound = errors.New("pending turn not found")

//go:embed static
var staticFS embed.FS

// Server wires HTTP handlers to the store.
type Server struct {
	Store *store.Store
}

func (sv *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/pending", sv.handlePending)
	mux.HandleFunc("POST /api/pending/approve", sv.handleApprove)
	mux.HandleFunc("POST /api/pending/reject", sv.handleReject)
	mux.HandleFunc("GET /api/memories", sv.handleMemories)
	mux.HandleFunc("POST /api/memories/update", sv.handleUpdate)
	mux.HandleFunc("POST /api/memories/delete", sv.handleDelete)
	mux.HandleFunc("GET /api/audit", sv.handleAudit)
	mux.HandleFunc("GET /api/conflicts", sv.handleConflicts)
	mux.HandleFunc("POST /api/conflicts/resolve", sv.handleResolveConflict)
	mux.HandleFunc("GET /api/digest", sv.handleDigest)
	return mux
}

// Handler returns the UI's http.Handler (also used by tests).
func (sv *Server) Handler() http.Handler { return sv.routes() }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	http.Error(w, err.Error(), code)
}

func (sv *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	p, err := sv.Store.ListPending(0)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if p == nil {
		p = []*store.PendingTurn{}
	}
	writeJSON(w, p)
}

func (sv *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID   int64  `json:"id"`
		Text string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err)
		return
	}
	pending, err := sv.Store.ListPending(0)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	var found *store.PendingTurn
	for _, p := range pending {
		if p.ID == in.ID {
			found = p
			break
		}
	}
	if found == nil {
		writeErr(w, 404, errNotFound)
		return
	}
	content := in.Text
	if content == "" {
		content = found.Content
	}
	_, err = sv.Store.Remember(&store.Memory{Content: content, Type: "fact", AgentID: "ui"}, "ui")
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := sv.Store.DropPending(in.ID); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (sv *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := sv.Store.DropPending(in.ID); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (sv *Server) handleMemories(w http.ResponseWriter, r *http.Request) {
	mems, err := sv.Store.List(r.URL.Query().Get("type"), r.URL.Query().Get("agent_id"), time.Time{}, time.Time{}, 0, r.URL.Query().Get("superseded") == "1")
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if mems == nil {
		mems = []*store.Memory{}
	}
	writeJSON(w, mems)
}

func (sv *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := sv.Store.Update(in.ID, in.Content); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (sv *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err)
		return
	}
	ok, err := sv.Store.Forget(in.ID, "ui", "deleted in review UI")
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"deleted": ok})
}

func (sv *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := sv.Store.ListAudit(0)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if entries == nil {
		entries = []*store.AuditEntry{}
	}
	writeJSON(w, entries)
}

func (sv *Server) handleConflicts(w http.ResponseWriter, r *http.Request) {
	conflicts, err := sv.Store.ListConflicts(0)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if conflicts == nil {
		conflicts = []*store.Conflict{}
	}
	writeJSON(w, conflicts)
}

func (sv *Server) handleResolveConflict(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID         int64  `json:"id"`
		Resolution string `json:"resolution"` // new | old | both
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := sv.Store.ResolveConflict(in.ID, in.Resolution, "ui"); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, 404, err)
			return
		}
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (sv *Server) handleDigest(w http.ResponseWriter, r *http.Request) {
	st, err := sv.Store.CollectStats()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, st)
}
