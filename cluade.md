# CLAUDE.md - Project Context for Claude Code

## Project Overview

**code-review-graph-go** is a persistent, incrementally-updated knowledge graph for token-efficient code reviews — rewritten in Go for maximum concurrency and performance. It parses codebases using Tree-sitter (via CGo bindings), builds a structural graph in SQLite (WAL mode), and provides CLI tooling for graph construction, incremental updates, impact analysis, and file watching. Goroutines are used throughout for parallel parsing, concurrent graph traversal, and non-blocking incremental updates.

This is a Go port of [code-review-graph](https://github.com/tirth8205/code-review-graph) (Python).

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

### Package Layout

| Package | Path | Description |
|---------|------|-------------|
| **CLI** | `cmd/code-review-graph/main.go` | Cobra CLI: build, update, status, watch, detect-changes, version |
| **Config** | `internal/config/config.go` | Environment-driven configuration (CRG_* vars), limits, ignore patterns |
| **Graph Store** | `internal/graph/` | SQLite-backed graph (nodes, edges, metadata), schema migrations v1→v6, BFS impact radius via recursive CTE, batch queries, FTS5 search |
| **Parser** | `internal/parser/` | Tree-sitter multi-language AST parser (17 languages), WorkerPool for goroutine-parallel parsing |
| **Incremental** | `internal/incremental/` | Git-based change detection, file collection, hash-based skip logic, dependent file expansion, fsnotify file watcher |

### Implemented Languages (17)

Python, JavaScript, TypeScript, TSX, Go, Rust, Java, C, C++, C#, Ruby, Kotlin, Swift, PHP, Scala, Lua, Bash

### Database Schema (v6)

```sql
-- Core tables
nodes (id, kind, name, qualified_name, file_path, line_start, line_end,
       language, parent_name, params, return_type, modifiers, is_test,
       file_hash, extra, updated_at, signature, community_id)
edges (id, kind, source_qualified, target_qualified, file_path, line, extra, updated_at)
metadata (key, value)

-- Analysis tables (v3+)
flows (id, name, entry_point_id, depth, node_count, file_count, criticality, path_json)
flow_memberships (flow_id, node_id, position)

-- Community tables (v4+)
communities (id, name, level, parent_id, cohesion, size, dominant_language, description)

-- Search (v5+)
nodes_fts (FTS5: name, qualified_name, file_path, signature)

-- Summary tables (v6+)
community_summaries, flow_snapshots, risk_index
```

Node kinds: `File`, `Class`, `Function`, `Type`, `Test`
Edge kinds: `CALLS`, `IMPORTS_FROM`, `INHERITS`, `IMPLEMENTS`, `CONTAINS`, `TESTED_BY`, `DEPENDS_ON`, `REFERENCES`

## Concurrency Model

| Primitive | Where |
|-----------|-------|
| `sync.Mutex` | Graph Store write serialisation |
| `sync.WaitGroup` | WorkerPool goroutine lifecycle |
| `chan ParseResult` | Result collection from parser goroutines |
| `chan FileJob` | Job distribution to parser goroutines |
| `context.Context` | Cancellation propagated through all goroutines |
| `sync.Mutex` | Parser instance pool (one per goroutine) |
| `time.AfterFunc` | Watch mode debounce (300ms) |

**SQLite write serialisation**: all writes go through a `sync.Mutex` in `Store`. Reads use the `sql.DB` connection pool (concurrent readers in WAL mode).

## Key Commands

```bash
# Build
CGO_ENABLED=1 CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" go build -o bin/code-review-graph ./cmd/code-review-graph

# Use
./bin/code-review-graph build [--repo PATH]        # Full parallel graph build
./bin/code-review-graph update [--base HEAD~1]      # Incremental update (changed files only)
./bin/code-review-graph status                      # Show graph stats
./bin/code-review-graph watch                       # fsnotify-based watch mode
./bin/code-review-graph detect-changes [--brief]    # Risk-scored change impact
./bin/code-review-graph version                     # Show version

# Development
go test ./...                                       # Run all tests
go test ./... -race                                 # Run with race detector
go vet ./...                                        # Static analysis
make build                                          # Build via Makefile
make test-race                                      # Race-detector tests
```

## Code Conventions

* **Go version**: 1.22+ (module requires 1.25+)
* **CGo required**: Tree-sitter and SQLite both need CGo. Set `CGO_ENABLED=1` and `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5"`.
* **SQL**: Always use parameterized queries (`?` placeholders), never `fmt.Sprintf` into SQL
* **Error handling**: Wrap errors with `fmt.Errorf("context: %w", err)`, never swallow
* **Goroutine discipline**:
  - Every goroutine has a clear owner responsible for `wg.Wait()` / `close(ch)`
  - Always pass `context.Context` as the first argument to functions that launch goroutines
  - Goroutines must respect `ctx.Done()` and return promptly on cancellation
* **Channel patterns**:
  - Buffered channels sized to `runtime.NumCPU()` for CPU-bound pipelines
  - Sender always closes the channel; receiver ranges over it
* **Logging**: Use `log/slog` (structured logging); never `fmt.Println` in library code
* **Testing**: Table-driven tests; use `t.Parallel()` in every test and subtest

## Security Invariants

* No `os/exec` with shell interpolation — use `exec.Command` with explicit args only
* `SanitizeName()` strips control characters, caps at 256 chars (prompt injection defence)
* Git ref validation via `safeGitRef` regex before passing to `exec.Command`
* Binary file detection to skip non-text files
* API keys only from environment variables, never hardcoded
* Ignore pattern matching prevents path traversal into sensitive directories

## Project Structure

```
code-review-graph-go/
├── cmd/
│   ├── code-review-graph/   # CLI binary
│   │   └── main.go
│   └── mcp-server/          # MCP stdio server (planned)
│       └── main.go
├── internal/
│   ├── config/              # Environment config, constants
│   │   └── config.go
│   ├── graph/               # SQLite graph store, migrations, BFS
│   │   ├── models.go
│   │   ├── store.go
│   │   ├── migrations.go
│   │   └── sqlite_options.go
│   ├── parser/              # Tree-sitter AST parsing + WorkerPool
│   │   ├── parser.go
│   │   ├── languages.go
│   │   └── workerpool.go
│   ├── incremental/         # Git diff + fsnotify watcher
│   │   ├── builder.go
│   │   ├── git.go
│   │   └── watcher.go
│   ├── search/              # FTS5 + vector hybrid search (planned)
│   ├── visualization/       # D3.js HTML generator (planned)
│   ├── tools/               # MCP tool implementations (planned)
│   ├── community/           # Community detection (planned)
│   ├── wiki/                # Markdown wiki generator (planned)
│   └── registry/            # Multi-repo registry (planned)
├── tests/
│   └── fixtures/            # Sample source files per language
├── go.mod
├── go.sum
├── .gitignore
├── .golangci.yml
├── Makefile
├── LICENSE
├── README.md
└── cluade.md                # ← you are here
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CRG_REPO_ROOT` | (auto-detect) | Override project root |
| `CRG_DATA_DIR` | `<repo>/.code-review-graph` | Override graph data directory |
| `CRG_PARSE_WORKERS` | `min(NumCPU, 8)` | Parallel parse worker count |
| `CRG_SERIAL_PARSE` | `0` | Set `1` for serial parsing (debug) |
| `CRG_GIT_TIMEOUT` | `30` | Git subprocess timeout (seconds) |
| `CRG_RECURSE_SUBMODULES` | `false` | Include git submodule files |
| `CRG_MAX_IMPACT_NODES` | `500` | Max nodes in impact radius |
| `CRG_MAX_IMPACT_DEPTH` | `2` | Max BFS depth for impact |
| `CRG_MAX_BFS_DEPTH` | `15` | Max BFS depth |
| `CRG_MAX_SEARCH_RESULTS` | `20` | Max search results |
| `CRG_DEPENDENT_HOPS` | `2` | Dependency tracking hops |

## Dependencies (go.mod)

```
github.com/smacker/go-tree-sitter       # CGo Tree-sitter bindings (17 languages)
github.com/mattn/go-sqlite3             # CGo SQLite3 driver (WAL mode, FTS5)
github.com/spf13/cobra                  # CLI framework
github.com/fsnotify/fsnotify            # Cross-platform file watching
```

> CGo is required for Tree-sitter and SQLite. Set `CGO_ENABLED=1` in your build environment.

## Performance (tested on code-review-graph-main repo)

- **Full build**: 116 files → 1798 nodes, 10418 edges in ~1.5s (8 goroutine workers)
- **Incremental update**: Only re-parses changed + dependent files
- **File watching**: 300ms debounce, instant re-parse on save

---

## Implementation Status

### ✅ Completed

- [x] Project scaffolding (go.mod, Makefile, .gitignore, .golangci.yml)
- [x] SQLite graph store with WAL mode, connection pooling, write serialisation
- [x] Schema migrations v1→v6 (nodes, edges, metadata, flows, communities, FTS5, summaries)
- [x] Tree-sitter multi-language parser (17 languages via CGo)
- [x] WorkerPool goroutine-based parallel parsing
- [x] File-level, class-level, and function-level node extraction
- [x] Edge extraction: CONTAINS, CALLS, IMPORTS_FROM
- [x] Import extraction for Python, Go, JS/TS, Java, Rust, C/C++
- [x] Test function detection (per-language prefixes + file patterns)
- [x] Git-based change detection (diff, staged, tracked files)
- [x] Incremental update with hash-based skip + dependent file expansion
- [x] fsnotify file watcher with 300ms debounce
- [x] BFS blast-radius via SQLite recursive CTE
- [x] Batch node/edge queries with SQLite variable limit safety
- [x] FTS5 keyword search table
- [x] Cobra CLI: build, update, status, watch, detect-changes, version
- [x] Colored banner with terminal detection
- [x] SanitizeName for prompt injection defence
- [x] Binary file detection, symlink skip, ignore patterns

### 🚧 Planned

- [ ] D3.js interactive HTML visualization (`internal/visualization/`)
- [ ] MCP server with stdio transport (`cmd/mcp-server/`)
- [ ] MCP tool implementations (22 tools)
- [ ] FTS5-powered search API (`internal/search/`)
- [ ] Community detection (`internal/community/`)
- [ ] Wiki generation (`internal/wiki/`)
- [ ] Multi-repo registry (`internal/registry/`)
- [ ] Execution flow tracing
- [ ] Vector embeddings (optional)
- [ ] Comprehensive test suite
- [ ] CI pipeline (GitHub Actions)
- [ ] GoReleaser for pre-built binaries
