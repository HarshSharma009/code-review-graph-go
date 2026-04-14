# Low-Level Design: Architecture Map

## System Context

```
┌─────────────────────────────────────────────────────────────────────┐
│                         User / AI Tool                              │
│   (Claude Code, Cursor, Windsurf, Zed, Continue, OpenCode, CLI)    │
└────────────┬───────────────────────────────────┬────────────────────┘
             │ CLI commands                      │ MCP JSON-RPC 2.0
             │                                   │ (stdio transport)
┌────────────▼───────────────────────────────────▼────────────────────┐
│                     code-review-graph-go                            │
│  ┌──────────┐  ┌───────────┐  ┌──────────┐  ┌───────────────────┐  │
│  │   CLI    │  │ MCP Server│  │  Skills   │  │     Registry      │  │
│  │ (cobra)  │  │ (JSON-RPC)│  │ Installer │  │  (multi-repo)     │  │
│  └────┬─────┘  └─────┬─────┘  └──────────┘  └───────────────────┘  │
│       │              │                                              │
│  ┌────▼──────────────▼──────────────────────────────────────────┐   │
│  │                     Tool Registry (19 tools)                 │   │
│  └──┬────┬────┬────┬────┬────┬────┬────┬────┬────┬────┬────┬───┘   │
│     │    │    │    │    │    │    │    │    │    │    │    │         │
│  ┌──▼──┐ │ ┌──▼──┐ │ ┌──▼──┐ │ ┌──▼──┐ │ ┌──▼──┐ │ ┌──▼──┐      │
│  │Graph│ │ │Srch │ │ │Flows│ │ │Refac│ │ │Wiki │ │ │ Viz │      │
│  │Store│ │ │     │ │ │     │ │ │     │ │ │     │ │ │     │      │
│  └──┬──┘ │ └──┬──┘ │ └──┬──┘ │ └──┬──┘ │ └──┬──┘ │ └──┬──┘      │
│     │    │    │    │    │    │    │    │    │    │    │            │
│  ┌──▼────▼────▼────▼────▼────▼────▼────▼────▼────▼────▼──┐        │
│  │              SQLite (WAL mode, FTS5)                   │        │
│  │  nodes | edges | metadata | flows | communities | fts  │        │
│  └────────────────────────────────────────────────────────┘        │
└─────────────────────────────────────────────────────────────────────┘
```

---

## Package Dependency Graph

```
cmd/code-review-graph/main.go
├── internal/graph          (Store, models, migrations)
├── internal/incremental    (FullBuild, IncrementalUpdate, Watch)
├── internal/mcp            (MCP Server)
├── internal/search         (HybridSearch)
├── internal/flows          (TraceFlows)
├── internal/wiki           (GenerateWiki)
├── internal/registry       (multi-repo)
├── internal/skills         (FullInstall)
└── internal/visualization  (GenerateHTML)

internal/mcp
├── internal/tools          (Registry, 19 ToolDefs)
├── internal/hints          (Session, GenerateHints)
├── internal/prompts        (AllPrompts)
└── internal/graph          (Store)

internal/tools
├── internal/graph
├── internal/incremental
├── internal/search
├── internal/embeddings
├── internal/flows
├── internal/refactor
├── internal/wiki
└── internal/visualization

internal/search
├── internal/graph
└── internal/embeddings

internal/flows
├── internal/graph
└── internal/config

internal/refactor
├── internal/graph
└── internal/flows

internal/wiki
├── internal/graph
└── internal/flows

internal/incremental
├── internal/graph
├── internal/parser
└── internal/config

internal/parser
└── internal/graph          (NodeInfo, EdgeInfo types)

internal/embeddings
└── internal/graph

internal/visualization
└── internal/graph

internal/hints              (no internal deps)
internal/prompts            (no internal deps)
internal/config             (no internal deps)
internal/registry           (no internal deps — uses adapter pattern)
internal/skills             (no internal deps — uses os/exec)
```

---

## Core Data Model

