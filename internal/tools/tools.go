package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/harshsharma/code-review-graph-go/internal/graph"
	"github.com/harshsharma/code-review-graph-go/internal/incremental"
	"github.com/harshsharma/code-review-graph-go/internal/visualization"
)

// Registry holds all MCP tool definitions with their handlers.
type Registry struct {
	store    *graph.Store
	repoRoot string
}

func NewRegistry(store *graph.Store, repoRoot string) *Registry {
	return &Registry{store: store, repoRoot: repoRoot}
}

// ToolDef describes a single MCP tool.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Handler     func(ctx context.Context, params map[string]any) (any, error)
}

// AllTools returns the list of all registered MCP tools.
func (r *Registry) AllTools() []ToolDef {
	return []ToolDef{
		r.buildOrUpdateGraphTool(),
		r.getMinimalContextTool(),
		r.getImpactRadiusTool(),
		r.queryGraphTool(),
		r.semanticSearchNodesTool(),
		r.listGraphStatsTool(),
		r.findLargeFunctionsTool(),
		r.getReviewContextTool(),
		r.detectChangesTool(),
		r.visualizeTool(),
	}
}

func (r *Registry) buildOrUpdateGraphTool() ToolDef {
	return ToolDef{
		Name:        "build_or_update_graph",
		Description: "Build or incrementally update the code knowledge graph. Use full_rebuild=true for a complete rebuild, or false for incremental update of changed files only.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"full_rebuild": map[string]any{"type": "boolean", "description": "If true, rebuild the entire graph. If false, only update changed files.", "default": false},
				"base":         map[string]any{"type": "string", "description": "Git diff base for incremental updates", "default": "HEAD~1"},
			},
		},
		Handler: func(ctx context.Context, params map[string]any) (any, error) {
			fullRebuild, _ := params["full_rebuild"].(bool)
			base, _ := params["base"].(string)
			if base == "" {
				base = "HEAD~1"
			}

			if fullRebuild {
				result, err := incremental.FullBuild(ctx, r.repoRoot, r.store)
				if err != nil {
					return nil, fmt.Errorf("full build failed: %w", err)
				}
				return result, nil
			}

			result, err := incremental.IncrementalUpdate(ctx, r.repoRoot, r.store, base, nil)
			if err != nil {
				return nil, fmt.Errorf("incremental update failed: %w", err)
			}
			return result, nil
		},
	}
}

func (r *Registry) getMinimalContextTool() ToolDef {
	return ToolDef{
		Name:        "get_minimal_context",
		Description: "Get ultra-compact context for any task (~100 tokens). Always call this first before other graph tools to understand the codebase structure.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Handler: func(ctx context.Context, params map[string]any) (any, error) {
			stats, err := r.store.GetStats()
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"nodes":        stats.TotalNodes,
				"edges":        stats.TotalEdges,
				"files":        stats.FilesCount,
				"languages":    stats.Languages,
				"last_updated": stats.LastUpdated,
				"nodes_by_kind": stats.NodesByKind,
				"edges_by_kind": stats.EdgesByKind,
			}, nil
		},
	}
}

func (r *Registry) getImpactRadiusTool() ToolDef {
	return ToolDef{
		Name:        "get_impact_radius",
		Description: "Analyze the blast radius of changed files. Returns changed nodes, impacted nodes, impacted files, and connecting edges.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"changed_files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "List of changed file paths"},
				"max_depth":     map[string]any{"type": "integer", "description": "Maximum BFS depth", "default": 2},
				"max_nodes":     map[string]any{"type": "integer", "description": "Maximum nodes to return", "default": 500},
			},
			"required": []string{"changed_files"},
		},
		Handler: func(ctx context.Context, params map[string]any) (any, error) {
			files, _ := toStringSlice(params["changed_files"])
			maxDepth := intParam(params, "max_depth", 2)
			maxNodes := intParam(params, "max_nodes", 500)

			result, err := r.store.GetImpactRadius(files, maxDepth, maxNodes)
			if err != nil {
				return nil, err
			}

			changedDicts := make([]map[string]any, len(result.ChangedNodes))
			for i, n := range result.ChangedNodes {
				changedDicts[i] = graph.NodeToDict(n)
			}
			impactedDicts := make([]map[string]any, len(result.ImpactedNodes))
			for i, n := range result.ImpactedNodes {
				impactedDicts[i] = graph.NodeToDict(n)
			}
			edgeDicts := make([]map[string]any, len(result.Edges))
			for i, e := range result.Edges {
				edgeDicts[i] = graph.EdgeToDict(e)
			}

			return map[string]any{
				"changed_nodes":  changedDicts,
				"impacted_nodes": impactedDicts,
				"impacted_files": result.ImpactedFiles,
				"edges":          edgeDicts,
				"truncated":      result.Truncated,
				"total_impacted": result.TotalImpacted,
			}, nil
		},
	}
}

