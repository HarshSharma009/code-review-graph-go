package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/harshsharma/code-review-graph-go/internal/graph"
)

func setupTestServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	store, err := graph.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	srv := NewServer(store, "/tmp/test-repo")
	buf := &bytes.Buffer{}
	srv.writer = buf
	return srv, buf
}

func sendRequest(t *testing.T, srv *Server, buf *bytes.Buffer, method string, params any) jsonRPCResponse {
	t.Helper()
	var rawParams json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		rawParams = b
	}

	id, _ := json.Marshal(1)
	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  rawParams,
	}

	buf.Reset()
	srv.handleRequest(context.Background(), req)

	raw := buf.String()
	idx := strings.Index(raw, "\r\n\r\n")
	if idx < 0 {
		t.Fatalf("no header separator found in response: %q", raw)
	}
	body := raw[idx+4:]

	var resp jsonRPCResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("failed to parse response: %v; raw=%q", err, body)
	}
	return resp
}

func TestInitialize(t *testing.T) {
	srv, buf := setupTestServer(t)
	resp := sendRequest(t, srv, buf, "initialize", nil)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not map: %T", resp.Result)
	}
	if result["protocolVersion"] != protocolVersion {
		t.Errorf("wrong protocol version: %v", result["protocolVersion"])
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != serverName {
		t.Errorf("wrong server name: %v", info["name"])
	}
}

func TestToolsList(t *testing.T) {
	srv, buf := setupTestServer(t)
	resp := sendRequest(t, srv, buf, "tools/list", nil)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}

	result, _ := resp.Result.(map[string]any)
	toolsRaw, _ := result["tools"].([]any)
	if len(toolsRaw) == 0 {
		t.Fatal("no tools returned")
	}

	names := make(map[string]bool)
	for _, tr := range toolsRaw {
		tm, _ := tr.(map[string]any)
		names[tm["name"].(string)] = true
	}

	required := []string{
		"build_or_update_graph",
		"get_minimal_context",
		"get_impact_radius",
		"query_graph",
		"semantic_search_nodes",
		"list_graph_stats",
		"find_large_functions",
		"get_review_context",
		"detect_changes",
		"visualize_graph",
	}
	for _, name := range required {
		if !names[name] {
			t.Errorf("missing tool: %s", name)
		}
	}
}

func TestToolsCallMinimalContext(t *testing.T) {
	srv, buf := setupTestServer(t)
	resp := sendRequest(t, srv, buf, "tools/call", map[string]any{
		"name":      "get_minimal_context",
		"arguments": map[string]any{},
	})

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}

	result, _ := resp.Result.(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("no content in response")
	}
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	if text == "" {
		t.Error("empty text in content")
	}
	if strings.Contains(text, "Error") {
		t.Errorf("unexpected error in response: %s", text)
	}
}

func TestToolsCallListStats(t *testing.T) {
	srv, buf := setupTestServer(t)
	resp := sendRequest(t, srv, buf, "tools/call", map[string]any{
		"name":      "list_graph_stats",
		"arguments": map[string]any{},
	})

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}

	result, _ := resp.Result.(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("no content returned")
	}
}

func TestToolsCallUnknownTool(t *testing.T) {
	srv, buf := setupTestServer(t)
	resp := sendRequest(t, srv, buf, "tools/call", map[string]any{
		"name":      "nonexistent_tool",
		"arguments": map[string]any{},
	})

	if resp.Error == nil {
		t.Fatal("expected error for unknown tool")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("expected code -32602, got %d", resp.Error.Code)
	}
}

func TestToolsCallSearch(t *testing.T) {
	srv, buf := setupTestServer(t)
	resp := sendRequest(t, srv, buf, "tools/call", map[string]any{
		"name":      "semantic_search_nodes",
		"arguments": map[string]any{"query": "test", "limit": 5},
	})

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}
}

func TestPing(t *testing.T) {
	srv, buf := setupTestServer(t)
	resp := sendRequest(t, srv, buf, "ping", nil)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}
}

func TestMethodNotFound(t *testing.T) {
	srv, buf := setupTestServer(t)
	resp := sendRequest(t, srv, buf, "nonexistent/method", nil)

	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("expected code -32601, got %d", resp.Error.Code)
	}
}