### Graph Schema (SQLite v6)

```
┌──────────────────────────────────────────────────────────────────┐
│                           nodes                                  │
├──────────────────────────────────────────────────────────────────┤
│ id            INTEGER PRIMARY KEY AUTOINCREMENT                  │
│ kind          TEXT NOT NULL  (File|Class|Function|Type|Test)      │
│ name          TEXT NOT NULL                                      │
│ qualified_name TEXT NOT NULL UNIQUE                               │
│ file_path     TEXT NOT NULL                                      │
│ line_start    INTEGER                                            │
│ line_end      INTEGER                                            │
│ language      TEXT                                                │
│ parent_name   TEXT                                                │
│ params        TEXT                                                │
│ return_type   TEXT                                                │
│ modifiers     TEXT                                                │
│ is_test       INTEGER DEFAULT 0                                  │
│ file_hash     TEXT                                                │
│ extra         TEXT DEFAULT '{}'                                   │
│ updated_at    INTEGER                                            │
│ signature     TEXT           (v2+)                                │
│ community_id  INTEGER        (v4+)                               │
└──────────────────────────────────────────────────────────────────┘
         │ 1
         │
         │ qualified_name ←→ source_qualified / target_qualified
         │
         │ *
┌──────────────────────────────────────────────────────────────────┐
│                           edges                                  │
├──────────────────────────────────────────────────────────────────┤
│ id               INTEGER PRIMARY KEY AUTOINCREMENT               │
│ kind             TEXT NOT NULL  (CALLS|IMPORTS_FROM|INHERITS|     │
│                                  IMPLEMENTS|CONTAINS|TESTED_BY|  │
│                                  DEPENDS_ON|REFERENCES)          │
│ source_qualified TEXT NOT NULL                                    │
│ target_qualified TEXT NOT NULL                                    │
│ file_path        TEXT                                             │
│ line             INTEGER                                         │
│ extra            TEXT DEFAULT '{}'                                │
│ updated_at       INTEGER                                         │
└──────────────────────────────────────────────────────────────────┘

┌──────────────────────┐  ┌────────────────────────────────────────┐
│     metadata         │  │           nodes_fts (FTS5 v5+)         │
├──────────────────────┤  ├────────────────────────────────────────┤
│ key   TEXT PK        │  │ name, qualified_name, file_path,       │
│ value TEXT           │  │ signature                              │
└──────────────────────┘  │ tokenize='porter unicode61'            │
                          └────────────────────────────────────────┘

┌──────────────────────────────┐  ┌────────────────────────────────┐
│          flows (v3+)         │  │   flow_memberships (v3+)       │
├──────────────────────────────┤  ├────────────────────────────────┤
│ id             INTEGER PK    │  │ flow_id   INTEGER FK→flows.id  │
│ name           TEXT          │  │ node_id   INTEGER FK→nodes.id  │
│ entry_point_id INTEGER       │  │ position  INTEGER              │
│ depth          INTEGER       │  └────────────────────────────────┘
│ node_count     INTEGER       │
│ file_count     INTEGER       │  ┌────────────────────────────────┐
│ criticality    REAL          │  │     communities (v4+)          │
│ path_json      TEXT          │  ├────────────────────────────────┤
└──────────────────────────────┘  │ id, name, level, parent_id,   │
                                  │ cohesion, size,                │
                                  │ dominant_language, description │
                                  └────────────────────────────────┘
```

### Qualified Name Construction

```
File nodes:     file_path                         e.g. "cmd/main.go"
Top-level:      file_path::name                   e.g. "cmd/main.go::main"
Nested (child): file_path::parent_name.child_name e.g. "auth/service.go::UserService.GetByID"
```

### Node Kinds

| Kind | Extracted From |
|------|----------------|
| `File` | Every parsed file |
| `Class` | class/struct/trait/interface declarations |
| `Function` | function/method/def declarations |
| `Type` | type aliases, enums, typedefs |
| `Test` | Functions matching test patterns (per language) |

### Edge Kinds