func (r *Registry) queryGraphTool() ToolDef {
	return ToolDef{
		Name:        "query_graph",
		Description: "Run a graph query to explore code relationships. Supports: dependents_of, dependencies_of, callers_of, callees_of, file_symbols.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query_type": map[string]any{"type": "string", "enum": []string{"dependents_of", "dependencies_of", "callers_of", "callees_of", "file_symbols"}, "description": "Type of query"},
				"target":     map[string]any{"type": "string", "description": "Target node qualified name or file path"},
			},
			"required": []string{"query_type", "target"},
		},
		Handler: func(ctx context.Context, params map[string]any) (any, error) {
			queryType, _ := params["query_type"].(string)
			target, _ := params["target"].(string)

			switch queryType {
			case "file_symbols":
				nodes, err := r.store.GetNodesByFile(target)
				if err != nil {
					return nil, err
				}
				result := make([]map[string]any, len(nodes))
				for i, n := range nodes {
					result[i] = graph.NodeToDict(n)
				}
				return map[string]any{"nodes": result}, nil

			case "callers_of", "dependents_of":
				edges, err := r.store.GetEdgesByTarget(target)
				if err != nil {
					return nil, err
				}
				result := make([]map[string]any, len(edges))
				for i, e := range edges {
					result[i] = graph.EdgeToDict(e)
				}
				return map[string]any{"edges": result, "count": len(edges)}, nil

			case "callees_of", "dependencies_of":
				edges, err := r.store.GetEdgesBySource(target)
				if err != nil {
					return nil, err
				}
				result := make([]map[string]any, len(edges))
				for i, e := range edges {
					result[i] = graph.EdgeToDict(e)
				}
				return map[string]any{"edges": result, "count": len(edges)}, nil

			default:
				return nil, fmt.Errorf("unknown query type: %s", queryType)
			}
		},
	}
}

func (r *Registry) semanticSearchNodesTool() ToolDef {
	return ToolDef{
		Name:        "semantic_search_nodes",
		Description: "Search for code entities by name or keyword. Returns matching nodes with their metadata.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query"},
				"limit": map[string]any{"type": "integer", "description": "Maximum results", "default": 20},
			},
			"required": []string{"query"},
		},
		Handler: func(ctx context.Context, params map[string]any) (any, error) {
			query, _ := params["query"].(string)
			limit := intParam(params, "limit", 20)

			nodes, err := r.store.SearchNodes(query, limit)
			if err != nil {
				return nil, err
			}
			result := make([]map[string]any, len(nodes))
			for i, n := range nodes {
				result[i] = graph.NodeToDict(n)
			}
			return map[string]any{"nodes": result, "count": len(nodes)}, nil
		},
	}
}

func (r *Registry) listGraphStatsTool() ToolDef {
	return ToolDef{
		Name:        "list_graph_stats",
		Description: "Get aggregate statistics about the code knowledge graph including node/edge counts, languages, and last update time.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Handler: func(ctx context.Context, params map[string]any) (any, error) {
			stats, err := r.store.GetStats()
			if err != nil {
				return nil, err
			}
			return stats, nil
		},
	}
}

func (r *Registry) findLargeFunctionsTool() ToolDef {
	return ToolDef{
		Name:        "find_large_functions",
		Description: "Find functions, classes, or files exceeding a line-count threshold. Useful for identifying refactoring candidates.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"min_lines": map[string]any{"type": "integer", "description": "Minimum line count", "default": 50},
				"kind":      map[string]any{"type": "string", "description": "Node kind filter (Function, Class, File)"},
				"limit":     map[string]any{"type": "integer", "description": "Maximum results", "default": 50},
			},
		},
		Handler: func(ctx context.Context, params map[string]any) (any, error) {
			minLines := intParam(params, "min_lines", 50)
			kind, _ := params["kind"].(string)
			limit := intParam(params, "limit", 50)

			conditions := []string{
				"line_start IS NOT NULL",
				"line_end IS NOT NULL",
				"(line_end - line_start + 1) >= ?",
			}
			queryParams := []any{minLines}

			if kind != "" {
				conditions = append(conditions, "kind = ?")
				queryParams = append(queryParams, kind)
			}

			queryParams = append(queryParams, limit)
			where := strings.Join(conditions, " AND ")

			rows, err := r.store.DB().Query(
				fmt.Sprintf("SELECT * FROM nodes WHERE %s ORDER BY (line_end - line_start + 1) DESC LIMIT ?", where), //nolint:gosec
				queryParams...,
			)
			if err != nil {
				return nil, err
			}
			defer rows.Close()

			var nodes []map[string]any
			for rows.Next() {
				var n graph.GraphNode
				var extraStr string
				var isTest int
				var sig, cid interface{}
				if err := rows.Scan(
					&n.ID, &n.Kind, &n.Name, &n.QualifiedName, &n.FilePath,
					&n.LineStart, &n.LineEnd, &n.Language, &n.ParentName,
					&n.Params, &n.ReturnType, new(interface{}),
					&isTest, &n.FileHash, &extraStr, new(float64),
					&sig, &cid,
				); err != nil {
					continue
				}
				n.IsTest = isTest != 0
				d := graph.NodeToDict(n)
				d["line_count"] = n.LineEnd - n.LineStart + 1
				nodes = append(nodes, d)
			}
			return map[string]any{"nodes": nodes, "count": len(nodes)}, nil
		},
	}
}

