package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/harshsharma/code-review-graph-go/internal/graph"
	"github.com/harshsharma/code-review-graph-go/internal/hints"
	"github.com/harshsharma/code-review-graph-go/internal/prompts"
	"github.com/harshsharma/code-review-graph-go/internal/tools"
)

const (
	protocolVersion  = "2024-11-05"
	serverName       = "code-review-graph"
	serverVersion    = "1.0.0"
)

// Server implements the Model Context Protocol over stdio (JSON-RPC 2.0).
type Server struct {
	store    *graph.Store
	repoRoot string
	registry *tools.Registry
	toolMap  map[string]tools.ToolDef
	session  *hints.Session
	mu       sync.Mutex
	writer   io.Writer
	reader   io.Reader
}

func NewServer(store *graph.Store, repoRoot string) *Server {
	reg := tools.NewRegistry(store, repoRoot)
	allTools := reg.AllTools()
	toolMap := make(map[string]tools.ToolDef, len(allTools))
	for _, t := range allTools {
		toolMap[t.Name] = t
	}
	return &Server{
		store:    store,
		repoRoot: repoRoot,
		registry: reg,
		toolMap:  toolMap,
		session:  hints.NewSession(),
		writer:   os.Stdout,
		reader:   os.Stdin,
	}
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Run starts the MCP server, reading JSON-RPC requests from stdin and writing responses to stdout.
func (s *Server) Run(ctx context.Context) error {
	slog.Info("MCP server starting", "transport", "stdio")
	scanner := bufio.NewScanner(s.reader)

	// MCP uses Content-Length framing, but also supports newline-delimited JSON.
	// Support both: try reading Content-Length headers, fall back to line-delimited.
	buf := make([]byte, 0, 4*1024*1024)
	scanner.Buffer(buf, 4*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Handle Content-Length header framing
		if strings.HasPrefix(line, "Content-Length:") {
			lenStr := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			contentLen, err := strconv.Atoi(lenStr)
			if err != nil {
				slog.Warn("invalid Content-Length", "value", lenStr)
				continue
			}
			// Skip blank separator line
			scanner.Scan()

			body := make([]byte, contentLen)
			n, err := io.ReadFull(s.reader, body)
			if err != nil {
				slog.Warn("failed to read content body", "err", err, "expected", contentLen, "got", n)
				continue
			}
			line = string(body)
		}

		var req jsonRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			slog.Warn("failed to parse JSON-RPC request", "err", err)
			s.writeError(nil, -32700, "Parse error", nil)
			continue
		}

		if req.JSONRPC != "2.0" && req.JSONRPC != "" {
			s.writeError(req.ID, -32600, "Invalid Request: expected jsonrpc 2.0", nil)
			continue
		}

		s.handleRequest(ctx, &req)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanner error: %w", err)
	}
	return nil
}

func (s *Server) handleRequest(ctx context.Context, req *jsonRPCRequest) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "initialized":
		slog.Info("client initialized")
	case "tools/list":
		s.handleToolsList(req)
	case "tools/call":
		s.handleToolsCall(ctx, req)
	case "prompts/list":
		s.handlePromptsList(req)
	case "prompts/get":
		s.handlePromptsGet(req)
	case "ping":
		s.writeResult(req.ID, map[string]any{})
	default:
		// Notifications (no id) are silently ignored per spec
		if req.ID != nil {
			s.writeError(req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method), nil)
		}
	}
}

func (s *Server) handleInitialize(req *jsonRPCRequest) {
	s.writeResult(req.ID, map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{
				"listChanged": false,
			},
			"prompts": map[string]any{
				"listChanged": false,
			},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": serverVersion,
		},
	})
}

func (s *Server) handleToolsList(req *jsonRPCRequest) {
	allTools := s.registry.AllTools()
	toolDefs := make([]map[string]any, len(allTools))
	for i, t := range allTools {
		toolDefs[i] = map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		}
	}
	s.writeResult(req.ID, map[string]any{"tools": toolDefs})
}

func (s *Server) handleToolsCall(ctx context.Context, req *jsonRPCRequest) {
	var callParams struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			s.writeError(req.ID, -32602, "Invalid params", nil)
			return
		}
	}

	tool, ok := s.toolMap[callParams.Name]
	if !ok {
		s.writeError(req.ID, -32602, fmt.Sprintf("Unknown tool: %s", callParams.Name), nil)
		return
	}

	slog.Info("tool call", "tool", callParams.Name)

	result, err := tool.Handler(ctx, callParams.Arguments)
	if err != nil {
		slog.Warn("tool error", "tool", callParams.Name, "err", err)
		s.writeResult(req.ID, map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": fmt.Sprintf("Error: %v", err),
			}},
			"isError": true,
		})
		return
	}

	// Generate hints if result is a map
	resultMap, isMap := result.(map[string]any)
	if isMap && s.session != nil {
		h := hints.GenerateHints(callParams.Name, resultMap, s.session)
		resultMap["_hints"] = h
	}

	text, err := json.Marshal(result)
	if err != nil {
		s.writeResult(req.ID, map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": fmt.Sprintf("Error serializing result: %v", err),
			}},
			"isError": true,
		})
		return
	}

	s.writeResult(req.ID, map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": string(text),
		}},
	})
}

func (s *Server) handlePromptsList(req *jsonRPCRequest) {
	allPrompts := prompts.AllPrompts()
	defs := make([]map[string]any, len(allPrompts))
	for i, p := range allPrompts {
		def := map[string]any{
			"name":        p.Name,
			"description": p.Description,
		}
		if len(p.Arguments) > 0 {
			args := make([]map[string]any, len(p.Arguments))
			for j, a := range p.Arguments {
				args[j] = map[string]any{
					"name":        a.Name,
					"description": a.Description,
					"required":    a.Required,
				}
			}
			def["arguments"] = args
		}
		defs[i] = def
	}
	s.writeResult(req.ID, map[string]any{"prompts": defs})
}

func (s *Server) handlePromptsGet(req *jsonRPCRequest) {
	var getParams struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &getParams); err != nil {
			s.writeError(req.ID, -32602, "Invalid params", nil)
			return
		}
	}

	allPrompts := prompts.AllPrompts()
	for _, p := range allPrompts {
		if p.Name == getParams.Name {
			messages := p.Handler(getParams.Arguments)
			msgDicts := make([]map[string]any, len(messages))
			for i, m := range messages {
				msgDicts[i] = map[string]any{
					"role": m.Role,
					"content": map[string]any{
						"type": "text",
						"text": m.Content,
					},
				}
			}
			s.writeResult(req.ID, map[string]any{
				"description": p.Description,
				"messages":    msgDicts,
			})
			return
		}
	}
	s.writeError(req.ID, -32602, fmt.Sprintf("Unknown prompt: %s", getParams.Name), nil)
}

func (s *Server) writeResult(id json.RawMessage, result any) {
	s.sendResponse(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func (s *Server) writeError(id json.RawMessage, code int, message string, data any) {
	s.sendResponse(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonRPCError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	})
}

func (s *Server) sendResponse(resp jsonRPCResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	body, err := json.Marshal(resp)
	if err != nil {
		slog.Error("failed to marshal response", "err", err)
		return
	}

	// Write with Content-Length framing per MCP spec
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := fmt.Fprint(s.writer, header); err != nil {
		slog.Error("failed to write header", "err", err)
		return
	}
	if _, err := s.writer.Write(body); err != nil {
		slog.Error("failed to write body", "err", err)
	}
}
