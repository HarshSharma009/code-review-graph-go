# CLAUDE.md - Project Context for Claude Code

## Project Overview

**code-review-graph-go** is a persistent, incrementally-updated knowledge graph for token-efficient code reviews with Claude Code — rewritten in Go for maximum concurrency and performance. It parses codebases using Tree-sitter (via CGo bindings), builds a structural graph in SQLite, and exposes it via MCP tools. Goroutines are used throughout for parallel parsing, concurrent graph traversal, and non-blocking incremental updates.

## Architecture

* **Core Package layout** (`internal/` + `cmd/`):

  + `internal/parser/parser.go` — Tree-sitter multi-language AST parser (Go, Python, TypeScript, JavaScript, Rust, Java, C/C++, and more). Each file is parsed in its own goroutine via a bounded `WorkerPool`.
  + `internal/graph/graph.go` — SQLite-backed graph store (nodes, edges, BFS/DFS impact analysis). WAL mode enabled; connection pool manages concurrent reads.
  + `internal/graph/bfs.go` — Concurrent BFS for blast-radius computation using `sync.WaitGroup` and a channel-based frontier queue.
  + `internal/tools/tools.go` — MCP tool implementations (22 tools), each handler is goroutine-safe.
  + `internal/incremental/incremental.go` — Git-based change detection and file-system watcher (`fsnotify`). Dirty-file fan-out dispatches re-parse jobs concurrently.
  + `internal/embeddings/embeddings.go` — Optional vector embeddings (local ONNX or Google Gemini). Batch encoding runs N embeddings in parallel goroutines.
  + `internal/visualization/visualization.go` — D3.js interactive HTML graph generator.
  + `internal/search/search.go` — FTS5-powered hybrid keyword + vector search.
  + `internal/registry/registry.go` — Multi-repo registry; cross-repo queries fan out concurrently.
  + `internal/community/community.go` — Leiden / file-grouping community detection.
  + `internal/wiki/wiki.go` — Markdown wiki generation from community structure.
  + `cmd/code-review-graph/main.go` — CLI entry point (uses `cobra`).
  + `cmd/mcp-server/main.go` — MCP stdio server entry point.

* **VS Code Extension**: `code-review-graph-vscode/` (TypeScript, unchanged from Python version)

  + Reads from `.code-review-graph/graph.db` via SQLite

* **Database**: `.code-review-graph/graph.db` (SQLite, WAL mode, `_journal_mode=WAL&_synchronous=NORMAL&cache=shared`)

## Concurrency Model

```
┌──────────────────────────────────────────────────────┐
│                    CLI / MCP Handler                 │
└────────────────────┬─────────────────────────────────┘
                     │  fan-out
          ┌──────────▼──────────┐
          │   WorkerPool (N=    │  N = runtime.NumCPU()
          │   runtime.NumCPU())  │
          └──┬───────┬───────┬──┘
             │       │       │
          goroutine goroutine goroutine  ← one per file
             │       │       │
          parser  parser  parser        ← Tree-sitter (CGo, thread-safe)
             │       │       │
          results channel (buffered)
             │
          graph writer (single goroutine, serialised DB writes)
```

Key primitives used:

| Primitive | Where |
|---|---|
| `sync.WaitGroup` | WorkerPool, BFS traversal, batch embeddings |
| `chan ParseResult` | Result collection from parser goroutines |
| `chan FileEvent` | fsnotify → incremental updater pipeline |
| `sync.RWMutex` | In-memory node/edge caches |
| `sync/atomic` | Progress counters, skip-file flags |
| `context.Context` | Cancellation propagated through all goroutines |
| `errgroup.Group` | Fan-out with first-error collection |
| `sync.Once` | Lazy singleton DB connection pool |

**SQLite write serialisation**: all writes go through a single `dbWriter` goroutine fed by a buffered channel. Reads use a `sql.DB` pool (up to `runtime.NumCPU()` concurrent readers in WAL mode).

## Key Commands

