package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/teochenglim/veda/internal/store"
)

// session wires an in-memory MCP client to a server over a test store.
func session(t *testing.T) (*mcp.ClientSession, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/veda.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv := New(s, "test", "test-agent")
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	ct, st := mcp.NewInMemoryTransports()
	go srv.Run(ctx, st) // blocks until the transport closes
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, s
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("%s: no content", tool)
	}
	text, ok := res.Content[0].(interface{ Text() string })
	var out string
	if ok {
		out = text.Text()
	} else {
		b, _ := json.Marshal(res.StructuredContent)
		out = string(b)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%s: content is not JSON: %q (%v)", tool, out, err)
	}
	return m
}

// AC2: the MCP server exposes exactly the five spec tools over stdio-shaped
// sessions: remember, recall, list, forget, export.
func TestAC2_ToolCatalog(t *testing.T) {
	cs, _ := session(t)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"remember", "recall", "list", "forget", "export"} {
		if !names[want] {
			t.Errorf("missing spec tool %q; got %v", want, names)
		}
	}
}

// AC2: the remember → recall → forget round trip through MCP.
func TestAC2_RememberRecallForget(t *testing.T) {
	cs, _ := session(t)
	out := call(t, cs, "remember", map[string]any{
		"content": "User prefers aisle seats on red-eye flights",
		"type":    "preference",
	})
	id, _ := out["id"].(string)
	if !strings.HasPrefix(id, "mem_") {
		t.Fatalf("remember must return an id, got %v", out)
	}

	rec := call(t, cs, "recall", map[string]any{"query": "aisle seats"})
	mems, _ := rec["memories"].([]any)
	if len(mems) != 1 {
		t.Fatalf("recall failed: %v", rec)
	}

	lst := call(t, cs, "list", map[string]any{"type": "preference"})
	if l, _ := lst["memories"].([]any); len(l) != 1 {
		t.Fatalf("list failed: %v", lst)
	}

	frg := call(t, cs, "forget", map[string]any{"id": id})
	if frg["deleted"] != true {
		t.Fatalf("forget must return deleted:true, got %v", frg)
	}
	rec2 := call(t, cs, "recall", map[string]any{"query": "aisle seats"})
	if m, _ := rec2["memories"].([]any); len(m) != 0 {
		t.Fatalf("forgotten memory must not recall: %v", rec2)
	}
}

// AC2: export returns the full store as JSON.
func TestAC2_ExportTool(t *testing.T) {
	cs, _ := session(t)
	call(t, cs, "remember", map[string]any{"content": "User's timezone is Asia/Singapore", "type": "fact"})
	exp := call(t, cs, "export", map[string]any{})
	raw, _ := exp["json"].(string)
	if !strings.Contains(raw, "Asia/Singapore") {
		t.Fatalf("export must contain stored memories, got: %.200s", raw)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("export json field must be valid JSON: %v", err)
	}
	if _, ok := doc["memories"]; !ok {
		t.Fatal("export must have a memories key")
	}
}

// recall is audited with hit markers for the recall-hit-rate metric.
func TestRecallAudited(t *testing.T) {
	cs, s := session(t)
	call(t, cs, "recall", map[string]any{"query": "nothing matches this"})
	audit, _ := s.ListAudit(0)
	sawMiss := false
	for _, a := range audit {
		if a.Action == "recall_miss" {
			sawMiss = true
		}
	}
	if !sawMiss {
		t.Fatal("a no-hit recall must be audited as recall_miss")
	}
}

func jsonMarshalOut(v any) ([]byte, error) { return json.Marshal(v) }