| Kind | Meaning |
|------|---------|
| `CALLS` | Function A calls function B |
| `IMPORTS_FROM` | File A imports symbol from file/module B |
| `INHERITS` | Class A extends/inherits class B |
| `IMPLEMENTS` | Class A implements interface B |
| `CONTAINS` | File contains class/function (parent→child) |
| `TESTED_BY` | Function is tested by test function |
| `DEPENDS_ON` | File-level dependency |
| `REFERENCES` | Symbol reference (type annotation, usage) |

---

## Concurrency Model

```
                        ┌─────────────────────┐
                        │    CLI / MCP call    │
                        └──────────┬──────────┘
                                   │
                        ┌──────────▼──────────┐
                        │   context.Context    │  ← cancellation propagation
                        └──────────┬──────────┘
                                   │
              ┌────────────────────▼────────────────────┐
              │           WorkerPool.ParseAll            │
              │       (N = min(NumCPU, 8) goroutines)    │
              └────┬──────┬──────┬──────┬──────┬────────┘
                   │      │      │      │      │
                ┌──▼──┐┌──▼──┐┌──▼──┐┌──▼──┐┌──▼──┐
                │ W-1 ││ W-2 ││ W-3 ││ W-4 ││ W-N │    each worker:
                └──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘    - reads from chan FileJob
                   │      │      │      │      │       - owns its own CodeParser
                   │      │      │      │      │       - sends ParseResult to out chan
                   └──────┴──────┴──┬───┴──────┘
                                    │
                         chan ParseResult (buffered)
                                    │
                         ┌──────────▼──────────┐
                         │   Result Consumer    │
                         │ StoreFileNodesEdges  │
                         └──────────┬──────────┘
                                    │
                         ┌──────────▼──────────┐
                         │  Store.writeMu       │  ← sync.Mutex serialises writes
                         │  (single writer)     │
                         └──────────┬──────────┘
                                    │
                         ┌──────────▼──────────┐
                         │  SQLite WAL mode     │  ← concurrent readers OK
                         └─────────────────────┘
```

### Synchronisation Primitives

| Primitive | Location | Purpose |
|-----------|----------|---------|
| `sync.Mutex` | `graph.Store.writeMu` | Serialise all DB writes |
| `sync.Mutex` | `parser.CodeParser.mu` | Protect parser instance map |
| `sync.Mutex` | `mcp.Server.mu` | Serialise stdio JSON-RPC writes |
| `sync.Mutex` | `embeddings.Store.mu` | Protect embedding operations |
| `sync.Mutex` | `registry.Registry.mu` | Protect registry file read/write |
| `sync.Mutex` | `refactor.pendingMu` | Protect pending refactor map |
| `sync.WaitGroup` | `WorkerPool` | Wait for all parse goroutines |
| `chan FileJob` | `WorkerPool` | Distribute files to workers |
| `chan ParseResult` | `WorkerPool` | Collect results from workers |
| `context.Context` | All goroutine-launching functions | Cancellation signal |
| `time.AfterFunc` | `watcher.go` | 300ms debounce for file events |

---

## Component Detail

### 1. Graph Store (`internal/graph/`)

**Files:** `store.go`, `models.go`, `migrations.go`, `sqlite_options.go`

```
NewStore(dbPath)
  ├── sql.Open("sqlite3", dsn)      dsn includes WAL + foreign keys
  ├── ensureSchema()                 CREATE TABLE IF NOT EXISTS (v1)
  ├── runMigrations()                v2→v6 incremental DDL
  └── return &Store{db, writeMu}
```

**Write path** (all go through `writeMu.Lock()`):

| Method | Operation |
|--------|-----------|
| `UpsertNode` | INSERT ON CONFLICT UPDATE, returns node ID |
| `UpsertEdge` | Lookup existing → UPDATE or INSERT |
| `RemoveFileData` | DELETE nodes + edges WHERE file_path = ? |
| `StoreFileNodesEdges` | Transaction: RemoveFileData → batch UpsertNode → batch UpsertEdge |
| `SetMetadata` | INSERT OR REPLACE |