```bash
# Development
go test ./...                               # Run all tests
go test ./... -race                         # Run with race detector (CI requirement)
go vet ./...                                # Static analysis
golangci-lint run ./...                     # Lint (golangci-lint)
go build -o bin/code-review-graph ./cmd/code-review-graph

# Build & use
./bin/code-review-graph build               # Full parallel graph build
./bin/code-review-graph update              # Incremental update (changed files only)
./bin/code-review-graph status              # Show graph stats
./bin/code-review-graph watch               # fsnotify-based watch mode
./bin/code-review-graph serve               # Start MCP stdio server
./bin/code-review-graph visualize           # Generate interactive HTML graph
./bin/code-review-graph wiki                # Generate markdown wiki
./bin/code-review-graph detect-changes      # Risk-scored change impact
./bin/code-review-graph eval                # Run evaluation benchmarks
```

## Code Conventions

* **Go version**: 1.22+ (uses `slices`, `maps` stdlib packages)
* **Line length**: 120 chars (enforced by `gofmt` + `wsl` linter)
* **SQL**: Always use parameterized queries (`?` placeholders), never `fmt.Sprintf` into SQL
* **Error handling**: Wrap errors with `fmt.Errorf("context: %w", err)`, never swallow
* **Goroutine discipline**:
  - Every goroutine launched must have a clear owner responsible for `wg.Wait()` / `close(ch)`
  - Never start a goroutine in an `init()` function
  - Always pass `context.Context` as the first argument to functions that launch goroutines
  - Goroutines must respect `ctx.Done()` and return promptly on cancellation
* **Channel patterns**:
  - Use buffered channels sized to `runtime.NumCPU()` for CPU-bound pipelines
  - Sender always closes the channel; receiver ranges over it
  - Never send on a closed channel — use `sync.Once` guarded close helpers
* **CGo (Tree-sitter)**:
  - Tree-sitter C parsers are thread-safe at the parser object level
  - Each `WorkerPool` goroutine holds its own `*Parser` instance — no sharing
  - CGo calls are wrapped in `runtime.LockOSThread()` only where required by the C library
* **Node names**: Sanitize via `sanitizeName()` before returning to MCP clients (strips control chars, caps at 256 chars)
* **File reads**: Read bytes once, hash with SHA-256, then parse (TOCTOU-safe pattern)
* **Logging**: Use `slog` (structured logging); never `fmt.Println` in library code
* **Testing**: Table-driven tests; use `t.Parallel()` in every test and subtest

## Security Invariants

* No `os/exec` with shell interpolation — use `exec.Command` with explicit args only
* No `unsafe.Pointer` arithmetic outside the `internal/cgo` wrapper package
* `validateRepoRoot()` prevents path traversal via the `repo_root` parameter
* `sanitizeName()` strips control characters, caps at 256 chars (prompt injection defence)
* `escHTML()` in visualization escapes HTML entities including quotes and backticks
* SRI hash on D3.js CDN `<script>` tag
* API keys only from environment variables (read via `os.Getenv`), never hardcoded
* `gosec` scanner runs in CI; findings block merge

## Project Structure

```
code-review-graph-go/
├── cmd/
│   ├── code-review-graph/   # CLI binary
│   │   └── main.go
│   └── mcp-server/          # MCP stdio server binary
│       └── main.go
├── internal/
│   ├── parser/              # Tree-sitter AST parsing + WorkerPool
│   ├── graph/               # SQLite graph store, BFS blast-radius
│   ├── tools/               # MCP tool implementations
│   ├── incremental/         # Git diff + fsnotify watcher
│   ├── embeddings/          # Vector embeddings (ONNX / Gemini)
│   ├── visualization/       # D3.js HTML generator
│   ├── search/              # FTS5 + vector hybrid search
│   ├── registry/            # Multi-repo registry
│   ├── community/           # Leiden community detection
│   ├── wiki/                # Markdown wiki generator
│   └── cgo/                 # CGo wrappers for Tree-sitter C libraries
├── tests/
│   ├── parser_test.go
│   ├── graph_test.go
│   ├── tools_test.go
│   ├── incremental_test.go
│   ├── embeddings_test.go
│   └── fixtures/            # Sample source files per language
├── go.mod
├── go.sum
├── .golangci.yml
├── Makefile
└── CLAUDE.md                # ← you are here
```

