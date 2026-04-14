package hints

import (
	"sync"
	"time"
)

// Intent categories and their characteristic tool names.
var intentTools = map[string]map[string]struct{}{
	"reviewing":   {"detect_changes": {}, "get_review_context": {}, "get_affected_flows": {}, "get_impact_radius": {}},
	"debugging":   {"query_graph": {}, "get_flow": {}, "semantic_search_nodes": {}},
	"refactoring": {"refactor": {}, "find_dead_code": {}, "apply_refactor": {}},
	"exploring":   {"list_flows": {}, "list_graph_stats": {}, "get_minimal_context": {}},
}

// Workflow adjacency: for each tool, which tools are useful next.
var workflow = map[string][]Suggestion{
	"list_flows":       {{Tool: "get_flow", Text: "Drill into a specific flow"}, {Tool: "get_affected_flows", Text: "Check which flows are affected by changes"}},
	"get_flow":         {{Tool: "query_graph", Text: "Inspect callers/callees of a step"}, {Tool: "get_affected_flows", Text: "Check if changes affect this flow"}},
	"get_affected_flows": {{Tool: "detect_changes", Text: "Get risk-scored change analysis"}, {Tool: "get_flow", Text: "Inspect a specific affected flow"}},
	"detect_changes":   {{Tool: "get_review_context", Text: "Build a full review context"}, {Tool: "get_affected_flows", Text: "See which flows are affected"}, {Tool: "get_impact_radius", Text: "Expand blast radius analysis"}, {Tool: "refactor", Text: "Look for refactoring opportunities"}},
	"refactor":         {{Tool: "query_graph", Text: "Verify call sites before applying"}, {Tool: "detect_changes", Text: "Check risk of refactored code"}, {Tool: "semantic_search_nodes", Text: "Find related symbols"}},
	"semantic_search_nodes": {{Tool: "query_graph", Text: "Inspect callers/callees of a result"}, {Tool: "get_flow", Text: "See execution flow through a match"}, {Tool: "get_impact_radius", Text: "Check blast radius from matches"}},
	"get_impact_radius": {{Tool: "get_affected_flows", Text: "See which flows are affected"}, {Tool: "detect_changes", Text: "Get full change analysis"}},
	"query_graph":      {{Tool: "get_flow", Text: "See the execution flow"}, {Tool: "get_impact_radius", Text: "Check blast radius"}},
}

const maxPerCategory = 3

// Suggestion is a next-step hint.
type Suggestion struct {
	Tool string `json:"tool"`
	Text string `json:"suggestion"`
}

// Hints is the structured response attached to tool results.
type Hints struct {
	NextSteps []Suggestion `json:"next_steps"`
	Related   []string     `json:"related,omitempty"`
	Warnings  []string     `json:"warnings,omitempty"`
}

// Session tracks in-memory state for a single MCP connection.
type Session struct {
	mu             sync.Mutex
	toolsCalled    []string
	nodesQueried   map[string]struct{}
	filesTouched   map[string]struct{}
	InferredIntent string
	LastToolTime   time.Time
}

// NewSession creates a fresh session.
func NewSession() *Session {
	return &Session{
		nodesQueried: make(map[string]struct{}),
		filesTouched: make(map[string]struct{}),
	}
}

// RecordToolCall records a tool invocation.
func (s *Session) RecordToolCall(toolName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolsCalled = append(s.toolsCalled, toolName)
	if len(s.toolsCalled) > 100 {
		s.toolsCalled = s.toolsCalled[len(s.toolsCalled)-100:]
	}
	s.LastToolTime = time.Now()
}

// RecordFiles records touched file paths.
func (s *Session) RecordFiles(files []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range files {
		s.filesTouched[f] = struct{}{}
	}
}

// InferIntent classifies the user's likely intent from tool-call history.
func (s *Session) InferIntent() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.toolsCalled) == 0 {
		return "exploring"
	}

	start := len(s.toolsCalled) - 10
	if start < 0 {
		start = 0
	}
	recent := s.toolsCalled[start:]

	scores := make(map[string]int)
	for _, tool := range recent {
		for intent, tools := range intentTools {
			if _, ok := tools[tool]; ok {
				scores[intent]++
			}
		}
	}

	best := "exploring"
	bestScore := 0
	for intent, score := range scores {
		if score > bestScore {
			best = intent
			bestScore = score
		}
	}
	return best
}

// GenerateHints builds context-aware hints for a tool response.
func GenerateHints(toolName string, result map[string]any, session *Session) Hints {
	session.RecordToolCall(toolName)
	session.InferredIntent = session.InferIntent()

	nextSteps := buildNextSteps(toolName, session)
	warnings := extractWarnings(result)
	related := buildRelated(result, session)

	trackResult(result, session)

	if len(nextSteps) > maxPerCategory {
		nextSteps = nextSteps[:maxPerCategory]
	}
	if len(warnings) > maxPerCategory {
		warnings = warnings[:maxPerCategory]
	}
	if len(related) > maxPerCategory {
		related = related[:maxPerCategory]
	}

	return Hints{
		NextSteps: nextSteps,
		Related:   related,
		Warnings:  warnings,
	}
}

func buildNextSteps(toolName string, session *Session) []Suggestion {
	session.mu.Lock()
	called := make(map[string]struct{}, len(session.toolsCalled))
	for _, t := range session.toolsCalled {
		called[t] = struct{}{}
	}
	session.mu.Unlock()

	candidates := workflow[toolName]
	var out []Suggestion
	for _, c := range candidates {
		if _, ok := called[c.Tool]; !ok {
			out = append(out, c)
		}
	}
	return out
}

func extractWarnings(result map[string]any) []string {
	var warnings []string

	if gaps, ok := result["test_gaps"].([]any); ok && len(gaps) > 0 {
		warnings = append(warnings, "Test coverage gaps detected")
	}

	if risk, ok := result["risk_score"].(float64); ok && risk > 0.7 {
		warnings = append(warnings, "High risk score — review carefully")
	}

	return warnings
}

func buildRelated(result map[string]any, session *Session) []string {
	session.mu.Lock()
	defer session.mu.Unlock()

	var related []string
	if impacted, ok := result["impacted_files"].([]any); ok {
		for _, f := range impacted {
			if fp, ok := f.(string); ok {
				if _, touched := session.filesTouched[fp]; !touched {
					related = append(related, fp)
					if len(related) >= maxPerCategory {
						break
					}
				}
			}
		}
	}
	return related
}

func trackResult(result map[string]any, session *Session) {
	for _, key := range []string{"changed_files", "impacted_files"} {
		if files, ok := result[key].([]any); ok {
			var strs []string
			for _, f := range files {
				if s, ok := f.(string); ok {
					strs = append(strs, s)
				}
			}
			session.RecordFiles(strs)
		}
	}
}
