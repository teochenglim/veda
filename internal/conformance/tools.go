package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// requiredTools is the draft-02 tool contract set.
var requiredTools = []string{"remember", "recall", "list", "forget", "export"}

// commandFor builds the stdio transport command from argv; env nil ⇒ inherit.
func commandFor(command, env []string) *exec.Cmd {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = env
	return cmd
}

// RunTools drives the MCP server at command (argv) over stdio and scores it
// against the UOMP draft-02 tool contract (spec/uomp-draft-02.md §2). env
// replaces the child environment when non-nil (nil inherits). Every failure
// names the violated rule. A non-zero exit or protocol failure is itself a
// failed check.
func RunTools(command []string, env []string) *Report {
	rep := &Report{Dir: strings.Join(command, " ")} // Dir doubles as the subject here
	add := func(name string, ok bool, format string, args ...any) {
		detail := ""
		if !ok {
			detail = fmt.Sprintf(format, args...)
		}
		rep.Results = append(rep.Results, Result{Name: name, OK: ok, Detail: detail})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if len(command) == 0 {
		add("handshake", false, "no --command given")
		return rep
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "uomp-conformance", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: commandFor(command, env)}, nil)
	if err != nil {
		add("handshake", false, "connect failed: %v", err)
		return rep
	}
	defer session.Close()
	add("handshake", true, "")

	catalog, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		add("tools-catalog", false, "ListTools failed: %v", err)
		return rep
	}
	have := map[string]bool{}
	for _, tool := range catalog.Tools {
		have[tool.Name] = true
	}
	var missing []string
	for _, name := range requiredTools {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		add("tools-catalog", false, "missing tools: %s", strings.Join(missing, ", "))
		return rep
	}
	add("tools-catalog", true, "")

	// --- remember ---------------------------------------------------------

	const marker = "uompconformance"
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "remember", Arguments: map[string]any{"content": marker + " prefers aisle seats", "type": "preference"},
	})
	if toolFailed(res, err) {
		add("remember-saves", false, "remember failed: %s", toolErrText(res, err))
		return rep
	}
	out := structuredOf(res)
	id1, _ := out["id"].(string)
	if id1 == "" {
		add("remember-saves", false, "remember returned no id (got %v)", out["id"])
		return rep
	}
	add("remember-saves", true, "")

	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{}})
	if !toolFailed(res, err) {
		add("remember-requires-content", false, "remember with no content must fail (rule §2.1)")
	} else {
		add("remember-requires-content", true, "")
	}

	// --- recall -----------------------------------------------------------

	// six more marker memories so the default-limit check has something to
	// trim; one of them is typed "fact" for the list-filter check
	for i := 2; i <= 7; i++ {
		args := map[string]any{"content": fmt.Sprintf("%s fact number %d", marker, i)}
		if i == 3 {
			args["type"] = "fact"
		}
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: args}); err != nil {
			add("recall-finds", false, "fixture remember %d failed: %v", i, err)
			return rep
		}
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "recall", Arguments: map[string]any{"query": marker},
	})
	if toolFailed(res, err) {
		add("recall-finds", false, "recall failed: %s", toolErrText(res, err))
		return rep
	}
	memories := memoriesOf(res)
	if len(memories) == 0 {
		add("recall-finds", false, "recall found none of the %s fixtures", marker)
		return rep
	}
	for _, m := range memories {
		if _, ok := m["id"].(string); !ok {
			add("recall-finds", false, "memory result missing string id: %v", m)
			return rep
		}
		if _, ok := m["content"].(string); !ok {
			add("recall-finds", false, "memory result missing string content: %v", m)
			return rep
		}
	}
	add("recall-finds", true, "")

	if len(memories) > 5 {
		add("recall-default-limit", false, "default recall returned %d results; the contract default is 5 (rule §2.2)", len(memories))
	} else {
		add("recall-default-limit", true, "")
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "recall", Arguments: map[string]any{"query": marker, "limit": float64(2)},
	})
	if toolFailed(res, err) {
		add("recall-limit-flag", false, "recall with limit failed: %s", toolErrText(res, err))
	} else if got := len(memoriesOf(res)); got > 2 {
		add("recall-limit-flag", false, "recall limit=2 returned %d results", got)
	} else {
		add("recall-limit-flag", true, "")
	}

	// --- list -------------------------------------------------------------

	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "list", Arguments: map[string]any{"type": "fact"},
	})
	if toolFailed(res, err) {
		add("list-filters", false, "list with type filter failed: %s", toolErrText(res, err))
		return rep
	}
	facts := memoriesOf(res)
	if len(facts) == 0 {
		add("list-filters", false, `list type="fact" returned nothing although fact fixtures were stored`)
		return rep
	}
	for _, m := range facts {
		if t, _ := m["type"].(string); t != "fact" {
			add("list-filters", false, "list type filter leaked a %q memory", t)
			return rep
		}
	}
	add("list-filters", true, "")
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "list", Arguments: map[string]any{"include_superseded": true},
	})
	if toolFailed(res, err) {
		add("list-include-superseded", false, "include_superseded must be accepted (rule §2.3): %s", toolErrText(res, err))
	} else {
		add("list-include-superseded", true, "")
	}

	// --- forget -----------------------------------------------------------

	const missingID = "mem_conformance_missing000000000000000000"
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "forget", Arguments: map[string]any{"id": missingID},
	})
	if toolFailed(res, err) {
		add("forget-unknown-id", false, "forget of an unknown id must not error — it returns {deleted:false} (rule §2.4): %s", toolErrText(res, err))
	} else if d, _ := structuredOf(res)["deleted"].(bool); d {
		add("forget-unknown-id", false, "forget of an unknown id must return deleted=false (rule §2.4)")
	} else {
		add("forget-unknown-id", true, "")
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "forget", Arguments: map[string]any{"id": id1},
	})
	if toolFailed(res, err) {
		add("forget-deletes", false, "forget failed: %s", toolErrText(res, err))
	} else {
		deleted := false
		if d, _ := structuredOf(res)["deleted"].(bool); d {
			deleted = true
		}
		stillThere := false
		res, err = session.CallTool(ctx, &mcp.CallToolParams{
			Name: "recall", Arguments: map[string]any{"query": marker + " prefers aisle"},
		})
		if !toolFailed(res, err) {
			for _, m := range memoriesOf(res) {
				if mid, _ := m["id"].(string); mid == id1 {
					stillThere = true
				}
			}
		}
		if deleted && !stillThere {
			add("forget-deletes", true, "")
		} else {
			var why string
			if !deleted {
				why = "forget of a live id returned deleted=false"
			} else {
				why = "forgotten memory is still recallable — forget must remove it from results"
			}
			add("forget-deletes", false, "%s (rule §2.4)", why)
		}
	}

	// --- export -----------------------------------------------------------

	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "export", Arguments: map[string]any{}})
	if toolFailed(res, err) {
		add("export-shape", false, "export failed: %s", toolErrText(res, err))
		return rep
	}
	docStr, _ := structuredOf(res)["json"].(string)
	if docStr == "" {
		add("export-shape", false, "export returned no json field (rule §2.5)")
		return rep
	}
	if viols := ValidateExport([]byte(docStr)); len(viols) > 0 {
		add("export-shape", false, "export violates draft-01: %s", strings.Join(viols, "; "))
		return rep
	}
	var doc struct {
		Audit []map[string]any `json:"audit"`
	}
	_ = json.Unmarshal([]byte(docStr), &doc)
	forgot := false
	for _, entry := range doc.Audit {
		if entry["memory_id"] == id1 && entry["action"] == "forget" {
			forgot = true
		}
	}
	if !forgot {
		add("forget-audits", false, "no audit entry (action=forget) for %s — forget must record the deletion (rule §2.4)", id1)
	} else {
		add("forget-audits", true, "")
	}
	add("export-shape", true, "")

	// --- error behavior ---------------------------------------------------

	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "uomp_nonexistent_tool", Arguments: map[string]any{},
	})
	if !toolFailed(res, err) {
		add("unknown-tool-errors", false, "calling an unknown tool must fail (rule §2.6)")
	} else {
		add("unknown-tool-errors", true, "")
	}

	rep.OK = true
	for _, res := range rep.Results {
		if !res.OK {
			rep.OK = false
		}
	}
	return rep
}

// toolFailed reports whether a tool call surfaced an error, either as a
// protocol-level error (err) or an in-band isError result (both are
// "the tool failed"; draft-02 prefers the in-band form).
func toolFailed(res *mcp.CallToolResult, err error) bool {
	return err != nil || (res != nil && res.IsError)
}

func toolErrText(res *mcp.CallToolResult, err error) string {
	if err != nil {
		return err.Error()
	}
	if res != nil && len(res.Content) > 0 {
		if txt, ok := res.Content[0].(*mcp.TextContent); ok {
			return txt.Text
		}
	}
	return "isError result"
}

// structuredOf decodes the structured content of a tool result. Typed MCP
// outputs arrive as generic JSON values.
func structuredOf(res *mcp.CallToolResult) map[string]any {
	if res == nil || res.StructuredContent == nil {
		return nil
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func memoriesOf(res *mcp.CallToolResult) []map[string]any {
	out := structuredOf(res)
	if out == nil {
		return nil
	}
	raw, ok := out["memories"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var memories []map[string]any
	if json.Unmarshal(b, &memories) != nil {
		return nil
	}
	return memories
}
