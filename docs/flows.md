# Execution Flows

Every user-facing operation in code-review-graph-go follows a distinct flow through the system. This document traces each one end-to-end, showing the call chain, concurrency boundaries, and data transformations.

---

## Table of Contents

1. [Full Build Flow](#1-full-build-flow)
2. [Incremental Update Flow](#2-incremental-update-flow)
3. [File Watch Flow](#3-file-watch-flow)
4. [Impact Analysis Flow](#4-impact-analysis-flow)
5. [Hybrid Search Flow](#5-hybrid-search-flow)
6. [Execution Flow Tracing](#6-execution-flow-tracing)
7. [MCP Server Request Flow](#7-mcp-server-request-flow)
8. [MCP Tool Call Flow](#8-mcp-tool-call-flow)
9. [Refactor Preview + Apply Flow](#9-refactor-preview--apply-flow)
10. [Dead Code Detection Flow](#10-dead-code-detection-flow)
11. [Wiki Generation Flow](#11-wiki-generation-flow)
12. [Visualization Flow](#12-visualization-flow)
13. [Skills Installation Flow](#13-skills-installation-flow)
14. [Multi-Repo Search Flow](#14-multi-repo-search-flow)
15. [FTS Index Rebuild Flow](#15-fts-index-rebuild-flow)
16. [Hint Generation Flow](#16-hint-generation-flow)
17. [Prompt Rendering Flow](#17-prompt-rendering-flow)

---

## 1. Full Build Flow

**Trigger:** `code-review-graph build --repo /path` or MCP `build_or_update_graph {full_rebuild: true}`

```
User
 │
 ▼
CLI buildCmd / MCP tool handler
 │
 ├── FindProjectRoot(repoPath)          resolve repo root from cwd or flag
 ├── GetDBPath(repoRoot)                → <repo>/.code-review-graph/graph.db
 ├── graph.NewStore(dbPath)             open SQLite, run migrations v1→v6
 │
 ▼
incremental.FullBuild(ctx, repoRoot, store)
 │
 ├── CollectAllFiles(repoRoot)
 │    ├── GetAllTrackedFiles(repoRoot)  git ls-files (if git repo)
 │    └── fallback: filepath.Walk       (if not a git repo)
 │
 ├── LoadIgnorePatterns(repoRoot)       .code-review-graph/ignore
 │
 ├── Filter files:
 │    ├── ShouldIgnore(path, patterns)  glob matching against ignore list
 │    ├── isBinaryFile(path)            null-byte scan in first 8KB
 │    └── isSymlink(path)               os.Lstat check
 │
 ├── Remove stale files from DB:
 │    store.GetAllFiles() → diff with collected → store.RemoveFileData(stale)
 │
 ├── Build FileJob list for each valid file
 │
 ├─────────────── CONCURRENCY BOUNDARY ─────────────────
 │
 ├── if config.SerialParse():
 │    for each job: ParseSingle(ctx, job) → store.StoreFileNodesEdges
 │
 ├── else: WorkerPool.ParseAll(ctx, jobs)
 │    ┌──────────────────────────────────────────────────┐
 │    │  chan FileJob (buffered = len(jobs))              │
 │    │       │                                          │
 │    │  ┌────▼────┐  ┌────▼────┐  ┌────▼────┐          │
 │    │  │Worker 1 │  │Worker 2 │  │Worker N │          │
 │    │  │CodePars.│  │CodePars.│  │CodePars.│          │
 │    │  └────┬────┘  └────┬────┘  └────┬────┘          │
 │    │       │            │            │                │
 │    │  chan ParseResult (buffered = numWorkers)         │
 │    └──────────────────────────────────────────────────┘
 │
 ├── for result := range resultCh:
 │    ├── if result.Err → log error, continue
 │    └── store.StoreFileNodesEdges(filePath, nodes, edges, hash)
 │         ├── writeMu.Lock()           ← serialised DB writes
 │         ├── BEGIN TRANSACTION
 │         ├── DELETE FROM nodes WHERE file_path = ?
 │         ├── DELETE FROM edges WHERE file_path = ?
 │         ├── INSERT nodes (ON CONFLICT UPDATE)
 │         ├── INSERT edges (lookup or create)
 │         ├── COMMIT
 │         └── writeMu.Unlock()
 │
 ├── EnsureGitignore(repoRoot)          add .code-review-graph/ to .gitignore
 │
 ├── store.SetMetadata("last_updated", now)
 ├── store.SetMetadata("git_branch", branch)
 ├── store.SetMetadata("git_head_sha", sha)
 │
 └── return BuildResult{FilesParsed, TotalNodes, TotalEdges, Errors}
```

**Data volume:** 116 files → ~1,798 nodes, ~10,418 edges in ~1.5s on 8 workers.

---

## 2. Incremental Update Flow

**Trigger:** `code-review-graph update --base HEAD~1` or MCP `build_or_update_graph {full_rebuild: false}`

```
User
 │
 ▼
incremental.IncrementalUpdate(ctx, repoRoot, store, base, changedFiles)
 │
 ├── Resolve changed files:
 │    ├── if changedFiles provided → use directly
 │    ├── else: GetChangedFiles(repoRoot, base)
 │    │    └── git diff --name-only <base> HEAD
 │    └── if empty: GetStagedAndUnstaged(repoRoot)
 │         └── git status --porcelain → parse M/A/? entries
 │
 ├── Expand to dependents (multi-hop BFS):
 │    for each changed file:
 │      FindDependents(store, file)
 │       ├── get edges WHERE source or target qualified starts with file
 │       ├── BFS: follow IMPORTS_FROM / DEPENDS_ON / CALLS edges
 │       │    up to config.DependentHops (default 2) levels
 │       └── collect unique file paths (cap at MaxDependentFiles)
 │
 ├── Merge: changedFiles ∪ dependentFiles → filesToProcess
 │
 ├── For each file in filesToProcess:
 │    ├── read file content
 │    ├── parser.FileHash(content) → sha256
 │    ├── if hash == existing file_hash in DB → SKIP (no change)
 │    └── parse + store (serial or WorkerPool, same as full build)
 │
 ├── Update metadata
 │
 └── return UpdateResult{FilesUpdated, ChangedFiles, DependentFiles, Errors}
```

**Key insight:** Hash-based skip means even if 20 files are in the changed+dependent set, only the truly modified ones get re-parsed.

---

## 3. File Watch Flow

**Trigger:** `code-review-graph watch`

```
Watch(ctx, repoRoot, store)
 │
 ├── fsnotify.NewWatcher()
 │
 ├── Walk all directories under repoRoot:
 │    ├── skip: .git, node_modules, __pycache__, .code-review-graph
 │    └── watcher.Add(dir)
 │
 └── Event loop (blocking):
      │
      ├── case event := <-watcher.Events:
      │    ├── filter: ignore non-source files, directories, binaries
      │    │
      │    ├── DEBOUNCE: time.AfterFunc(300ms, handler)
      │    │    └── coalesces rapid saves into a single update
      │    │
      │    ├── CREATE / WRITE event:
      │    │    └── parser.ParseFileToStore(ctx, store, absPath, source)
      │    │         ├── ParseBytes(ctx, path, source) → nodes, edges
      │    │         └── store.StoreFileNodesEdges(path, nodes, edges, hash)
      │    │
      │    └── REMOVE event:
      │         └── store.RemoveFileData(relPath)
      │
      ├── case err := <-watcher.Errors:
      │    └── log error, continue
      │
      └── case <-ctx.Done():
           └── return (clean shutdown)
```

---

## 4. Impact Analysis Flow

**Trigger:** `detect-changes --brief` or MCP `get_impact_radius`, `detect_changes`, `get_review_context`

```
store.GetImpactRadius(changedFiles, maxDepth=2, maxNodes=500)
 │
 ├── Seed: find all nodes in changed files
 │    SELECT qualified_name FROM nodes WHERE file_path IN (...)
 │    → INSERT INTO temporary table _impact_seeds
 │
 ├── Recursive CTE (breadth-first expansion):
 │    WITH RECURSIVE impact(qualified_name, depth) AS (
 │      SELECT qualified_name, 0 FROM _impact_seeds
 │      UNION
 │      SELECT e.source_qualified, i.depth + 1
 │      FROM edges e
 │      JOIN impact i ON e.target_qualified = i.qualified_name
 │      WHERE i.depth < maxDepth
 │    )
 │    SELECT DISTINCT qualified_name FROM impact LIMIT maxNodes
 │
 ├── Partition results:
 │    ├── changedNodes: nodes directly in changed files
 │    └── impactedNodes: nodes reachable via edges (not in changed files)
 │
 ├── Collect connecting edges:
 │    GetEdgesAmong(allQualifiedNames)  → edges between changed + impacted
 │
 ├── Extract unique impacted file paths
 │
 └── return ImpactResult{
       ChangedNodes, ImpactedNodes, ImpactedFiles,
       Edges, Truncated (if hit maxNodes), TotalImpacted
     }
```

---

## 5. Hybrid Search Flow

**Trigger:** `code-review-graph search <query>` or MCP `semantic_search_nodes`

```
search.HybridSearch(store, query, kind, limit, contextFiles, embStore)
 │
 ├── Phase 1: Gather ranked result lists (fetchLimit = limit * 3)
 │    │
 │    ├── ftsSearch(db, query, fetchLimit)
 │    │    ├── sanitise: wrap query in double-quotes (prevent FTS5 injection)
 │    │    ├── SELECT rowid, rank FROM nodes_fts
 │    │    │   WHERE nodes_fts MATCH ? ORDER BY rank LIMIT ?
 │    │    └── negate rank (FTS5 returns negative BM25) → higher = better
 │    │    returns: []idScore{nodeID, bm25Score}
 │    │
 │    └── embeddingSearch(store, embStore, query, fetchLimit)
 │         ├── if embStore nil or unavailable → return nil
 │         ├── embStore.Search(query, fetchLimit)
 │         │    ├── provider.EmbedQuery(query)   → float32 vector
 │         │    ├── scan ALL rows in embeddings table
 │         │    ├── decodeVector(blob) for each
 │         │    ├── cosineSimilarity(queryVec, storedVec)
 │         │    └── sort, return top-K
 │         └── map qualifiedName → nodeID via store.GetNode
 │         returns: []idScore{nodeID, similarityScore}
 │
 ├── Phase 2: Merge or fallback
 │    ├── if FTS or embedding results exist:
 │    │    rrfMerge(ftsResults, embResults)
 │    │    └── for each list:
 │    │         for rank, item in list:
 │    │           scores[item.id] += 1.0 / (60 + rank + 1)
 │    │         sort by RRF score descending
 │    │
 │    └── else: keywordSearch(db, query, fetchLimit)
 │         ├── split query into words
 │         ├── AND-join: LOWER(name) LIKE %word% OR LOWER(qualified_name) LIKE %word%
 │         └── score: exact=3.0, prefix=2.0, contains=1.0
 │
 ├── Phase 3: Batch-fetch candidate nodes
 │    ├── collect all candidate IDs from merged results
 │    └── SELECT ... FROM nodes WHERE id IN (?) — batches of 450
 │         → nodeRows map[int64]nodeRow
 │
 ├── Phase 4: Apply boosting
 │    ├── detectQueryKindBoost(query):
 │    │    ├── PascalCase (e.g. "UserService") → Class×1.5, Type×1.5
 │    │    ├── snake_case (e.g. "get_user")    → Function×1.5
 │    │    └── dotted (e.g. "auth.User")       → qualified×2.0
 │    │
 │    ├── for each candidate:
 │    │    boost = 1.0
 │    │    if node.kind in kindBoosts → boost *= kindBoosts[kind]
 │    │    if query has dot AND qualified_name contains query → boost *= 2.0
 │    │    if file in contextFiles → boost *= 1.5
 │    │    finalScore = rrfScore * boost
 │    │
 │    └── sort by finalScore descending
 │
 └── Phase 5: Build results
      ├── filter: if kind != "" AND node.kind != kind → skip
      ├── cap at limit
      └── return []Result{Name, QualifiedName, Kind, FilePath, Score, ...}
```

---

## 6. Execution Flow Tracing

**Trigger:** `code-review-graph postprocess` or MCP `list_flows`

```
DetectEntryPoints(store)
 │
 ├── SQL: SELECT target_qualified FROM edges WHERE kind = 'CALLS'
 │    → callTargets set (nodes that are called by something)
 │
 ├── SQL: SELECT * FROM nodes WHERE kind IN ('Function', 'Test')
 │    → all function/test candidates
 │
 └── Filter: keep node if ANY of:
      ├── node.qualified_name NOT IN callTargets     (no incoming calls = root)
      ├── HasFrameworkDecorator(node)                 (route, endpoint, handler, etc.)
      └── MatchesEntryName(node)                      (main, run, setup, test_, etc.)
      → returns []GraphNode (entry points)

TraceFlows(store, maxDepth=15)
 │
 ├── entries := DetectEntryPoints(store)
 │
 └── for each entry:
      ├── BFS initialisation:
      │    queue := [entry.QualifiedName]
      │    visited := {entry.QualifiedName}
      │    path := [entry]
      │
      ├── BFS loop (depth ≤ maxDepth):
      │    for level = 0; level < maxDepth && len(queue) > 0:
      │      nextQueue := []
      │      for each node in queue:
      │        edges := store.GetEdgesBySource(node)  — outgoing CALLS
      │        for each edge:
      │          if edge.target NOT in visited:
      │            visited.Add(edge.target)
      │            nextQueue.Append(edge.target)
      │            targetNode := store.GetNode(edge.target)
      │            path.Append(targetNode)
      │      queue = nextQueue
      │
      ├── Collect metadata:
      │    ├── files := unique file paths from path nodes
      │    ├── depth := max BFS level reached
      │    └── nodeCount, fileCount
      │
      ├── computeCriticality(flow):
      │    score = weighted sum of:
      │    ├── file_spread     (0.25) = fileCount / max(totalFiles, 1)
      │    ├── external_calls  (0.20) = cross-file edges / total edges
      │    ├── security_touch  (0.20) = any node name ∈ SecurityKeywords?
      │    ├── test_gap        (0.20) = untested nodes / total nodes
      │    └── depth_factor    (0.15) = depth / maxDepth
      │    → clamp to [0.0, 1.0]
      │
      └── Flow{Name, EntryPoint, Path, Depth, NodeCount, FileCount, Criticality}

StoreFlows(store, flows)
 ├── DELETE FROM flows; DELETE FROM flow_memberships
 └── for each flow:
      INSERT INTO flows(name, entry_point_id, depth, node_count, file_count,
                        criticality, path_json)
      for i, nodeQN in flow.Path:
        INSERT INTO flow_memberships(flow_id, node_id, position)
```

---

## 7. MCP Server Request Flow

**Trigger:** AI tool connects to `code-review-graph serve` via stdio

```
Server.Run(ctx)
 │
 └── loop:
      ├── Read line from stdin
      │    ├── Parse "Content-Length: N\r\n" header
      │    ├── Read N bytes of JSON body
      │    └── json.Unmarshal → jsonRPCRequest{JSONRPC, ID, Method, Params}
      │
      ├── handleRequest(ctx, req):
      │    │
      │    ├── "initialize"
      │    │    └── respond: protocolVersion, serverInfo, capabilities{tools, prompts}
      │    │
      │    ├── "initialized"
      │    │    └── log, no response (notification)
      │    │
      │    ├── "tools/list"
      │    │    └── registry.AllTools() → [{name, description, inputSchema}, ...]
      │    │
      │    ├── "tools/call"
      │    │    └── → see Tool Call Flow below
      │    │
      │    ├── "prompts/list"
      │    │    └── prompts.AllPrompts() → [{name, description, arguments}, ...]
      │    │
      │    ├── "prompts/get"
      │    │    └── find prompt by name → Handler(args) → [{role, content}, ...]
      │    │
      │    ├── "ping"
      │    │    └── respond: {}
      │    │
      │    └── unknown method
      │         └── if has ID: error -32601 (method not found)
      │
      └── Write response:
           server.mu.Lock()      ← serialise stdout writes
           fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len, json)
           server.mu.Unlock()
```

---

## 8. MCP Tool Call Flow

**Trigger:** MCP `tools/call` request

```
handleToolsCall(ctx, params)
 │
 ├── Extract tool name and arguments from params
 │
 ├── Lookup: toolMap[name]
 │    └── if not found → error -32602 (invalid params)
 │
 ├── Execute: tool.Handler(ctx, arguments)
 │    └── dispatches to the specific tool implementation
 │         (e.g. search.HybridSearch, flows.GetFlows, etc.)
 │    → returns (result any, err error)
 │
 ├── if err → wrap as JSON text: {"error": "message"}
 │
 ├── if result is map[string]any:
 │    ├── hints.GenerateHints(toolName, resultMap, session)
 │    │    ├── session.RecordToolCall(toolName)
 │    │    ├── session.InferIntent()
 │    │    ├── buildNextSteps(toolName, session)
 │    │    ├── extractWarnings(resultMap)
 │    │    └── buildRelated(resultMap, session)
 │    └── resultMap["_hints"] = hints
 │
 ├── json.Marshal(result)
 │
 └── respond: {content: [{type: "text", text: jsonString}]}
```

---

## 9. Refactor Preview + Apply Flow

**Trigger:** MCP `refactor {operation: "rename", old_name: "X", new_name: "Y"}`

```
refactor.RenamePreview(store, oldName, newName)
 │
 ├── store.SearchNodes(oldName, 50)
 │    └── keyword search → candidate nodes
 │
 ├── Find best match:
 │    ├── exact qualified_name match → use it
 │    └── else: first name match → use it
 │    → target GraphNode
 │
 ├── Build Edit list:
 │    ├── Edit{File: target.FilePath, Line: target.LineStart,
 │    │        Old: oldName, New: newName, Confidence: "high"}
 │    │
 │    ├── store.GetEdgesByTarget(target.QualifiedName)
 │    │    filter: CALLS edges
 │    │    for each caller:
 │    │      Edit{File: edge.FilePath, Line: edge.Line,
 │    │           Old: oldName, New: newName, Confidence: "high"}
 │    │
 │    └── IMPORTS_FROM edges → Edit{..., Confidence: "medium"}
 │
 ├── genID() → "rfct_<8-hex-chars>"
 │
 ├── pendingMu.Lock()
 │    pending[refactorID] = Preview{Edits, CreatedAt: now}
 │    cleanupExpired()     remove entries older than 600s
 │    pendingMu.Unlock()
 │
 └── return Preview{RefactorID, Type: "rename", OldName, NewName, Edits, Stats}

─── Later ───

refactor.ApplyRefactor(refactorID, repoRoot, dryRun)
 │
 ├── pendingMu.Lock()
 │    preview := pending[refactorID]
 │    pendingMu.Unlock()
 │    └── if not found or expired → return {status: "error"}
 │
 ├── for each edit in preview.Edits:
 │    ├── absPath := filepath.Join(repoRoot, edit.File)
 │    ├── validate: filepath.Rel(repoRoot, absPath) must not start with ".."
 │    ├── read file content
 │    ├── replaced := strings.Replace(content, edit.Old, edit.New, 1)
 │    ├── if dryRun → collect diff only
 │    └── else → os.WriteFile(absPath, replaced, 0644)
 │
 ├── delete pending[refactorID]
 │
 └── return {status: "applied"|"dry_run", applied: N, skipped: M, diffs: [...]}
```

---

## 10. Dead Code Detection Flow

**Trigger:** MCP `find_dead_code` or `refactor {operation: "dead_code"}`

```
refactor.FindDeadCode(store, kind, filePattern)
 │
 ├── SQL: SELECT * FROM nodes WHERE kind IN ('Function', 'Class')
 │    optional: AND kind = ?kind
 │    optional: AND file_path LIKE %filePattern%
 │    → candidates
 │
 ├── for each candidate:
 │    ├── SKIP if node.IsTest          (test code is allowed to be "uncalled")
 │    ├── SKIP if HasFrameworkDecorator (routes, handlers are entry points)
 │    ├── SKIP if MatchesEntryName     (main, setup, run, etc.)
 │    │
 │    ├── Check incoming CALLS:
 │    │    store.GetEdgesByTarget(node.QualifiedName)
 │    │    filter: kind == "CALLS"
 │    │    → if any exist → NOT dead code → skip
 │    │
 │    ├── Check TESTED_BY:
 │    │    filter: kind == "TESTED_BY"
 │    │    → if any exist → NOT dead code → skip
 │    │
 │    ├── Check IMPORTS_FROM:
 │    │    filter: kind == "IMPORTS_FROM"
 │    │    → if any exist → NOT dead code → skip
 │    │
 │    └── No references at all → DEAD CODE
 │         append to results: {name, qualified_name, kind, file, line}
 │
 └── return []map[string]any (dead code entries)
```

---

## 11. Wiki Generation Flow

**Trigger:** `code-review-graph wiki` or MCP `generate_wiki`

```
wiki.GenerateWiki(store, wikiDir)
 │
 ├── os.MkdirAll(wikiDir, 0755)
 │
 ├── GetCommunities(store)
 │    └── SELECT c.*, GROUP_CONCAT(n.qualified_name)
 │        FROM communities c
 │        LEFT JOIN nodes n ON n.community_id = c.id
 │        GROUP BY c.id
 │    → []Community{ID, Name, Size, Members, DominantLanguage, ...}
 │
 ├── for each community:
 │    ├── slug := slugify(community.Name)
 │    │    └── lowercase, replace non-alphanum with "-", dedup dashes
 │    │        if collision → append "-2", "-3", etc.
 │    │
 │    ├── content := generateCommunityPage(store, community)
 │    │    ├── "# <Name>"
 │    │    ├── Description
 │    │    ├── "## Members" table:
 │    │    │    for each member (top 50 by line count):
 │    │    │      | Name | Kind | File | Lines |
 │    │    ├── "## Execution Flows"
 │    │    │    flows.GetFlows(store) → filter where flow path
 │    │    │    touches any community member
 │    │    └── "## Dependencies"
 │    │         cross-community edges → links to other wiki pages
 │    │
 │    ├── if file exists AND content unchanged → PagesUnchanged++
 │    ├── if file exists AND content changed   → PagesUpdated++
 │    └── else write new file                  → PagesGenerated++
 │
 ├── Generate index.md:
 │    ├── "# Code Wiki"
 │    └── for each community:
 │         "- [<Name>](<slug>.md) — <size> members, <language>"
 │
 └── return GenerateResult{PagesGenerated, PagesUpdated, PagesUnchanged}
```

---

## 12. Visualization Flow

**Trigger:** `code-review-graph visualize [--serve]` or MCP `visualize_graph`

```
visualization.GenerateHTML(store, outputPath)
 │
 ├── ExportGraphData(store)
 │    ├── get all File-kind nodes → deduplicate qualified names
 │    ├── get all non-File nodes in those files
 │    ├── getAllEdges(store)
 │    │    SELECT * FROM edges → resolve both endpoints
 │    │    filter: keep only edges where both source and target exist
 │    ├── compute stats:
 │    │    nodesByKind, edgesByKind, languageCounts
 │    └── return exportData{Nodes, Edges, Stats}
 │
 ├── json.Marshal(exportData) → jsonBytes
 │
 ├── Embed into D3.js HTML template:
 │    ├── <script> const graphData = <jsonBytes>; </script>
 │    ├── Force-directed simulation (d3.forceSimulation)
 │    ├── Node rendering:
 │    │    ├── color by kind (Function=blue, Class=green, File=gray, etc.)
 │    │    ├── size by line count
 │    │    └── shape: circle (function), square (class), diamond (type)
 │    ├── Edge rendering:
 │    │    ├── color by kind (CALLS=orange, IMPORTS=blue, CONTAINS=gray)
 │    │    └── toggleable per kind
 │    ├── Search: highlight matching nodes, fade others
 │    ├── Click: detail panel with node metadata + connections
 │    └── Dark theme with CSS variables
 │
 └── os.WriteFile(outputPath, html, 0644)

Optional --serve:
  http.Handle("/", http.FileServer(dataDir))
  http.ListenAndServe(":8765", nil)
```

---

## 13. Skills Installation Flow

**Trigger:** `code-review-graph install [--platform all]`

```
skills.FullInstall(repoRoot, platform)
 │
 ├── Step 1: InstallPlatformConfigs(repoRoot, platform, dryRun=false)
 │    │
 │    ├── Resolve targets:
 │    │    if platform == "all" → all 6 platforms
 │    │    else → single platform
 │    │
 │    └── for each Platform{Name, ConfigPath, Detect, Format}:
 │         ├── if !Detect() → skip (platform not installed)
 │         ├── configPath := Platform.ConfigPath(repoRoot)
 │         ├── read existing config (JSON/TOML/etc.)
 │         ├── inject MCP server entry:
 │         │    "code-review-graph": {
 │         │      "command": "code-review-graph",
 │         │      "args": ["serve", "--repo", repoRoot],
 │         │      "env": {"CRG_REPO_ROOT": repoRoot}
 │         │    }
 │         └── write updated config
 │
 │    Platforms:
 │    ├── claude:    ~/.claude.json
 │    ├── cursor:    .cursor/mcp.json
 │    ├── windsurf:  ~/.codeium/windsurf/mcp_config.json
 │    ├── zed:       ~/.config/zed/settings.json
 │    ├── continue:  ~/.continue/config.json
 │    └── opencode:  ~/.config/opencode/config.json
 │
 ├── Step 2: GenerateSkills(repoRoot)
 │    └── write to .code-review-graph/skills/:
 │         ├── explore-codebase.md
 │         ├── review-changes.md
 │         ├── debug-issue.md
 │         └── refactor-safely.md
 │
 ├── Step 3: InstallHooks(repoRoot)
 │    └── write .claude/settings.json:
 │         {"permissions": {"allow": ["code-review-graph *"]}}
 │
 ├── Step 4: InstallGitHook(repoRoot)
 │    └── write .git/hooks/pre-commit:
 │         #!/bin/sh
 │         code-review-graph update --repo <repoRoot> 2>/dev/null || true
 │
 ├── Step 5: InjectClaudeMD(repoRoot)
 │    └── append/replace marked section in CLAUDE.md:
 │         <!-- code-review-graph:start -->
 │         ... graph tool instructions ...
 │         <!-- code-review-graph:end -->
 │
 ├── Step 6: InjectPlatformInstructions(repoRoot, platform)
 │    └── append to .cursorrules / .windsurfrules / etc.
 │
 └── Step 7: exec.Command("code-review-graph", "build", "--repo", repoRoot)
      └── initial graph build
```

---

## 14. Multi-Repo Search Flow

**Trigger:** MCP cross-repo search (via registry)

```
registry.CrossRepoSearch(reg, query, limit)
 │
 ├── reg.ListRepos() → []RepoEntry{Path, Alias}
 │
 ├── for each repo:
 │    ├── dbPath := <repo.Path>/.code-review-graph/graph.db
 │    ├── store := openGraphStore(dbPath)
 │    │    └── graph.NewStore(dbPath)
 │    │
 │    ├── store.SearchNodes(query, limit) → nodes
 │    │
 │    ├── for each node:
 │    │    result := {name, qualified_name, kind, file,
 │    │              repo_path: repo.Path, repo_alias: repo.Alias}
 │    │
 │    └── store.Close()
 │
 └── return merged results (first `limit` across all repos)
```

---

## 15. FTS Index Rebuild Flow

**Trigger:** MCP `rebuild_fts_index` or `search --rebuild-index`

```
search.RebuildFTSIndex(store)
 │
 ├── DROP TABLE IF EXISTS nodes_fts
 │
 ├── CREATE VIRTUAL TABLE nodes_fts USING fts5(
 │      name, qualified_name, file_path, signature,
 │      tokenize='porter unicode61'
 │    )
 │
 ├── INSERT INTO nodes_fts(rowid, name, qualified_name, file_path, signature)
 │    SELECT id, name, qualified_name, file_path, COALESCE(signature, '')
 │    FROM nodes
 │
 └── return count (rows indexed)
```

---

## 16. Hint Generation Flow

**Trigger:** Automatically after every MCP `tools/call` response

```
hints.GenerateHints(toolName, result, session)
 │
 ├── session.RecordToolCall(toolName)
 │    └── append to toolHistory (ring buffer of last 10)
 │
 ├── session.InferIntent()
 │    └── match toolHistory against intentTools patterns:
 │         "reviewing":   {get_review_context, detect_changes, get_impact_radius}
 │         "debugging":   {query_graph, semantic_search_nodes, get_flow}
 │         "refactoring": {refactor, find_dead_code, apply_refactor}
 │         "exploring":   {get_minimal_context, list_graph_stats, list_flows}
 │         → set session.InferredIntent
 │
 ├── nextSteps := buildNextSteps(toolName, session)
 │    └── workflow map lookup:
 │         build_or_update_graph → [get_minimal_context, list_flows]
 │         get_impact_radius     → [get_review_context, get_affected_flows]
 │         semantic_search_nodes → [query_graph, get_impact_radius]
 │         refactor              → [apply_refactor]
 │         ... etc.
 │
 ├── warnings := extractWarnings(result)
 │    ├── if result["truncated"] == true → "Results truncated"
 │    ├── if result["error"] exists      → surface error text
 │    └── if count > 100                 → "Large result set"
 │
 ├── related := buildRelated(result, session)
 │    └── extract file_path, qualified_name from result → suggest related tools
 │
 ├── trackResult(result, session)
 │    └── record files and nodes into session state
 │
 └── return Hints{NextSteps: [...], Related: [...], Warnings: [...]}
```

---

## 17. Prompt Rendering Flow

**Trigger:** MCP `prompts/get {name: "review_changes", arguments: {base: "HEAD~3"}}`

```
handlePromptsGet(params)
 │
 ├── find prompt by name in prompts.AllPrompts()
 │
 ├── prompt.Handler(args) → []PromptMessage
 │    │
 │    ├── "review_changes"(base):
 │    │    └── role:"user", content: token-efficiency preamble +
 │    │        "1. build_or_update_graph(base=<base>)
 │    │         2. detect_changes(base=<base>)
 │    │         3. For each high-risk file: get_impact_radius
 │    │         4. Synthesise review summary"
 │    │
 │    ├── "architecture_map"(scope):
 │    │    └── "1. get_minimal_context
 │    │         2. list_flows(sort_by=criticality)
 │    │         3. For top flows: get_flow
 │    │         4. Build architecture diagram"
 │    │
 │    ├── "debug_issue"(symptom, file):
 │    │    └── "1. semantic_search_nodes(query=<symptom>)
 │    │         2. query_graph(dependents_of=<file>)
 │    │         3. get_flow for affected flows
 │    │         4. Narrow down root cause"
 │    │
 │    ├── "onboard_developer"(area):
 │    │    └── "1. get_minimal_context
 │    │         2. semantic_search_nodes(query=<area>)
 │    │         3. list_flows for area
 │    │         4. Build onboarding guide"
 │    │
 │    └── "pre_merge_check"(branch):
 │         └── "1. build_or_update_graph
 │              2. detect_changes(base=main)
 │              3. find_dead_code
 │              4. get_affected_flows
 │              5. Generate merge checklist"
 │
 └── respond: {messages: [{role, content}]}
```