## Test Structure

* `tests/parser_test.go` — Parser correctness, cross-file resolution, parallel parse correctness under `-race`
* `tests/graph_test.go` — Graph CRUD, stats, impact radius, concurrent read/write correctness
* `tests/tools_test.go` — MCP tool integration tests; goroutine-safe handler checks
* `tests/visualization_test.go` — Export, HTML generation
* `tests/incremental_test.go` — Build, update, git ops, fsnotify event pipeline
* `tests/multilang_test.go` — Language parsing tests (Go, Python, TS, JS, Rust, Java, C/C++, …)
* `tests/embeddings_test.go` — Vector encode/decode, similarity, batch parallel encoding
* `tests/fixtures/` — Sample files for each supported language

All tests must pass with `go test -race ./...`. Race detector failures are treated as test failures in CI.

## CI Pipeline

* **lint**: `golangci-lint run` (includes `staticcheck`, `gosec`, `errcheck`, `wsl`)
* **vet**: `go vet ./...`
* **race**: `go test -race ./...` — race detector is mandatory
* **security**: `gosec ./...`
* **test**: `go test ./...` matrix (Go 1.22, 1.23) with `-coverprofile`, 50% coverage minimum
* **build**: `go build ./...` for all target platforms (linux/amd64, darwin/arm64, windows/amd64)

## Dependencies (go.mod highlights)

```
github.com/smacker/go-tree-sitter       # CGo Tree-sitter bindings
github.com/mattn/go-sqlite3             # CGo SQLite3 driver (WAL mode)
github.com/spf13/cobra                  # CLI framework
github.com/mark3labs/mcp-go             # MCP server SDK for Go
github.com/fsnotify/fsnotify            # Cross-platform file watching
golang.org/x/sync/errgroup             # Fan-out with error collection
github.com/google/generative-ai-go     # Gemini embeddings (optional)
go.uber.org/zap                         # Structured logging (or slog)
```

> CGo is required for Tree-sitter and SQLite. Set `CGO_ENABLED=1` in your build environment.
> On Linux CI, install `build-essential` and `libsqlite3-dev` before building.

---

## TODO — Feature Roadmap

Features are grouped by theme and ordered within each group by priority. Each item notes the Go-specific implementation angle where relevant.

### 🚀 Phase 1 — Core Parity with Python version

These items get the Go port to feature-complete status relative to the Python original.

- [ ] **Language coverage: reach 19 languages**
  Port remaining Tree-sitter grammars (Vue SFC, Solidity, Scala, Swift, Kotlin, PHP, Dart, R, Perl, Lua). Each language adds entries to `EXTENSION_TO_LANGUAGE` and node-type maps in `internal/parser/parser.go`. Add one fixture file and one table-driven test row per language.

- [ ] **Leiden community detection**
  Wrap the `igraph` C library (or port a pure-Go Leiden implementation) in `internal/community/`. Community detection is embarrassingly parallel — partition the graph by connected component first, then detect communities within each partition concurrently using an `errgroup`.

- [ ] **Jupyter / Databricks notebook parser (`.ipynb`)**
  Parse JSON notebook cells in parallel goroutines (one goroutine per cell), dispatching to the appropriate language sub-parser per cell. Aggregate results back into the graph under a synthetic `notebook:<filename>` node.

- [ ] **Interactive D3.js visualization**
  Port `visualization.go` to produce the same force-directed HTML graph with edge-type toggles and search. No runtime server required — embed all JS/CSS inline via Go's `embed.FS`.

- [ ] **Execution flow tracing (`list_flows`, `get_flow`, `get_affected_flows`)**
  Implement call-chain tracing from entry points. Fan out DFS from each entry point in parallel goroutines, collect paths via a result channel, de-duplicate with a `sync.Map`.

- [ ] **`cross_repo_search` MCP tool**
  Fan-out search across all registered repos concurrently using `errgroup`. Each repo's SQLite search runs in its own goroutine; results are merged and ranked by score.

