package conformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/teochenglim/veda/internal/mcpserver"
	"github.com/teochenglim/veda/internal/store"
)

// TestHelperMCPProcess is the stdio MCP server the tools suite runs against
// in tests. Mode UOMP_CONFORMANCE_HELPER=conform runs Veda's real MCP layer
// (mcpserver.New); =broken runs a hand-rolled server whose forget lies —
// it answers deleted:true without deleting, tombstoning, or auditing.
func TestHelperMCPProcess(t *testing.T) {
	mode := os.Getenv("UOMP_CONFORMANCE_HELPER")
	if mode == "" {
		t.Skip("helper process for TestAC1/TestAC2")
	}

	dir, err := os.MkdirTemp("", "uomp-helper-*")
	if err != nil {
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	s, err := store.Open(filepath.Join(dir, "veda.db"))
	if err != nil {
		os.Exit(1)
	}
	defer s.Close()

	var srv *mcp.Server
	if mode == "broken" {
		srv = brokenStub(s)
	} else {
		srv = mcpserver.New(s, nil, "conformance", nil)
	}
	_ = srv.Run(context.Background(), &mcp.StdioTransport{})
}

func helperCommand(t *testing.T, mode string) ([]string, []string) {
	t.Helper()
	return []string{os.Args[0], "-test.run=^TestHelperMCPProcess$"},
		append(os.Environ(),
			"UOMP_CONFORMANCE_HELPER="+mode,
			"VEDA_HOME="+t.TempDir(),
		)
}

// TestAC1_ConformanceToolsPassesVeda is the CI drift gate for the tool
// contract: Veda's own MCP layer must satisfy draft-02 over stdio.
func TestAC1_ConformanceToolsPassesVeda(t *testing.T) {
	rep := RunTools(helperCommand(t, "conform"))
	if !rep.OK {
		t.Fatalf("veda's own MCP server violates draft-02: %+v", rep.Results)
	}
	for _, name := range []string{
		"handshake", "tools-catalog", "remember-saves", "remember-requires-content",
		"recall-finds", "recall-default-limit", "recall-limit-flag", "list-filters",
		"list-include-superseded", "forget-unknown-id", "forget-deletes",
		"forget-audits", "export-shape", "unknown-tool-errors",
	} {
		if res := result(t, rep, name); !res.OK {
			t.Fatalf("%s unexpectedly failed: %+v", name, res)
		}
	}
}

// TestAC2_ConformanceToolsFailsBrokenStub points the suite at a server with
// wrong forget semantics (deleted:true for anything, deletes nothing, audits
// nothing) and requires the violated rules to be named.
func TestAC2_ConformanceToolsFailsBrokenStub(t *testing.T) {
	rep := RunTools(helperCommand(t, "broken"))
	if rep.OK {
		t.Fatal("the broken stub must fail tools conformance")
	}
	details := allDetails(rep)
	for _, want := range []string{"forget-unknown-id", "forget-deletes", "forget-audits"} {
		if !strings.Contains(details, want) {
			t.Fatalf("expected the broken forget to violate %s, report was:\n%s", want, details)
		}
	}
}

// brokenStub implements the five tools against the real store EXCEPT forget,
// which always claims success and does nothing — the canonical violation. Its
// recall also ignores the contract's default limit.
func brokenStub(s *store.Store) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "veda-broken", Version: "0"}, nil)

	mcp.AddTool(srv, &mcp.Tool{Name: "remember"}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Content string `json:"content"`
		Type    string `json:"type,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		if in.Content == "" {
			return nil, nil, fmt.Errorf("content is required")
		}
		id, err := s.Remember(&store.Memory{Content: in.Content, Type: in.Type, Confidence: 0.9, Salience: 0.8}, "broken")
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"id": id}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{Name: "recall"}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Query string `json:"query"`
		Limit int    `json:"limit,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 1000 // deliberately not the contract default of 5
		}
		mems, err := s.Recall(in.Query, limit, 0.5)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"memories": mems}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{Name: "list"}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Type              string `json:"type,omitempty"`
		IncludeSuperseded bool   `json:"include_superseded,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		mems, err := s.List(in.Type, "", time.Time{}, time.Time{}, 0, false)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"memories": mems}, nil
	})

	// forget: THE VIOLATION — always {deleted:true}, touches nothing.
	mcp.AddTool(srv, &mcp.Tool{Name: "forget"}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		ID string `json:"id"`
	}) (*mcp.CallToolResult, any, error) {
		_ = in
		return nil, map[string]any{"deleted": true}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{Name: "export"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		data, err := s.Export()
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"json": string(data)}, nil
	})
	return srv
}

func allDetails(rep *Report) string {
	var sb strings.Builder
	for _, r := range rep.Results {
		if !r.OK {
			fmt.Fprintf(&sb, "%s: %s\n", r.Name, r.Detail)
		}
	}
	return sb.String()
}
