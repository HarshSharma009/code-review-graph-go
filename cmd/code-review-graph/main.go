package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/harshsharma/code-review-graph-go/internal/flows"
	"github.com/harshsharma/code-review-graph-go/internal/graph"
	"github.com/harshsharma/code-review-graph-go/internal/skills"
	"github.com/harshsharma/code-review-graph-go/internal/incremental"
	"github.com/harshsharma/code-review-graph-go/internal/mcp"
	"github.com/harshsharma/code-review-graph-go/internal/registry"
	"github.com/harshsharma/code-review-graph-go/internal/visualization"
	"github.com/harshsharma/code-review-graph-go/internal/wiki"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	root := &cobra.Command{
		Use:   "code-review-graph",
		Short: "Persistent incremental knowledge graph for code reviews",
		Long:  banner(),
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Print(banner())
		},
	}

	root.AddCommand(
		versionCmd(),
		installCmd(),
		initCmd(),
		buildCmd(),
		updateCmd(),
		postprocessCmd(),
		statusCmd(),
		watchCmd(),
		detectChangesCmd(),
		visualizeCmd(),
		wikiCmd(),
		registerCmd(),
		unregisterCmd(),
		reposCmd(),
		serveCmd(),
	)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("code-review-graph %s\n", version)
		},
	}
}

func installCmd() *cobra.Command {
	var platform string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Full installation: MCP config, skills, hooks, git hook, CLAUDE.md",
		Long:  "Install code-review-graph for all detected AI coding platforms. Sets up MCP server configs, skill files, hooks, git pre-commit hook, and CLAUDE.md integration.",
		RunE: func(cmd *cobra.Command, args []string) error {
			repoRoot := incremental.FindProjectRoot("")
			return skills.FullInstall(repoRoot, platform)
		},
	}
	cmd.Flags().StringVar(&platform, "platform", "all", "Target platform (claude, cursor, windsurf, zed, continue, opencode, all)")
	return cmd
}