func (r *Registry) getReviewContextTool() ToolDef {
	return ToolDef{
		Name:        "get_review_context",
		Description: "Generate a focused, token-efficient review context for code changes. Combines impact analysis with structural context.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"changed_files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "List of changed file paths"},
				"base":          map[string]any{"type": "string", "description": "Git diff base", "default": "HEAD~1"},
			},
		},
		Handler: func(ctx context.Context, params map[string]any) (any, error) {
			files, _ := toStringSlice(params["changed_files"])
			if len(files) == 0 {
				base, _ := params["base"].(string)
				if base == "" {
					base = "HEAD~1"
				}
				var err error
				files, err = incremental.GetChangedFiles(r.repoRoot, base)
				if err != nil || len(files) == 0 {
					files, _ = incremental.GetStagedAndUnstaged(r.repoRoot)
				}
			}

			if len(files) == 0 {
				return map[string]any{"message": "No changes detected"}, nil
			}

			impact, err := r.store.GetImpactRadius(files, 2, 200)
			if err != nil {
				return nil, err
			}

			return map[string]any{
				"changed_files":  files,
				"changed_nodes":  len(impact.ChangedNodes),
				"impacted_nodes": len(impact.ImpactedNodes),
				"impacted_files": impact.ImpactedFiles,
				"total_impacted": impact.TotalImpacted,
			}, nil
		},
	}
}

func (r *Registry) detectChangesTool() ToolDef {
	return ToolDef{
		Name:        "detect_changes",
		Description: "Detect changes and produce risk-scored, priority-ordered review guidance.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"base": map[string]any{"type": "string", "description": "Git diff base", "default": "HEAD~1"},
			},
		},
		Handler: func(ctx context.Context, params map[string]any) (any, error) {
			base, _ := params["base"].(string)
			if base == "" {
				base = "HEAD~1"
			}

			changed, _ := incremental.GetChangedFiles(r.repoRoot, base)
			if len(changed) == 0 {
				changed, _ = incremental.GetStagedAndUnstaged(r.repoRoot)
			}
			if len(changed) == 0 {
				return map[string]any{"message": "No changes detected"}, nil
			}

			impact, err := r.store.GetImpactRadius(changed, 2, 500)
			if err != nil {
				return nil, err
			}

			return map[string]any{
				"changed_files":    changed,
				"total_changed":    len(changed),
				"impacted_files":   impact.ImpactedFiles,
				"total_impacted":   impact.TotalImpacted,
				"truncated":        impact.Truncated,
				"changed_nodes":    len(impact.ChangedNodes),
				"connecting_edges": len(impact.Edges),
			}, nil
		},
	}
}

func (r *Registry) visualizeTool() ToolDef {
	return ToolDef{
		Name:        "visualize_graph",
		Description: "Generate an interactive D3.js HTML visualization of the code graph.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Handler: func(ctx context.Context, params map[string]any) (any, error) {
			dataDir := incremental.GetDataDir(r.repoRoot)
			htmlPath := dataDir + "/graph.html"

			if err := visualization.GenerateHTML(r.store, htmlPath); err != nil {
				return nil, err
			}

			return map[string]any{
				"html_path": htmlPath,
				"message":   "Visualization generated. Open in browser to explore.",
			}, nil
		},
	}
}

// --- Helpers ---

func toStringSlice(v any) ([]string, bool) {
	if v == nil {
		return nil, false
	}
	switch s := v.(type) {
	case []string:
		return s, true
	case []any:
		result := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				result = append(result, str)
			}
		}
		return result, true
	}
	return nil, false
}

func intParam(params map[string]any, key string, fallback int) int {
	v, ok := params[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return fallback
		}
		return int(i)
	}
	return fallback
}
