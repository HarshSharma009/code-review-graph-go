package prompts

import "fmt"

const tokenEfficiencyPreamble = `## Rules for Token-Efficient Graph Usage
1. ALWAYS call get_minimal_context first with a task description.
2. Use targeted queries (query_graph with a specific symbol) over broad scans.
3. Never request more than 3 tool calls per turn unless absolutely necessary.
4. When reviewing changes: detect_changes → only expand on high-risk items.
`

// Prompt represents an MCP prompt template.
type Prompt struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Arguments   []PromptArgument  `json:"arguments,omitempty"`
	Handler     func(args map[string]string) []PromptMessage
}

// PromptArgument describes a prompt parameter.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptMessage is a role+content pair returned by a prompt.
type PromptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AllPrompts returns the 5 standard MCP prompt templates.
func AllPrompts() []Prompt {
	return []Prompt{
		{
			Name:        "review_changes",
			Description: "Pre-commit review workflow using detect_changes, affected_flows, and test gaps.",
			Arguments:   []PromptArgument{{Name: "base", Description: "Git ref to diff against (default: HEAD~1)"}},
			Handler: func(args map[string]string) []PromptMessage {
				base := args["base"]
				if base == "" {
					base = "HEAD~1"
				}
				return []PromptMessage{{
					Role: "user",
					Content: tokenEfficiencyPreamble + fmt.Sprintf(`## Review Workflow
1. Call get_minimal_context(task="review changes against %s") to get risk overview.
2. Call detect_changes for risk-scored analysis.
3. For each high-risk function, check test coverage with query_graph.
4. Call get_affected_flows only if >3 changed functions.
5. Summarize: risk level, what changed, test gaps, improvements needed.`, base),
				}}
			},
		},
		{
			Name:        "architecture_map",
			Description: "Architecture documentation using communities, flows, and diagrams.",
			Handler: func(args map[string]string) []PromptMessage {
				return []PromptMessage{{
					Role: "user",
					Content: tokenEfficiencyPreamble + `## Architecture Mapping Workflow
1. Call get_minimal_context(task="map architecture").
2. Call list_graph_stats for technology overview.
3. Call list_flows for critical flow names + criticality scores.
4. Produce a concise diagram showing major modules and key flows.`,
				}}
			},
		},
		{
			Name:        "debug_issue",
			Description: "Guided debugging using search, flow tracing, and recent changes.",
			Arguments:   []PromptArgument{{Name: "description", Description: "Description of the issue to debug"}},
			Handler: func(args map[string]string) []PromptMessage {
				desc := args["description"]
				if desc == "" {
					desc = "<description>"
				}
				return []PromptMessage{{
					Role: "user",
					Content: tokenEfficiencyPreamble + fmt.Sprintf(`## Debug Workflow
1. Call get_minimal_context(task="debug: %s").
2. Call semantic_search_nodes(query=<keywords from description>, limit=5).
3. For top results, call query_graph(query_type="callers_of", target=<name>).
4. If the issue involves execution flow: call get_flow for the relevant flow.
5. Only call get_impact_radius if you need to trace the blast radius.`, desc),
				}}
			},
		},
		{
			Name:        "onboard_developer",
			Description: "New developer orientation using stats, architecture, and critical flows.",
			Handler: func(args map[string]string) []PromptMessage {
				return []PromptMessage{{
					Role: "user",
					Content: tokenEfficiencyPreamble + `## Onboarding Workflow
1. Call get_minimal_context(task="onboard developer").
2. Call list_graph_stats for technology overview.
3. Call list_flows — highlight the top 3 critical flows.
4. Present a table of major modules + sizes.
5. Only drill into a specific area if the developer asks.`,
				}}
			},
		},
		{
			Name:        "pre_merge_check",
			Description: "PR readiness check with risk scoring, test gaps, and dead code detection.",
			Arguments:   []PromptArgument{{Name: "base", Description: "Git ref to diff against (default: HEAD~1)"}},
			Handler: func(args map[string]string) []PromptMessage {
				base := args["base"]
				if base == "" {
					base = "HEAD~1"
				}
				return []PromptMessage{{
					Role: "user",
					Content: tokenEfficiencyPreamble + fmt.Sprintf(`## Pre-Merge Check Workflow
1. Call get_minimal_context(task="pre-merge check against %s").
2. Call detect_changes for risk score and test gaps.
3. If risk > 0.4: call get_affected_flows.
4. Call find_dead_code to check for newly dead code.
5. Output: GO/NO-GO recommendation with justification + required follow-ups.`, base),
				}}
			},
		},
	}
}
