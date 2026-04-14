package skills

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Platform describes an AI coding tool's MCP configuration layout.
type Platform struct {
	Name       string
	ConfigPath func(repoRoot string) string
	Key        string // JSON key for the servers map/array
	Format     string // "object", "array", "toml"
	NeedsType  bool   // whether to include "type": "stdio"
	Detect     func() bool
}

var Platforms = map[string]Platform{
	"claude": {
		Name:       "Claude Code",
		ConfigPath: func(root string) string { return filepath.Join(root, ".mcp.json") },
		Key:        "mcpServers",
		Format:     "object",
		NeedsType:  true,
		Detect:     func() bool { return true },
	},
	"cursor": {
		Name:       "Cursor",
		ConfigPath: func(root string) string { return filepath.Join(root, ".cursor", "mcp.json") },
		Key:        "mcpServers",
		Format:     "object",
		NeedsType:  true,
		Detect: func() bool {
			h, _ := os.UserHomeDir()
			_, err := os.Stat(filepath.Join(h, ".cursor"))
			return err == nil
		},
	},
	"windsurf": {
		Name: "Windsurf",
		ConfigPath: func(root string) string {
			h, _ := os.UserHomeDir()
			return filepath.Join(h, ".codeium", "windsurf", "mcp_config.json")
		},
		Key:       "mcpServers",
		Format:    "object",
		NeedsType: false,
		Detect: func() bool {
			h, _ := os.UserHomeDir()
			_, err := os.Stat(filepath.Join(h, ".codeium", "windsurf"))
			return err == nil
		},
	},
	"zed": {
		Name: "Zed",
		ConfigPath: func(root string) string {
			h, _ := os.UserHomeDir()
			if runtime.GOOS == "darwin" {
				return filepath.Join(h, "Library", "Application Support", "Zed", "settings.json")
			}
			return filepath.Join(h, ".config", "zed", "settings.json")
		},
		Key:       "context_servers",
		Format:    "object",
		NeedsType: false,
		Detect: func() bool {
			h, _ := os.UserHomeDir()
			var p string
			if runtime.GOOS == "darwin" {
				p = filepath.Join(h, "Library", "Application Support", "Zed")
			} else {
				p = filepath.Join(h, ".config", "zed")
			}
			_, err := os.Stat(p)
			return err == nil
		},
	},
	"continue": {
		Name: "Continue",
		ConfigPath: func(root string) string {
			h, _ := os.UserHomeDir()
			return filepath.Join(h, ".continue", "config.json")
		},
		Key:       "mcpServers",
		Format:    "array",
		NeedsType: true,
		Detect: func() bool {
			h, _ := os.UserHomeDir()
			_, err := os.Stat(filepath.Join(h, ".continue"))
			return err == nil
		},
	},
	"opencode": {
		Name:       "OpenCode",
		ConfigPath: func(root string) string { return filepath.Join(root, ".opencode.json") },
		Key:        "mcpServers",
		Format:     "object",
		NeedsType:  true,
		Detect:     func() bool { return true },
	},
}

func buildServerEntry(plat Platform) map[string]any {
	entry := map[string]any{
		"command": "code-review-graph",
		"args":    []string{"serve"},
	}
	if plat.NeedsType {
		entry["type"] = "stdio"
	}
	return entry
}

