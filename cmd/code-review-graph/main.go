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

	"github.com/harshsharma/code-review-graph-go/internal/graph"
	"github.com/harshsharma/code-review-graph-go/internal/incremental"
	"github.com/harshsharma/code-review-graph-go/internal/mcp"
	"github.com/harshsharma/code-review-graph-go/internal/visualization"

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
		buildCmd(),
		updateCmd(),
		statusCmd(),
		watchCmd(),
		detectChangesCmd(),
		visualizeCmd(),
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

	return fmt.Sprintf(`
%s  ●──●──●%s
%s  │╲ │ ╱│%s       %scode-review-graph%s  %sv%s%s
%s  ●──%s◆%s──●%s
%s  │╱ │ ╲│%s       %sStructural knowledge graph for%s
%s  ●──●──●%s       %ssmarter code reviews%s

  %sCommands:%s
    %sbuild%s       Full graph build %s(parse all files)%s
    %supdate%s      Incremental update %s(changed files only)%s
    %swatch%s       Auto-update on file changes
    %sstatus%s      Show graph statistics
    %svisualize%s   Generate interactive HTML graph
    %sdetect-changes%s Analyze change impact %s(risk-scored review)%s
    %sserve%s       Start MCP server %s(stdio transport)%s
    %sversion%s     Show version

  %sRun%s %scode-review-graph <command> --help%s %sfor details%s
`, c, r, c, r, b, r, d, version, r, c, y, c, r, c, r, d, r, c, r, d, r,
		b, r, g, r, d, r, g, r, d, r, g, r, g, r, g, r, g, r, d, r, g, r, d, r, g, r,
		d, r, b, r, d, r)
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