**Read path** (concurrent via `sql.DB` pool):

| Method | Operation |
|--------|-----------|
| `GetNode` | SELECT * WHERE qualified_name = ? |
| `GetNodesByFile` | SELECT * WHERE file_path = ? |
| `SearchNodes` | LIKE-based keyword search (fallback) |
| `GetImpactRadius` | Recursive CTE BFS over edges table |
| `BatchGetNodes` | SELECT IN (batches of 450) |
| `GetEdgesAmong` | SELECT WHERE source AND target IN set |

**Impact analysis algorithm:**

```sql
WITH RECURSIVE impact(qualified_name, depth) AS (
    SELECT qualified_name, 0 FROM _impact_seeds
    UNION
    SELECT e.source_qualified, i.depth + 1
    FROM edges e JOIN impact i ON e.target_qualified = i.qualified_name
    WHERE i.depth < ?max_depth
)
SELECT DISTINCT qualified_name FROM impact LIMIT ?max_nodes
```

### 2. Parser (`internal/parser/`)

**Files:** `parser.go`, `workerpool.go`, `languages.go`

**Language registration** (`languages.go`):

17 languages registered with file-extension mapping, per-language AST node types for classes, functions, imports, and test detection patterns.

**Parse pipeline** (`parser.go`):

```
ParseBytes(ctx, filePath, source)
  ├── DetectLanguage(filePath)       extension → language string
  ├── getParser(language)            lazy-init sitter.Parser per language
  ├── parser.ParseCtx(ctx, source)   Tree-sitter C call via CGo
  └── walkAST(tree, source)
       ├── extractFileNode()
       ├── for each class node:
       │    ├── extractClassNode()    → NodeInfo{Kind: "Class"}
       │    ├── extractMethods()      → NodeInfo{Kind: "Function"} + CONTAINS edge
       │    └── extractInheritance()  → INHERITS / IMPLEMENTS edges
       ├── for each function node:
       │    ├── extractFunctionNode() → NodeInfo{Kind: "Function"}
       │    └── extractCalls()        → CALLS edges
       ├── extractImports()           → IMPORTS_FROM edges
       └── detectTestFunctions()      → IsTest flag + TESTED_BY edges
```

**Worker pool** (`workerpool.go`):

```
ParseAll(ctx, jobs)
  ├── make(chan FileJob, len(jobs))
  ├── make(chan ParseResult, numWorkers)
  ├── for i := 0; i < numWorkers; i++:
  │    go worker(ctx, jobCh, resultCh)
  │      ├── parser := NewCodeParser()
  │      └── for job := range jobCh:
  │           result := parseSingleFile(ctx, parser, job)
  │           resultCh <- result
  ├── wg.Wait(); close(resultCh)
  └── return resultCh
```

### 3. Incremental Engine (`internal/incremental/`)

**Files:** `builder.go`, `git.go`, `watcher.go`

**FullBuild:**

```
FullBuild(ctx, repoRoot, store)
  ├── CollectAllFiles(repoRoot)         git ls-files or walk
  ├── LoadIgnorePatterns(repoRoot)      .code-review-graph/ignore
  ├── filter: ShouldIgnore, binary detection, symlink skip
  ├── store.GetAllFiles() → remove stale files from DB
  ├── WorkerPool.ParseAll(ctx, jobs)    parallel parse
  ├── for result := range results:
  │    store.StoreFileNodesEdges(...)   serialised writes
  ├── store.SetMetadata("last_updated", ...)
  └── return BuildResult{FilesParsed, TotalNodes, TotalEdges, Errors}
```

**IncrementalUpdate:**

