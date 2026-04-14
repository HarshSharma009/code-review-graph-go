# code-review-graph-go

A persistent, incrementally-updated structural knowledge graph for token-efficient code reviews — written in Go with goroutine-based concurrency.

Go port of [code-review-graph](https://github.com/tirth8205/code-review-graph) (Python), redesigned around goroutines, channels, and SQLite WAL mode for maximum throughput.

## Why

LLM-powered code reviews burn tokens re-discovering the same structural context on every call. This tool builds a persistent graph of your codebase — functions, classes, imports, call chains — so AI tools can query relationships in ~100 tokens instead of re-reading thousands of lines.

## Features

### Core Engine
- **Tree-sitter AST parsing** across 17 languages (Python, JavaScript, TypeScript, TSX, Go, Rust, Java, C, C++, C#, Ruby, Kotlin, Swift, PHP, Scala, Lua, Bash)
- **Goroutine-parallel parsing** — WorkerPool with `runtime.NumCPU()` workers, channel-based job distribution
- **SQLite graph store** with WAL mode for concurrent reads, mutex-serialised writes, and schema migrations (v1→v6)
- **Incremental updates** — git diff + SHA-256 hash-based skip logic + dependent file expansion
- **BFS blast-radius** — impact analysis via SQLite recursive CTE
- **File watching** — fsnotify with 300ms debounce for instant re-parse on save

### Search
- **Hybrid search engine** combining FTS5 BM25 full-text search, vector embedding similarity, and keyword LIKE fallback
- **Reciprocal Rank Fusion (RRF)** merge across search backends
- **Query-kind boosting** — PascalCase queries boost Class/Type, snake_case boosts Function, dotted paths boost qualified names
- **Context-file boosting** — results in active files score 1.5x higher

### Analysis
- **Execution flow tracing** — entry-point detection via framework decorators and name patterns, BFS traversal, criticality scoring
- **Refactoring engine** — graph-powered rename preview, dead code detection, community-driven move suggestions, safe apply with path-traversal checks
- **Wiki generation** — auto-generated markdown pages from community structure with member tables, flow summaries, and cross-community dependencies

### MCP Integration
- **19 MCP tools** over JSON-RPC 2.0 stdio transport for seamless AI coding tool integration
- **5 prompt templates** — review_changes, architecture_map, debug_issue, onboard_developer, pre_merge_check
- **Context-aware hints** — session state tracking, intent inference, next-step suggestions appended to tool responses
- **Multi-platform installer** — auto-configures Claude Code, Cursor, Windsurf, Zed, Continue, and OpenCode

### Multi-Repo
- **Registry** — manage multiple codebases from a single JSON registry with cross-repo search
- **Vector embeddings** — provider interface with SQLite blob storage and cosine similarity search

## Quick Start

```bash
# Build (CGo required for Tree-sitter and SQLite FTS5)
CGO_ENABLED=1 CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" go build -o bin/code-review-graph ./cmd/code-review-graph

# Full setup for AI coding tools (MCP config, skills, hooks, CLAUDE.md)
./bin/code-review-graph install

# Or just build the graph
./bin/code-review-graph build --repo /path/to/your/project

# Check stats
./bin/code-review-graph status

# Incremental update (only changed files)
./bin/code-review-graph update

# Watch for changes
./bin/code-review-graph watch

# Start MCP server for AI tool integration
./bin/code-review-graph serve
```

## Commands

| Command | Description |
|---------|-------------|
| `install` | Full setup — MCP config, skill files, hooks, git hook, CLAUDE.md |
| `init` | MCP server config only (platform auto-detect) |
| `build` | Full parallel graph build |
| `update` | Incremental update (changed files only) |
| `postprocess` | Trace execution flows and compute criticality |
| `status` | Show graph statistics |
| `watch` | Auto-update on file changes (fsnotify) |
| `detect-changes` | Risk-scored change impact analysis |
| `search` | Hybrid FTS5 + embeddings + keyword search |
| `visualize` | Interactive D3.js HTML graph visualization |
| `wiki` | Generate markdown wiki from communities |
| `register` | Add a repository to the multi-repo registry |
| `unregister` | Remove a repository from the registry |
| `repos` | List all registered repositories |
| `serve` | Start MCP server (stdio transport) |
| `version` | Show version |

## MCP Tools

When running as an MCP server (`serve`), the following 19 tools are available:

| Tool | Description |
|------|-------------|
| `build_or_update_graph` | Build or incrementally update the knowledge graph |
| `get_minimal_context` | Ultra-compact codebase context (~100 tokens) |
| `get_impact_radius` | Blast-radius analysis for changed files |
| `query_graph` | Explore code relationships (dependents, dependencies, callers, callees) |
| `semantic_search_nodes` | Hybrid search with FTS5 BM25, embeddings, RRF merge |
| `list_graph_stats` | Aggregate graph statistics |
| `find_large_functions` | Find functions/classes exceeding a line threshold |
| `get_review_context` | Token-efficient review context for changes |
| `detect_changes` | Risk-scored, priority-ordered review guidance |
| `visualize_graph` | Generate interactive HTML visualization |
| `list_flows` | List execution flows sorted by criticality |
| `get_flow` | Detailed step-by-step flow information |
| `get_affected_flows` | Find flows affected by changed files |
| `refactor` | Rename preview, dead code detection, move suggestions |
| `apply_refactor` | Apply a previewed refactoring (with dry-run) |
| `find_dead_code` | Find symbols with no callers or references |
| `generate_wiki` | Generate markdown wiki from communities |
| `get_wiki_page` | Retrieve a specific wiki page |
| `rebuild_fts_index` | Rebuild the FTS5 full-text search index |

## Architecture

```
┌───────────────────────────────────────────┐
│              CLI (cobra)                  │
│   build | update | status | watch | ...   │
└─────────────────┬─────────────────────────┘
                  │
     ┌────────────▼────────────┐
     │   WorkerPool (N=NumCPU) │   ← goroutine-per-file parsing
     └──┬─────┬─────┬─────────┘
        │     │     │
    goroutine goroutine goroutine
        │     │     │
     parser parser parser        ← Tree-sitter (CGo, per-goroutine)
        │     │     │
     results channel (buffered)
        │
     graph writer (mutex-serialised DB writes)
        │
     SQLite (WAL mode, FTS5)
```

### Graph Model

**Node kinds:** File, Class, Function, Type, Test

**Edge kinds:** CALLS, IMPORTS_FROM, INHERITS, IMPLEMENTS, CONTAINS, TESTED_BY, DEPENDS_ON, REFERENCES

### Project Structure

```
code-review-graph-go/
├── cmd/code-review-graph/       # CLI binary
├── internal/
│   ├── config/                  # Environment config, constants
│   ├── graph/                   # SQLite graph store, migrations, BFS
│   ├── parser/                  # Tree-sitter AST parsing + WorkerPool
│   ├── incremental/             # Git diff, hash-based skip, fsnotify watcher
│   ├── search/                  # Hybrid search (FTS5 + embeddings + RRF)
│   ├── mcp/                     # MCP JSON-RPC server (stdio transport)
│   ├── tools/                   # 19 MCP tool handlers
│   ├── flows/                   # Execution flow detection + criticality
│   ├── wiki/                    # Markdown wiki from communities
│   ├── refactor/                # Rename preview, dead code, safe apply
│   ├── embeddings/              # Vector embedding store + cosine search
│   ├── registry/                # Multi-repo JSON registry
│   ├── skills/                  # Platform MCP installer + skill files
│   ├── prompts/                 # MCP prompt templates
│   ├── hints/                   # Context-aware next-step suggestions
│   └── visualization/           # D3.js HTML generator
├── tests/fixtures/              # Sample source files per language
├── go.mod
├── Makefile
└── cluade.md                    # Full project spec for AI context
```

## Requirements

- Go 1.22+
- CGo enabled (`CGO_ENABLED=1`)
- C compiler (for Tree-sitter and SQLite)

## Configuration

All settings are optional and controlled via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `CRG_REPO_ROOT` | auto-detect | Override project root |
| `CRG_DATA_DIR` | `<repo>/.code-review-graph` | Override graph data directory |
| `CRG_PARSE_WORKERS` | `min(NumCPU, 8)` | Parallel parse worker count |
| `CRG_MAX_IMPACT_NODES` | `500` | Max nodes in impact radius |
| `CRG_MAX_SEARCH_RESULTS` | `20` | Max search results |

## Performance

Tested on the original code-review-graph Python repo:

- **Full build:** 116 files → 1,798 nodes, 10,418 edges in ~1.5s (8 goroutine workers)
- **Incremental update:** Only re-parses changed + dependent files
- **File watching:** 300ms debounce, instant re-parse on save

## Dependencies

| Package | Purpose |
|---------|---------|
| [go-tree-sitter](https://github.com/smacker/go-tree-sitter) | CGo Tree-sitter bindings (17 languages) |
| [go-sqlite3](https://github.com/mattn/go-sqlite3) | CGo SQLite3 driver (WAL mode, FTS5) |
| [cobra](https://github.com/spf13/cobra) | CLI framework |
| [fsnotify](https://github.com/fsnotify/fsnotify) | Cross-platform file watching |

## License

MIT