// InstallPlatformConfigs installs MCP server config for detected platforms.
func InstallPlatformConfigs(repoRoot, target string, dryRun bool) []string {
	var platforms map[string]Platform
	if target == "all" {
		platforms = make(map[string]Platform)
		for k, p := range Platforms {
			if p.Detect() {
				platforms[k] = p
			}
		}
	} else {
		p, ok := Platforms[target]
		if !ok {
			fmt.Printf("Unknown platform: %s\n", target)
			return nil
		}
		platforms = map[string]Platform{target: p}
	}

	var configured []string
	for _, plat := range platforms {
		configPath := plat.ConfigPath(repoRoot)
		serverEntry := buildServerEntry(plat)

		existing := make(map[string]any)
		if data, err := os.ReadFile(configPath); err == nil {
			json.Unmarshal(data, &existing) //nolint:errcheck
		}

		if plat.Format == "array" {
			arr, _ := existing[plat.Key].([]any)
			for _, s := range arr {
				if m, ok := s.(map[string]any); ok && m["name"] == "code-review-graph" {
					fmt.Printf("  %s: already configured in %s\n", plat.Name, configPath)
					configured = append(configured, plat.Name)
					goto next
				}
			}
			arrEntry := map[string]any{"name": "code-review-graph"}
			for k, v := range serverEntry {
				arrEntry[k] = v
			}
			arr = append(arr, arrEntry)
			existing[plat.Key] = arr
		} else {
			servers, _ := existing[plat.Key].(map[string]any)
			if servers == nil {
				servers = make(map[string]any)
			}
			if _, ok := servers["code-review-graph"]; ok {
				fmt.Printf("  %s: already configured in %s\n", plat.Name, configPath)
				configured = append(configured, plat.Name)
				continue
			}
			servers["code-review-graph"] = serverEntry
			existing[plat.Key] = servers
		}

		if dryRun {
			fmt.Printf("  [dry-run] %s: would write %s\n", plat.Name, configPath)
		} else {
			os.MkdirAll(filepath.Dir(configPath), 0o755) //nolint:errcheck
			data, _ := json.MarshalIndent(existing, "", "  ")
			if err := os.WriteFile(configPath, append(data, '\n'), 0o644); err != nil {
				slog.Warn("failed to write platform config", "platform", plat.Name, "err", err)
				continue
			}
			fmt.Printf("  %s: configured %s\n", plat.Name, configPath)
		}
		configured = append(configured, plat.Name)
	next:
	}
	return configured
}

// Skill markdown files for Claude Code / Cursor.
var skillFiles = map[string]struct {
	Name        string
	Description string
	Body        string
}{
	"explore-codebase.md": {
		Name:        "Explore Codebase",
		Description: "Navigate and understand codebase structure using the knowledge graph",
		Body: `## Explore Codebase

Use the code-review-graph MCP tools to explore and understand the codebase.

### Steps
1. Run ` + "`list_graph_stats`" + ` to see overall codebase metrics.
2. Use ` + "`semantic_search_nodes`" + ` to find specific functions or classes.
3. Use ` + "`query_graph`" + ` with callers_of, callees_of to trace relationships.
4. Use ` + "`list_flows`" + ` and ` + "`get_flow`" + ` to understand execution paths.
5. Use ` + "`find_large_functions`" + ` to identify complex code.

### Token Efficiency Rules
- ALWAYS start with ` + "`get_minimal_context`" + ` before any other graph tool.
- Target: complete any task in ≤5 tool calls and ≤800 total output tokens.`,
	},
	"review-changes.md": {
		Name:        "Review Changes",
		Description: "Perform a structured code review using change detection and impact",
		Body: `## Review Changes

Perform a thorough, risk-aware code review using the knowledge graph.

### Steps
1. Run ` + "`detect_changes`" + ` to get risk-scored change analysis.
2. Run ` + "`get_affected_flows`" + ` to find impacted execution paths.
3. For each high-risk function, check test coverage.
4. Run ` + "`get_impact_radius`" + ` to understand the blast radius.
5. For any untested changes, suggest specific test cases.

### Output Format
Provide findings grouped by risk level (high/medium/low).`,
	},
	"debug-issue.md": {
		Name:        "Debug Issue",
		Description: "Systematically debug issues using graph-powered code navigation",
		Body: `## Debug Issue

Use the knowledge graph to systematically trace and debug issues.

### Steps
1. Use ` + "`semantic_search_nodes`" + ` to find code related to the issue.
2. Use ` + "`query_graph`" + ` with callers_of and callees_of to trace call chains.
3. Use ` + "`get_flow`" + ` to see full execution paths through suspected areas.
4. Run ` + "`detect_changes`" + ` to check if recent changes caused the issue.
5. Use ` + "`get_impact_radius`" + ` on suspected files to see what else is affected.`,
	},
	"refactor-safely.md": {
		Name:        "Refactor Safely",
		Description: "Plan and execute safe refactoring using dependency analysis",
		Body: `## Refactor Safely

Use the knowledge graph to plan and execute refactoring with confidence.

### Steps
1. Use ` + "`refactor`" + ` with operation="suggest" for suggestions.
2. Use ` + "`find_dead_code`" + ` to find unreferenced code.
3. For renames, use ` + "`refactor`" + ` with operation="rename" to preview edits.
4. Use ` + "`apply_refactor`" + ` with the refactor_id to apply.
5. After changes, run ` + "`detect_changes`" + ` to verify impact.

### Safety Checks
- Always preview before applying.
- Check ` + "`get_impact_radius`" + ` before major refactors.
- Use ` + "`get_affected_flows`" + ` to ensure no critical paths are broken.`,
	},
}