- [ ] **`refactor_tool` + `apply_refactor_tool`**
  Rename preview: collect all reference sites via a parallel graph walk. Dead code detection: find nodes with in-degree 0 concurrently. Stage refactoring ops in a transaction; apply atomically.

- [ ] **Wiki generation with LLM summaries**
  `internal/wiki/wiki.go` — generate one markdown page per community. LLM summary calls (Ollama or Gemini) run concurrently, rate-limited via a semaphore (`chan struct{}` of size N).

---

### ⚡ Phase 2 — Go-native Performance Wins

Items that go beyond Python parity and exploit Go's concurrency model.

- [ ] **Streaming incremental graph updates over gRPC**
  Replace the stdio MCP transport with an optional gRPC server (`cmd/grpc-server/`). Clients subscribe to a `WatchGraph` stream; the server pushes node/edge diffs as they are committed by the `dbWriter` goroutine. Useful for IDE extensions that want live graph updates without polling.

- [ ] **Parallel SHA-256 file hashing at startup**
  On `build`, hash all source files concurrently before parsing to quickly identify unchanged files. A `WorkerPool` of `runtime.NumCPU()` goroutines reads and hashes; results feed the parser pool only for dirty files. Target: hash 10k files in < 500 ms on modern hardware.

- [ ] **Concurrent BFS with early-exit on depth limit**
  Current BFS stops at a configurable depth. Add `context`-based cancellation so that deep subgraphs that exceed a token budget cancel their own goroutines immediately rather than draining the whole queue.

- [ ] **Pipelined parse → embed → write**
  Chain three goroutine stages in a pipeline: `parser → embedder → dbWriter`, each communicating via buffered channels. Back-pressure is handled automatically; no stage blocks the others. Embedding is the bottleneck — size its channel to `2 × numEmbedWorkers`.

- [ ] **Memory-mapped SQLite WAL reader for read-heavy queries**
  For `get_review_context` and `get_impact_radius`, experiment with `mmap`-backed reads (`golang.org/x/sys/unix.Mmap`) against the WAL file to reduce syscall overhead on large graphs (> 100k nodes).

- [ ] **Adaptive worker pool sizing**
  Monitor goroutine scheduling latency via `runtime.NumGoroutine()` and CPU utilisation. Auto-tune `WorkerPool` size between `runtime.NumCPU()` and `4 × runtime.NumCPU()` based on whether the bottleneck is CPU (Tree-sitter parsing) or I/O (file reads, DB writes).

---

### 🧠 Phase 3 — Smarter Analysis

- [ ] **Go-aware import resolution**
  Resolve Go module paths (`go.mod` + `go.sum`) to map import strings to local package directories. Build inter-package edges that respect Go's visibility rules (`exported` vs. `unexported` identifiers). Run module graph construction concurrently with file parsing.

- [ ] **Interface satisfaction edges**
  For Go codebases, detect which concrete types implement which interfaces and add `implements` edges to the graph. This dramatically improves blast-radius accuracy for interface-heavy code (e.g., `io.Reader`, `http.Handler`).

- [ ] **Goroutine-aware call graph**
  When a call site uses `go func()`, tag the call edge as `async`. Surface these edges in `get_impact_radius` so Claude knows that a change may have concurrent callers. Detect common patterns: `go worker(ctx, ch)`, `go func() { ... }()`.

- [ ] **Test coverage gap detection**
  Parse `_test.go` files and map test functions to the functions they call. Report coverage gaps (functions with no test reaching them) as part of `detect_changes`. Run gap analysis concurrently with blast-radius BFS.

- [ ] **Improved semantic search ranking (MRR > 0.6)**
  Replace keyword-only FTS5 with a two-stage retrieval: BM25 candidate retrieval (FTS5) → re-rank with a lightweight embedding similarity (cosine on 384-dim vectors). Re-ranking runs in parallel for all candidates using `errgroup`.