func initCmd() *cobra.Command {
	var platform string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize MCP server configuration for detected platforms",
		RunE: func(cmd *cobra.Command, args []string) error {
			repoRoot := incremental.FindProjectRoot("")
			fmt.Println("Configuring MCP server...")
			configured := skills.InstallPlatformConfigs(repoRoot, platform, dryRun)
			if len(configured) == 0 {
				fmt.Println("No platforms detected.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&platform, "platform", "all", "Target platform (claude, cursor, windsurf, zed, continue, opencode, all)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be done without writing")
	return cmd
}

func buildCmd() *cobra.Command {
	var repoPath string
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Full graph build (re-parse all files)",
		RunE: func(cmd *cobra.Command, args []string) error {
			setupLogging()
			repoRoot := incremental.FindProjectRoot(repoPath)
			dbPath := incremental.GetDBPath(repoRoot)

			store, err := graph.NewStore(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer store.Close()

			ctx := context.Background()
			result, err := incremental.FullBuild(ctx, repoRoot, store)
			if err != nil {
				return fmt.Errorf("build failed: %w", err)
			}

			fmt.Printf("Full build: %d files, %d nodes, %d edges\n",
				result.FilesParsed, result.TotalNodes, result.TotalEdges)
			if len(result.Errors) > 0 {
				fmt.Printf("Errors: %d\n", len(result.Errors))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (auto-detected)")
	return cmd
}

func updateCmd() *cobra.Command {
	var repoPath, base string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Incremental update (only changed files)",
		RunE: func(cmd *cobra.Command, args []string) error {
			setupLogging()
			repoRoot := incremental.FindProjectRoot(repoPath)
			if incremental.FindRepoRoot(repoRoot) == "" {
				return fmt.Errorf("not in a git repository; use 'build' for a full parse")
			}

			dbPath := incremental.GetDBPath(repoRoot)
			store, err := graph.NewStore(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer store.Close()

			ctx := context.Background()
			result, err := incremental.IncrementalUpdate(ctx, repoRoot, store, base, nil)
			if err != nil {
				return fmt.Errorf("update failed: %w", err)
			}

			fmt.Printf("Incremental: %d files updated, %d nodes, %d edges\n",
				result.FilesUpdated, result.TotalNodes, result.TotalEdges)
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (auto-detected)")
	cmd.Flags().StringVar(&base, "base", "HEAD~1", "Git diff base")
	return cmd
}

func statusCmd() *cobra.Command {
	var repoPath string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show graph statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			setupLogging()
			repoRoot := incremental.FindProjectRoot(repoPath)
			dbPath := incremental.GetDBPath(repoRoot)

			store, err := graph.NewStore(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer store.Close()

			stats, err := store.GetStats()
			if err != nil {
				return fmt.Errorf("getting stats: %w", err)
			}

			fmt.Printf("Nodes: %d\n", stats.TotalNodes)
			fmt.Printf("Edges: %d\n", stats.TotalEdges)
			fmt.Printf("Files: %d\n", stats.FilesCount)
			fmt.Printf("Languages: %s\n", strings.Join(stats.Languages, ", "))
			fmt.Printf("Last updated: %s\n", orDefault(stats.LastUpdated, "never"))

			branch, _ := store.GetMetadata("git_branch")
			sha, _ := store.GetMetadata("git_head_sha")
			if branch != "" {
				fmt.Printf("Built on branch: %s\n", branch)
			}
			if sha != "" && len(sha) >= 12 {
				fmt.Printf("Built at commit: %s\n", sha[:12])
			}

			currentBranch, _ := incremental.GitBranchInfo(repoRoot)
			if branch != "" && currentBranch != "" && branch != currentBranch {
				fmt.Printf("WARNING: Graph was built on '%s' but you are now on '%s'. Run 'code-review-graph build' to rebuild.\n",
					branch, currentBranch)
			}

			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (auto-detected)")
	return cmd
}

func watchCmd() *cobra.Command {
	var repoPath string
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Watch for changes and auto-update",
		RunE: func(cmd *cobra.Command, args []string) error {
			setupLogging()
			repoRoot := incremental.FindProjectRoot(repoPath)
			dbPath := incremental.GetDBPath(repoRoot)

			store, err := graph.NewStore(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer store.Close()

			return incremental.Watch(context.Background(), repoRoot, store)
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (auto-detected)")
	return cmd
}

func detectChangesCmd() *cobra.Command {
	var repoPath, base string
	var brief bool
	cmd := &cobra.Command{
		Use:   "detect-changes",
		Short: "Analyze change impact",
		RunE: func(cmd *cobra.Command, args []string) error {
			setupLogging()
			repoRoot := incremental.FindProjectRoot(repoPath)
			if incremental.FindRepoRoot(repoRoot) == "" {
				return fmt.Errorf("not in a git repository")
			}

			dbPath := incremental.GetDBPath(repoRoot)
			store, err := graph.NewStore(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer store.Close()

			changed, _ := incremental.GetChangedFiles(repoRoot, base)
			if len(changed) == 0 {
				changed, _ = incremental.GetStagedAndUnstaged(repoRoot)
			}
			if len(changed) == 0 {
				fmt.Println("No changes detected.")
				return nil
			}

			result, err := store.GetImpactRadius(changed, 2, 500)
			if err != nil {
				return fmt.Errorf("computing impact: %w", err)
			}

			if brief {
				fmt.Printf("Changed files: %d, Impacted nodes: %d, Impacted files: %d\n",
					len(changed), result.TotalImpacted, len(result.ImpactedFiles))
			} else {
				out, _ := json.MarshalIndent(map[string]any{
					"changed_files":    changed,
					"impacted_files":   result.ImpactedFiles,
					"total_impacted":   result.TotalImpacted,
					"truncated":        result.Truncated,
					"changed_nodes":    len(result.ChangedNodes),
					"impacted_nodes":   len(result.ImpactedNodes),
					"connecting_edges": len(result.Edges),
				}, "", "  ")
				fmt.Println(string(out))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (auto-detected)")
	cmd.Flags().StringVar(&base, "base", "HEAD~1", "Git diff base")
	cmd.Flags().BoolVar(&brief, "brief", false, "Show brief summary only")
	return cmd
}

func visualizeCmd() *cobra.Command {
	var repoPath string
	var serve bool
	cmd := &cobra.Command{
		Use:   "visualize",
		Short: "Generate interactive HTML graph visualization",
		RunE: func(cmd *cobra.Command, args []string) error {
			setupLogging()
			repoRoot := incremental.FindProjectRoot(repoPath)
			dbPath := incremental.GetDBPath(repoRoot)

			store, err := graph.NewStore(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer store.Close()

			dataDir := incremental.GetDataDir(repoRoot)
			htmlPath := filepath.Join(dataDir, "graph.html")

			if err := visualization.GenerateHTML(store, htmlPath); err != nil {
				return fmt.Errorf("generating visualization: %w", err)
			}

			fmt.Printf("Visualization: %s\n", htmlPath)

			if serve {
				fmt.Printf("Serving at http://localhost:8765/graph.html\nPress Ctrl+C to stop.\n")
				http.Handle("/", http.FileServer(http.Dir(dataDir)))
				return http.ListenAndServe(":8765", nil)
			}

			fmt.Println("Open in browser to explore your codebase graph.")
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (auto-detected)")
	cmd.Flags().BoolVar(&serve, "serve", false, "Start a local HTTP server (localhost:8765)")
	return cmd
}

func postprocessCmd() *cobra.Command {
	var repoPath string
	cmd := &cobra.Command{
		Use:   "postprocess",
		Short: "Run post-processing (trace flows, compute criticality)",
		RunE: func(cmd *cobra.Command, args []string) error {
			setupLogging()
			repoRoot := incremental.FindProjectRoot(repoPath)
			dbPath := incremental.GetDBPath(repoRoot)

			store, err := graph.NewStore(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer store.Close()

			allFlows, err := flows.TraceFlows(store, 15)
			if err != nil {
				return fmt.Errorf("tracing flows: %w", err)
			}

			count, err := flows.StoreFlows(store, allFlows)
			if err != nil {
				return fmt.Errorf("storing flows: %w", err)
			}

			fmt.Printf("Postprocess: %d execution flows traced and stored\n", count)
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (auto-detected)")
	return cmd
}

func wikiCmd() *cobra.Command {
	var repoPath string
	cmd := &cobra.Command{
		Use:   "wiki",
		Short: "Generate markdown wiki from community structure",
		RunE: func(cmd *cobra.Command, args []string) error {
			setupLogging()
			repoRoot := incremental.FindProjectRoot(repoPath)
			dbPath := incremental.GetDBPath(repoRoot)

			store, err := graph.NewStore(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer store.Close()

			wikiDir := filepath.Join(incremental.GetDataDir(repoRoot), "wiki")
			result, err := wiki.GenerateWiki(store, wikiDir)
			if err != nil {
				return fmt.Errorf("generating wiki: %w", err)
			}

			fmt.Printf("Wiki: %d generated, %d updated, %d unchanged\n",
				result.PagesGenerated, result.PagesUpdated, result.PagesUnchanged)
			fmt.Printf("Output: %s\n", wikiDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (auto-detected)")
	return cmd
}

func registerCmd() *cobra.Command {
	var alias string
	cmd := &cobra.Command{
		Use:   "register <path>",
		Short: "Register a repository in the multi-repo registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg := registry.New("")
			entry, err := reg.Register(args[0], alias)
			if err != nil {
				return err
			}
			fmt.Printf("Registered: %s", entry.Path)
			if entry.Alias != "" {
				fmt.Printf(" (alias: %s)", entry.Alias)
			}
			fmt.Println()
			return nil
		},
	}
	cmd.Flags().StringVar(&alias, "alias", "", "Short alias for the repository")
	return cmd
}

func unregisterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unregister <path-or-alias>",
		Short: "Remove a repository from the multi-repo registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg := registry.New("")
			if reg.Unregister(args[0]) {
				fmt.Println("Unregistered.")
			} else {
				fmt.Println("Not found in registry.")
			}
			return nil
		},
	}
}

func reposCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "repos",
		Short: "List all registered repositories",
		Run: func(cmd *cobra.Command, args []string) {
			reg := registry.New("")
			repos := reg.ListRepos()
			if len(repos) == 0 {
				fmt.Println("No repositories registered.")
				return
			}
			for _, r := range repos {
				if r.Alias != "" {
					fmt.Printf("  %s (%s)\n", r.Path, r.Alias)
				} else {
					fmt.Printf("  %s\n", r.Path)
				}
			}
		},
	}
}

func serveCmd() *cobra.Command {
	var repoPath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server (stdio transport)",
		Long:  "Start the Model Context Protocol server for AI coding tool integration. Communicates over stdin/stdout using JSON-RPC 2.0.",
		RunE: func(cmd *cobra.Command, args []string) error {
			setupLogging()
			repoRoot := incremental.FindProjectRoot(repoPath)
			dbPath := incremental.GetDBPath(repoRoot)

			store, err := graph.NewStore(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer store.Close()

			srv := mcp.NewServer(store, repoRoot)
			return srv.Run(context.Background())
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (auto-detected)")
	return cmd
}

func setupLogging() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func banner() string {
	supportsColor := os.Getenv("NO_COLOR") == "" && isTerminal()

	c, y, b, d, g, r := "", "", "", "", "", ""
	if supportsColor {
		c = "\033[36m"
		y = "\033[33m"
		b = "\033[1m"
		d = "\033[2m"
		g = "\033[32m"
		r = "\033[0m"
	}

	cmd := func(name, desc string) string { return g + name + r + " " + d + desc + r }

	return fmt.Sprintf(`
%s  ●──●──●%s
%s  │╲ │ ╱│%s       %scode-review-graph%s  %sv%s%s
%s  ●──%s◆%s──●%s
%s  │╱ │ ╲│%s       %sStructural knowledge graph for%s
%s  ●──●──●%s       %ssmarter code reviews%s

  %sCommands:%s
    %s
    %s
    %s
    %s
    %s
    %s
    %s
    %s
    %s
    %s
    %s
    %s
    %s
    %s
    %s

  %sRun%s %scode-review-graph <command> --help%s %sfor details%s
`, c, r, c, r, b, r, d, version, r, c, y, c, r, c, r, d, r, c, r, d, r,
		b, r,
		cmd("install      ", "(MCP, skills, hooks, CLAUDE.md)"),
		cmd("init         ", "(platform auto-detect)"),
		cmd("build        ", "(parse all files)"),
		cmd("update       ", "(changed files only)"),
		cmd("postprocess  ", "(trace execution flows)"),
		cmd("watch        ", "(auto-update on changes)"),
		cmd("status       ", "(graph statistics)"),
		cmd("visualize    ", "(interactive HTML graph)"),
		cmd("detect-changes", "(risk-scored review)"),
		cmd("wiki         ", "(markdown from communities)"),
		cmd("register     ", "(add repo to registry)"),
		cmd("unregister   ", "(remove repo from registry)"),
		cmd("repos        ", "(list registered repos)"),
		cmd("serve        ", "(MCP stdio transport)"),
		cmd("version      ", ""),
		d, r, b, r, d, r)
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