```
IncrementalUpdate(ctx, repoRoot, store, base, changedFiles)
  ├── if changedFiles == nil:
  │    changedFiles = GetChangedFiles(repoRoot, base)
  │    if empty: GetStagedAndUnstaged(repoRoot)
  ├── for each changed file:
  │    dependents := FindDependents(store, file)    multi-hop BFS on edges
  │    expand changed set
  ├── for each file in expanded set:
  │    if hash unchanged → skip
  │    parse and store
  └── return UpdateResult{FilesUpdated, ChangedFiles, DependentFiles}
```

**Watch:**

```
Watch(ctx, repoRoot, store)
  ├── fsnotify.NewWatcher()
  ├── walk dirs → watcher.Add(dir)
  ├── event loop:
  │    case event := <-watcher.Events:
  │      debounce 300ms via time.AfterFunc
  │      if CREATE/WRITE → ParseFileToStore(ctx, store, path, source)
  │      if REMOVE       → store.RemoveFileData(path)
  │    case <-ctx.Done():
  │      return
  └── blocks until cancelled
```

### 4. Hybrid Search (`internal/search/`)

**Files:** `search.go`, `search_test.go`

```
HybridSearch(store, query, kind, limit, contextFiles, embStore)
  │
  ├── Phase 1: Gather ranked lists
  │    ├── ftsSearch(db, query, limit*3)
  │    │    └── FTS5 MATCH with BM25 scoring (negated rank)
  │    └── embeddingSearch(store, embStore, query, limit*3)
  │         └── embStore.Search → cosine similarity → map QN→nodeID
  │
  ├── Phase 2: Merge or fallback
  │    ├── if any results → rrfMerge(fts, emb)
  │    │    └── RRF score = Σ 1/(k + rank + 1) across lists, k=60
  │    └── else → keywordSearch(db, query, limit*3)
  │         └── LIKE '%word%' with exact/prefix/contains scoring
  │
  ├── Phase 3: Batch-fetch candidate nodes (batches of 450)
  │
  ├── Phase 4: Apply boosting
  │    ├── detectQueryKindBoost(query)
  │    │    PascalCase → Class/Type × 1.5
  │    │    snake_case → Function × 1.5
  │    │    dotted     → _qualified × 2.0
  │    └── context-file boost × 1.5
  │
  └── Phase 5: Kind filter + build []Result
```

### 5. MCP Server (`internal/mcp/`)

**Files:** `server.go`, `server_test.go`

```
NewServer(store, repoRoot)
  ├── tools.NewRegistry(store, repoRoot)
  ├── toolMap = map all 19 tools by name
  ├── hints.NewSession()
  └── return Server{..., reader: os.Stdin, writer: os.Stdout}

Run(ctx)
  └── loop: read Content-Length header → read JSON body
       ├── parse jsonRPCRequest
       └── handleRequest(ctx, req)
            ├── "initialize"    → protocol version, capabilities (tools, prompts)
            ├── "tools/list"    → all ToolDef metadata
            ├── "tools/call"    → lookup tool → Handler(ctx, args)
            │    └── if result is map → append GenerateHints → _hints
            ├── "prompts/list"  → all Prompt metadata
            ├── "prompts/get"   → Prompt.Handler(args) → messages
            └── "ping"          → {}
```

**JSON-RPC wire format:**

```
→ stdin:  Content-Length: N\r\n\r\n{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{...}}
← stdout: Content-Length: M\r\n\r\n{"jsonrpc":"2.0","id":1,"result":{...}}
```

### 6. Execution Flows (`internal/flows/`)

**File:** `flows.go`

```
DetectEntryPoints(store)
  ├── SQL: find all CALLS targets (nodes that ARE called)
  ├── get all Function + Test nodes
  └── filter: keep nodes that are NOT call targets
       OR have framework decorators (route, endpoint, handler, etc.)
       OR match entry-point name patterns (main, run, setup, etc.)

TraceFlows(store, maxDepth)
  ├── entries := DetectEntryPoints(store)
  └── for each entry:
       ├── BFS via CALLS edges (breadth-first, maxDepth limit)
       │    queue → dequeue → GetEdgesBySource → enqueue callees
       ├── collect unique nodes, files
       ├── criticality := computeCriticality(flow)
       │    ├── file_spread:  fileCount / totalFiles
       │    ├── external:     calls to nodes in other files
       │    ├── security:     touches security-sensitive keywords
       │    ├── test_gap:     untested nodes in flow
       │    └── depth_factor: depth / maxDepth
       │    → weighted sum normalised to [0, 1]
       └── append Flow{entry, path, depth, criticality}

StoreFlows(store, flows)
  ├── DELETE FROM flows; DELETE FROM flow_memberships
  └── INSERT each flow + membership rows
```

