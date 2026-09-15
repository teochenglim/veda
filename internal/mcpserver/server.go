// Package mcpserver exposes Veda as an MCP server over stdio with the five
// spec tools: remember, recall, list, forget, export.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/teochenglim/veda/internal/store"
)

func nowUnix() int64 { return time.Now().Unix() }

func envValue(k string) string { return os.Getenv(k) }

// Version is stamped at build time via -ldflags.
var Version = "dev"

// --- tool input/output types (input schemas are inferred from these) ------

type RememberIn struct {
	Content       string `json:"content" jsonschema:"the memory to save, one durable fact or preference"`
	Type          string `json:"type,omitempty" jsonschema:"one of: preference, fact, identity, goal"`
	SourceTurnIDs string `json:"source_turn_ids,omitempty" jsonschema:"optional comma-separated turn ids this came from"`
	TTLSeconds    int64  `json:"ttl_seconds,omitempty" jsonschema:"optional seconds until this memory expires; 0 = never"`
}
type RememberOut struct {
	ID string `json:"id"`
}

type RecallIn struct {
	Query         string  `json:"query" jsonschema:"free-text search over memories"`
	Limit         int     `json:"limit,omitempty" jsonschema:"max results (default 5)"`
	MinConfidence float64 `json:"min_confidence,omitempty" jsonschema:"minimum confidence 0..1 (default 0.5)"`
	Semantic      bool    `json:"semantic,omitempty" jsonschema:"true = pure semantic (embedding) ranking; default is hybrid keyword+semantic"`
}
type RecallOut struct {
	Memories []*store.Memory `json:"memories"`
}

type ListIn struct {
	Type              string `json:"type,omitempty" jsonschema:"filter by memory type"`
	AgentID           string `json:"agent_id,omitempty" jsonschema:"filter by creating agent"`
	Since             int64  `json:"since,omitempty" jsonschema:"unix seconds lower bound on created_at"`
	Until             int64  `json:"until,omitempty" jsonschema:"unix seconds upper bound on created_at"`
	IncludeSuperseded bool   `json:"include_superseded,omitempty" jsonschema:"true = also return memories replaced by newer conflicting ones"`
}
type ListOut struct {
	Memories []*store.Memory `json:"memories"`
}

type ForgetIn struct {
	ID string `json:"id" jsonschema:"the memory id to delete"`
}
type ForgetOut struct {
	Deleted bool `json:"deleted"`
}

type ExportIn struct{}
type ExportOut struct {
	JSON string `json:"json" jsonschema:"full memory store as a JSON document"`
}

// --- server ---------------------------------------------------------------

// New builds the MCP server. embedder may be nil ⇒ keyword-only recall;
// onClientInfo (nil-safe) receives each client's self-reported name at
// initialize (v0.5 telemetry ext hook).
func New(s *store.Store, embedder store.Embedder, agentID string, onClientInfo func(name string)) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "veda", Version: Version}, &mcp.ServerOptions{
		InitializedHandler: func(ctx context.Context, req *mcp.InitializedRequest) {
			if onClientInfo != nil && req.ClientInfo() != nil {
				onClientInfo(req.ClientInfo().Name)
			}
		},
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remember",
		Description: "Save a durable fact or preference about the user into their local Veda memory store.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in RememberIn) (*mcp.CallToolResult, *RememberOut, error) {
		if in.Content == "" {
			return nil, nil, fmt.Errorf("content is required")
		}
		m := &store.Memory{
			Content: in.Content, Type: in.Type,
			SourceTurnIDs: in.SourceTurnIDs, AgentID: agentID,
			TTL: ttlFromSeconds(in.TTLSeconds),
		}
		id, err := s.Remember(m, "mcp")
		if err != nil {
			return nil, nil, err
		}
		return nil, &RememberOut{ID: id}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "recall",
		Description: "Search the user's memories for facts relevant to a query. Call this whenever personal context could help.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in RecallIn) (*mcp.CallToolResult, *RecallOut, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 5
		}
		minConf := in.MinConfidence
		if minConf <= 0 {
			minConf = 0.5
		}
		mems, err := s.RecallHybrid(ctx, in.Query, limit, minConf, embedder, in.Semantic)
		if err != nil {
			return nil, nil, err
		}
		s.RecordRecall(len(mems) > 0)
		return nil, &RecallOut{Memories: mems}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list",
		Description: "List the user's memories, optionally filtered by type, agent, or time range.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ListIn) (*mcp.CallToolResult, *ListOut, error) {
		mems, err := s.List(in.Type, in.AgentID, unixTime(in.Since), unixTime(in.Until), 0, in.IncludeSuperseded)
		if err != nil {
			return nil, nil, err
		}
		return nil, &ListOut{Memories: mems}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "forget",
		Description: "Delete one of the user's memories by id. The deletion is soft and recorded in the audit log.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ForgetIn) (*mcp.CallToolResult, *ForgetOut, error) {
		deleted, err := s.Forget(in.ID, "mcp", "forgotten via MCP tool")
		if err != nil {
			return nil, nil, err
		}
		return nil, &ForgetOut{Deleted: deleted}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "export",
		Description: "Export the user's entire memory store as a JSON document. Portable by design.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ ExportIn) (*mcp.CallToolResult, *ExportOut, error) {
		data, err := s.Export()
		if err != nil {
			return nil, nil, err
		}
		return nil, &ExportOut{JSON: string(data)}, nil
	})

	return srv
}

func unixTime(ts int64) time.Time {
	if ts <= 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// Run serves MCP over stdio until the client disconnects. embedder may be nil.
func Run(s *store.Store, embedder store.Embedder, onClientInfo func(name string)) error {
	srv := New(s, embedder, agentFromEnv(), onClientInfo)
	return srv.Run(context.Background(), &mcp.StdioTransport{})
}

func ttlFromSeconds(sec int64) int64 {
	if sec <= 0 {
		return 0
	}
	return nowUnix() + sec
}

// agentFromEnv lets MCP clients pass an agent id through the config, e.g.
// {"env": {"VEDA_AGENT_ID": "cursor"}}.
func agentFromEnv() string {
	if a := envValue("VEDA_AGENT_ID"); a != "" {
		return a
	}
	return "mcp-client"
}

// MarshalJSON helpers used by the UI to reuse store types.
func MustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