// GenerateSkills writes skill markdown files to the skills directory.
func GenerateSkills(repoRoot string) (string, error) {
	skillsDir := filepath.Join(repoRoot, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return "", fmt.Errorf("creating skills dir: %w", err)
	}

	for filename, skill := range skillFiles {
		content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", skill.Name, skill.Description, skill.Body)
		path := filepath.Join(skillsDir, filename)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return "", fmt.Errorf("writing skill %s: %w", filename, err)
		}
		slog.Info("wrote skill", "path", path)
	}
	return skillsDir, nil
}

// HooksConfig returns Claude Code hooks configuration.
func HooksConfig() map[string]any {
	return map[string]any{
		"hooks": map[string]any{
			"PostToolUse": []map[string]any{{
				"matcher": "Edit|Write|Bash",
				"hooks": []map[string]any{{
					"type":    "command",
					"command": "code-review-graph update",
					"timeout": 30,
				}},
			}},
			"SessionStart": []map[string]any{{
				"matcher": "",
				"hooks": []map[string]any{{
					"type":    "command",
					"command": "code-review-graph status",
					"timeout": 10,
				}},
			}},
		},
	}
}

// InstallHooks writes hooks config to .claude/settings.json.
func InstallHooks(repoRoot string) error {
	settingsDir := filepath.Join(repoRoot, ".claude")
	os.MkdirAll(settingsDir, 0o755) //nolint:errcheck
	settingsPath := filepath.Join(settingsDir, "settings.json")

	existing := make(map[string]any)
	if data, err := os.ReadFile(settingsPath); err == nil {
		json.Unmarshal(data, &existing) //nolint:errcheck
	}

	hooks := HooksConfig()
	for k, v := range hooks {
		existing[k] = v
	}

	data, _ := json.MarshalIndent(existing, "", "  ")
	return os.WriteFile(settingsPath, append(data, '\n'), 0o644)
}

// InstallGitHook installs a pre-commit hook that prints a risk summary.
func InstallGitHook(repoRoot string) (string, error) {
	gitDir := filepath.Join(repoRoot, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return "", fmt.Errorf("no .git directory found at %s", repoRoot)
	}

	hookPath := filepath.Join(gitDir, "hooks", "pre-commit")
	os.MkdirAll(filepath.Dir(hookPath), 0o755) //nolint:errcheck

	marker := "code-review-graph detect-changes"
	script := `#!/bin/sh
# Installed by code-review-graph.
if command -v code-review-graph >/dev/null 2>&1; then
    code-review-graph detect-changes --brief || true
fi
`
	if data, err := os.ReadFile(hookPath); err == nil {
		if strings.Contains(string(data), marker) {
			return hookPath, nil
		}
		script = strings.TrimRight(string(data), "\n") + "\n" + script
	}

	if err := os.WriteFile(hookPath, []byte(script), 0o755); err != nil {
		return "", fmt.Errorf("writing hook: %w", err)
	}
	return hookPath, nil
}

const claudeMDMarker = "<!-- code-review-graph MCP tools -->"

const claudeMDSection = claudeMDMarker + `
## MCP Tools: code-review-graph

**IMPORTANT: This project has a knowledge graph. ALWAYS use the
code-review-graph MCP tools BEFORE using Grep/Glob/Read to explore
the codebase.** The graph is faster, cheaper (fewer tokens), and gives
you structural context (callers, dependents, test coverage) that file
scanning cannot.

### Key Tools

| Tool | Use when |
|------|----------|
| ` + "`detect_changes`" + ` | Reviewing code changes — gives risk-scored analysis |
| ` + "`get_impact_radius`" + ` | Understanding blast radius of a change |
| ` + "`get_affected_flows`" + ` | Finding which execution paths are impacted |
| ` + "`query_graph`" + ` | Tracing callers, callees, imports, tests |
| ` + "`semantic_search_nodes`" + ` | Finding functions/classes by name or keyword |
| ` + "`refactor`" + ` | Planning renames, finding dead code |
`