### 7. Refactor Engine (`internal/refactor/`)

**File:** `refactor.go`

```
RenamePreview(store, oldName, newName)
  ├── store.SearchNodes(oldName, 50)
  ├── find best match (exact qualified name or name match)
  ├── build Edit list:
  │    ├── definition site (high confidence)
  │    ├── callers: GetEdgesByTarget → CALLS edges (high confidence)
  │    └── importers: IMPORTS_FROM edges (medium confidence)
  ├── genID() → unique refactor ID
  ├── store in pending map (600s expiry)
  └── return Preview{RefactorID, Edits, Stats}

FindDeadCode(store, kind, filePattern)
  ├── SQL: all Function/Class nodes
  ├── for each candidate:
  │    ├── skip if is_test or HasFrameworkDecorator or MatchesEntryName
  │    ├── check: any CALLS edges targeting this node?
  │    ├── check: any TESTED_BY edges?
  │    ├── check: any IMPORTS_FROM edges?
  │    └── if no references → dead code
  └── return []map with node details

ApplyRefactor(refactorID, repoRoot, dryRun)
  ├── lookup pending[refactorID] → Preview
  ├── validate: no path traversal (filepath.Rel check)
  ├── for each Edit:
  │    ├── read file content
  │    ├── strings.Replace(content, edit.Old, edit.New, 1)
  │    └── if !dryRun → write file
  └── return {applied, skipped, diffs}
```

### 8. Embeddings (`internal/embeddings/`)

**File:** `embeddings.go`

```
Provider interface {
    Embed(texts []string) ([][]float32, error)    batch embed
    EmbedQuery(text string) ([]float32, error)     single query
    Dimension() int
    Name() string
}

Store struct {
    provider Provider
    db       *sql.DB        SQLite with embeddings table
    mu       sync.Mutex
}

EmbedNodes(nodes, batchSize)
  ├── for each node → nodeToText(node)   name + kind + file + params
  ├── hash text → skip if unchanged
  ├── batch provider.Embed(texts)
  └── INSERT OR REPLACE blob (encodeVector: float32 → binary)

Search(query, limit)
  ├── provider.EmbedQuery(query)
  ├── scan ALL embeddings rows
  ├── cosineSimilarity(query_vec, stored_vec)
  └── top-K by score → []SearchResult{QualifiedName, Score}
```

### 9. Wiki (`internal/wiki/`)

**File:** `wiki.go`

```
GenerateWiki(store, wikiDir)
  ├── GetCommunities(store)          SQL from communities + nodes tables
  ├── for each community:
  │    ├── slug := slugify(name)     collision handling with counter
  │    ├── content := generateCommunityPage(store, community)
  │    │    ├── header + description
  │    │    ├── members table (top 50 by line count)
  │    │    ├── execution flows touching this community
  │    │    └── cross-community dependency links
  │    └── write slug.md (skip if content unchanged)
  ├── generate index.md with links to all pages
  └── return GenerateResult{Generated, Updated, Unchanged}
```

### 10. Skills Installer (`internal/skills/`)

**File:** `skills.go`

