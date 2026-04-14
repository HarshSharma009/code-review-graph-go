# code-review-graph-go

A persistent, incrementally-updated structural knowledge graph for token-efficient code reviews — written in Go with goroutine-based concurrency.

Go port of [code-review-graph](https://github.com/tirth8205/code-review-graph) (Python).

## Features

- **Tree-sitter AST parsing** — 17 languages (Python, JS, TS, Go, Rust, Java, C, C++, C#, Ruby, Kotlin, Swift, PHP, Scala, Lua, Bash)
- **Goroutine-parallel parsing** — WorkerPool with `runtime.NumCPU()` workers
- **SQLite with WAL mode** — concurrent reads, serialised writes, FTS5 search
- **Incremental updates** — git diff + SHA-256 hash-based skip logic
- **Impact analysis** — BFS blast-radius via recursive CTE
- **File watching** — fsnotify with 300ms debounce

## Quick Start

```bash
# Build
CGO_ENABLED=1 go build -o bin/code-review-graph ./cmd/code-review-graph

# Build the graph for your project
./bin/code-review-graph build --repo /path/to/your/project

# Check stats
./bin/code-review-graph status

# Incremental update
./bin/code-review-graph update

# Watch for changes
./bin/code-review-graph watch

# Analyze change impact
./bin/code-review-graph detect-changes --brief
```

## Requirements

- Go 1.22+
- CGo enabled (`CGO_ENABLED=1`)
- C compiler (for Tree-sitter and SQLite)

## License

MIT
