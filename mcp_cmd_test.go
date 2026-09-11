package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// An MCP client against the real server, over the in-memory transport — the
// same conversation a harness has with `agent-inbox mcp` over stdio, minus
// the pipe. If the tools do not answer through the protocol, the fact that
// the functions beneath them are tested is worth nothing to a harness.

func TestMCPServesTheFleet(t *testing.T) {
	fleetEnv(t)

	ctx, contextCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer contextCancel()

	srv := mcp.NewServer(&mcp.Implementation{Name: "agent-inbox", Version: "test"}, nil)
	addFleetTools(srv)
	st, ct := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: "test"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })

	// The tool list is the contract: a harness decides what it can do from
	// this alone.
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, tool := range list.Tools {
		have[tool.Name] = true
	}
	for _, want := range []string{"fleet_status", "send", "git_query", "history", "notes_list", "note_add", "note_drop", "add_project", "logbook_read", "logbook_add"} {
		if !have[want] {
			t.Errorf("tool %q missing from tools/list", want)
		}
	}

	// fleet_status answers with the fleet.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fleet_status"})
	if err != nil {
		t.Fatalf("fleet_status: %v", err)
	}
	if res.IsError {
		t.Fatalf("fleet_status errored: %+v", res.Content)
	}
	var doc fleetStatusDoc
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("structured content is not the status doc: %v (%s)", err, b)
	}
	if len(doc.Projects) != 2 {
		t.Fatalf("projects = %d, want 2", len(doc.Projects))
	}

	// A note round trip through the protocol, and the drop that retracts it.
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "note_add",
		Arguments: map[string]any{"text": "teploy depends on neutron's client"},
	}); err != nil {
		t.Fatalf("note_add: %v", err)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "notes_list"})
	if err != nil {
		t.Fatalf("notes_list: %v", err)
	}
	b, _ = json.Marshal(res.StructuredContent)
	if !strings.Contains(string(b), "neutron's client") {
		t.Fatalf("note not listed: %s", b)
	}
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "note_drop",
		Arguments: map[string]any{"text": "neutron's client"},
	}); err != nil {
		t.Fatalf("note_drop: %v", err)
	}

	// A real turn through the protocol: send is the tool that spends money,
	// so it is the one that has to answer with the reply, the session, and
	// nothing left running afterwards.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "send",
		Arguments: map[string]any{"project": "alpha", "message": "hello", "timeout_seconds": 30},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.IsError {
		t.Fatalf("send errored: %+v", res.Content)
	}
	b, _ = json.Marshal(res.StructuredContent)
	var sent struct {
		Project   string `json:"project"`
		Status    string `json:"status"`
		SessionID string `json:"session_id"`
		Final     string `json:"final"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(b, &sent); err != nil {
		t.Fatalf("send result: %v (%s)", err, b)
	}
	if sent.Error != "" || sent.SessionID == "" || !strings.Contains(sent.Final, "[mock]") {
		t.Fatalf("send result wrong: %+v", sent)
	}

	// The logbook is the king's continuity: written through the protocol,
	// readable by whichever king comes next.
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "logbook_add",
		Arguments: map[string]any{"text": "asked alpha to introduce itself; it complied", "author": "test-harness"},
	}); err != nil {
		t.Fatalf("logbook_add: %v", err)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "logbook_read",
		Arguments: map[string]any{"last": 5},
	})
	if err != nil {
		t.Fatalf("logbook_read: %v", err)
	}
	b, _ = json.Marshal(res.StructuredContent)
	if !strings.Contains(string(b), "it complied") {
		t.Fatalf("logbook entry not read back: %s", b)
	}
}