```
FullInstall(repoRoot, platform)
  ├── InstallPlatformConfigs(repoRoot, platform, false)
  │    for each Platform in targets:
  │      ├── detect: platform.Detect() (check config file exists)
  │      ├── read existing JSON/TOML config
  │      ├── inject MCP server entry: {command, args, env}
  │      └── write updated config
  ├── GenerateSkills(repoRoot)
  │    write 4 skill .md files to .code-review-graph/skills/
  ├── InstallHooks(repoRoot)
  │    write .claude/settings.json with allowed tools
  ├── InstallGitHook(repoRoot)
  │    write .git/hooks/pre-commit: runs "code-review-graph update"
  ├── InjectClaudeMD(repoRoot)
  │    append/update marked section in CLAUDE.md
  ├── InjectPlatformInstructions(repoRoot, platform)
  │    append to .cursorrules / .windsurfrules / etc.
  └── exec.Command("code-review-graph", "build", "--repo", repoRoot)
```

### 11. Hints Engine (`internal/hints/`)

**File:** `hints.go`

```
Session struct {
    toolHistory    []string       last N tool calls
    queriedNodes   map[string]bool
    touchedFiles   map[string]bool
    InferredIntent string          "reviewing"|"debugging"|"refactoring"|"exploring"
    LastToolTime   time.Time
}

GenerateHints(toolName, result, session)
  ├── session.RecordToolCall(toolName)
  ├── session.InferIntent()          match tool history → intentTools patterns
  ├── nextSteps := buildNextSteps(toolName, session)
  │    └── workflow map: tool → suggested follow-up tools
  ├── warnings := extractWarnings(result)
  │    └── check "truncated", "error", high counts
  ├── related := buildRelated(result, session)
  │    └── extract file paths, node names from result
  ├── trackResult(result, session)
  │    └── record files and nodes mentioned in result
  └── return Hints{NextSteps, Related, Warnings}
```

### 12. Prompts (`internal/prompts/`)

**File:** `prompts.go`

5 MCP prompt templates, each with a `Handler func(args) []PromptMessage`:

| Prompt | Arguments | Purpose |
|--------|-----------|---------|
| `review_changes` | `base` (git ref) | Pre-commit review workflow |
| `architecture_map` | `scope` (path filter) | System architecture understanding |
| `debug_issue` | `symptom`, `file` | Systematic debugging |
| `onboard_developer` | `area` (focus area) | New developer onboarding |
| `pre_merge_check` | `branch` | Pre-merge validation checklist |

### 13. Visualization (`internal/visualization/`)

**File:** `visualization.go`

```
GenerateHTML(store, outputPath)
  ├── ExportGraphData(store)
  │    ├── get all File nodes
  │    ├── collect all qualified names
  │    ├── get all edges among those nodes
  │    ├── resolve edges to existing endpoints
  │    └── compute stats (node/edge counts by kind)
  ├── json.Marshal(exportData)
  └── write HTML template with embedded D3.js
       ├── force-directed layout
       ├── node coloring by kind
       ├── edge coloring by kind with toggles
       ├── search with highlighting
       ├── collapsible file groups
       ├── detail panel on click
       └── dark theme
```

---

## Migration History

| Version | Changes |
|---------|---------|
| v1 | Initial schema: nodes, edges, metadata tables |
| v2 | Add `signature` column to nodes |
| v3 | Create `flows` and `flow_memberships` tables |
| v4 | Add `community_id` to nodes, create `communities` table |
| v5 | Create `nodes_fts` FTS5 virtual table (soft-fail if unavailable) |
| v6 | Create `community_summaries`, `flow_snapshots`, `risk_index` tables |

---

## Security Model

| Threat | Mitigation |
|--------|------------|
| Prompt injection via node names | `SanitizeName()`: strip control chars, cap at 256 chars |
| Command injection via git refs | `safeGitRef` regex validation before `exec.Command` |
| Shell injection | Never use `os/exec` with shell=true; explicit arg lists only |
| Path traversal in refactoring | `filepath.Rel()` validation in `ApplyRefactor` |
| Binary file parsing | `isBinaryFile()` detection (null byte scan) |
| Sensitive directory access | Configurable ignore patterns (`.git`, `node_modules`, etc.) |
| Secret leakage | API keys from env vars only, never hardcoded |
| SQL injection | Parameterised queries (`?` placeholders) everywhere |