// InjectClaudeMD appends the MCP tools section to CLAUDE.md if not already present.
func InjectClaudeMD(repoRoot string) (bool, error) {
	path := filepath.Join(repoRoot, "CLAUDE.md")
	existing := ""
	if data, err := os.ReadFile(path); err == nil {
		existing = string(data)
	}
	if strings.Contains(existing, claudeMDMarker) {
		return false, nil
	}
	sep := ""
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		sep = "\n"
	}
	extra := ""
	if existing != "" {
		extra = "\n"
	}
	content := existing + sep + extra + claudeMDSection
	return true, os.WriteFile(path, []byte(content), 0o644)
}

// InjectPlatformInstructions writes instruction sections to platform rule files.
func InjectPlatformInstructions(repoRoot, target string) []string {
	files := map[string][]string{
		"AGENTS.md":      {"cursor", "opencode"},
		".cursorrules":   {"cursor"},
		".windsurfrules": {"windsurf"},
	}
	var updated []string
	for filename, owners := range files {
		if target != "all" {
			found := false
			for _, o := range owners {
				if o == target {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		path := filepath.Join(repoRoot, filename)
		existing := ""
		if data, err := os.ReadFile(path); err == nil {
			existing = string(data)
		}
		if strings.Contains(existing, claudeMDMarker) {
			continue
		}
		sep := ""
		if existing != "" && !strings.HasSuffix(existing, "\n") {
			sep = "\n"
		}
		extra := ""
		if existing != "" {
			extra = "\n"
		}
		content := existing + sep + extra + claudeMDSection
		if err := os.WriteFile(path, []byte(content), 0o644); err == nil {
			updated = append(updated, filename)
		}
	}
	return updated
}

// FullInstall runs the complete installation: MCP config, skills, hooks, git hook, CLAUDE.md.
func FullInstall(repoRoot, platform string) error {
	fmt.Println("Installing code-review-graph...")

	// 1. MCP platform configs
	fmt.Println("\nMCP server configuration:")
	configured := InstallPlatformConfigs(repoRoot, platform, false)
	if len(configured) == 0 {
		fmt.Println("  No platforms detected.")
	}

	// 2. Skills
	fmt.Println("\nSkill files:")
	skillsDir, err := GenerateSkills(repoRoot)
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
	} else {
		fmt.Printf("  Written to %s\n", skillsDir)
	}

	// 3. Hooks
	fmt.Println("\nHooks configuration:")
	if err := InstallHooks(repoRoot); err != nil {
		fmt.Printf("  Error: %v\n", err)
	} else {
		fmt.Println("  Written to .claude/settings.json")
	}

	// 4. Git pre-commit hook
	fmt.Println("\nGit pre-commit hook:")
	hookPath, err := InstallGitHook(repoRoot)
	if err != nil {
		fmt.Printf("  Skipped: %v\n", err)
	} else {
		fmt.Printf("  Written to %s\n", hookPath)
	}

	// 5. CLAUDE.md
	fmt.Println("\nInstruction files:")
	if modified, err := InjectClaudeMD(repoRoot); err != nil {
		fmt.Printf("  Error: %v\n", err)
	} else if modified {
		fmt.Println("  Appended MCP section to CLAUDE.md")
	} else {
		fmt.Println("  CLAUDE.md already contains instructions")
	}

	updated := InjectPlatformInstructions(repoRoot, platform)
	for _, f := range updated {
		fmt.Printf("  Wrote %s\n", f)
	}

	// 6. Initial build
	fmt.Println("\nBuilding initial graph...")
	cmd := exec.Command("code-review-graph", "build", "--repo", repoRoot)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("  Build skipped (run manually): %v\n", err)
	}

	fmt.Println("\nDone! The graph auto-updates via hooks on each edit.")
	return nil
}