- [ ] **Dead code detection across the whole graph**
  Find all nodes with zero inbound `calls` or `imports` edges (unreachable from any entry point). Run from all entry points concurrently via parallel DFS; take the complement of the reachable set.

---

### 🔌 Phase 4 — Integrations & Distribution

- [ ] **`code-review-graph install` auto-config for Go projects**
  Detect `go.mod` in the repo root and auto-configure `.golangci.yml`, `Makefile` targets, and VS Code `settings.json`. Inject graph-aware instructions into Claude Code's `CLAUDE.md` automatically.

- [ ] **Pre-built binaries via GoReleaser**
  Add `.goreleaser.yml` to produce signed binaries for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`. Publish to GitHub Releases and a Homebrew tap (`brew install code-review-graph`).

- [ ] **Docker image**
  Multi-stage `Dockerfile`: build stage compiles the Go binary with CGo; runtime stage uses `gcr.io/distroless/base`. Publish to `ghcr.io`. Useful for CI/CD pipelines that run `code-review-graph eval` in containers.

- [ ] **JetBrains IDE plugin**
  Port the VS Code extension logic to a JetBrains plugin (Kotlin). Reads from the same `.code-review-graph/graph.db` SQLite file — no Go changes needed, just a new client.

- [ ] **GitHub Actions integration**
  Publish a reusable GitHub Action (`uses: tirth8205/code-review-graph-go/.github/actions/review@v1`) that builds the graph, runs `detect-changes` on the PR diff, and posts a risk-scored comment via the GitHub API.

- [ ] **OpenTelemetry tracing**
  Instrument `WorkerPool`, `dbWriter`, BFS, and MCP tool handlers with `go.opentelemetry.io/otel` spans. Export traces to Jaeger or the OTLP collector. Makes it easy to profile where time is spent on large repositories.

---

### 🧪 Phase 5 — Evaluation & Observability

- [ ] **Port eval benchmarks to Go**
  Rewrite `evaluate/` in Go so benchmarks run without a Python dependency. Use `testing.B` for micro-benchmarks and a custom harness (mirroring the Python eval runner) for end-to-end token-reduction measurements.

- [ ] **Continuous benchmark tracking**
  Store benchmark results in a SQLite file in `.code-review-graph/benchmarks.db`. CLI command `code-review-graph bench --compare` shows regressions vs. the last stored run. Plot with a simple SVG chart (no external charting library needed).

- [ ] **pprof endpoint in MCP server**
  When `--debug` flag is set, expose `net/http/pprof` on a local port. Makes it trivial to capture goroutine dumps, heap profiles, and CPU traces during long-running `watch` sessions.

- [ ] **Chaos / fuzz testing for concurrent paths**
  Add `go test -fuzz` targets for the parser and BFS code. Add a chaos mode (`CRGO_CHAOS=1`) that randomly delays goroutine scheduling to surface race conditions not caught by `-race`.

---

### 💡 Icebox (future / exploratory)

- [ ] **Pure-Go Tree-sitter parser (no CGo)**
  Investigate `github.com/nicholasgasior/go-tree-sitter` or a WASM-compiled grammar approach to eliminate the CGo dependency. This would enable `CGO_ENABLED=0` static builds and simpler cross-compilation.

- [ ] **DuckDB backend (experimental)**
  Replace SQLite with DuckDB (`github.com/marcboeker/go-duckdb`) for analytical queries (e.g., "find all functions called more than 50 times"). DuckDB's columnar engine is faster for aggregation-heavy graph queries. Keep SQLite as the default; DuckDB opt-in via `--backend=duckdb`.

- [ ] **Distributed mode for very large monorepos**
  For monorepos with 100k+ files, shard the graph across multiple SQLite databases (one per top-level package directory). A coordinator goroutine routes queries to the right shard. Cross-shard BFS uses a concurrent fan-out over all shards.

- [ ] **WebAssembly build for browser-based visualization**
  Compile the graph query engine to WASM (`GOOS=js GOARCH=wasm`) so the D3.js visualization can query the graph directly in the browser without a local server. Useful for sharing read-only graph snapshots via static hosting.