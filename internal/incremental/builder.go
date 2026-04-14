package incremental

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/harshsharma/code-review-graph-go/internal/config"
	"github.com/harshsharma/code-review-graph-go/internal/graph"
	"github.com/harshsharma/code-review-graph-go/internal/parser"
)

type BuildResult struct {
	FilesParsed int      `json:"files_parsed"`
	TotalNodes  int      `json:"total_nodes"`
	TotalEdges  int      `json:"total_edges"`
	Errors      []string `json:"errors,omitempty"`
}

type UpdateResult struct {
	FilesUpdated   int      `json:"files_updated"`
	TotalNodes     int      `json:"total_nodes"`
	TotalEdges     int      `json:"total_edges"`
	ChangedFiles   []string `json:"changed_files"`
	DependentFiles []string `json:"dependent_files"`
	Errors         []string `json:"errors,omitempty"`
}

// FullBuild performs a full rebuild of the entire graph.
func FullBuild(ctx context.Context, repoRoot string, store *graph.Store) (*BuildResult, error) {
	files, err := CollectAllFiles(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("collecting files: %w", err)
	}

	// Purge stale data
	existingFiles, err := store.GetAllFiles()
	if err != nil {
		return nil, fmt.Errorf("getting existing files: %w", err)
	}
	currentSet := make(map[string]struct{}, len(files))
	for _, f := range files {
		currentSet[filepath.Join(repoRoot, f)] = struct{}{}
	}
	for _, ef := range existingFiles {
		if _, ok := currentSet[ef]; !ok {
			_ = store.RemoveFileData(ef)
		}
	}

	result := &BuildResult{}

	if config.SerialParse() || len(files) < 8 {
		result = buildSerial(ctx, repoRoot, store, files)
	} else {
		result = buildParallel(ctx, repoRoot, store, files)
	}

	now := time.Now().Format("2006-01-02T15:04:05")
	_ = store.SetMetadata("last_updated", now)
	_ = store.SetMetadata("last_build_type", "full")

	branch, sha := GitBranchInfo(repoRoot)
	if branch != "" {
		_ = store.SetMetadata("git_branch", branch)
	}
	if sha != "" {
		_ = store.SetMetadata("git_head_sha", sha)
	}

	return result, nil
}

func buildSerial(ctx context.Context, repoRoot string, store *graph.Store, files []string) *BuildResult {
	result := &BuildResult{}
	for i, relPath := range files {
		if ctx.Err() != nil {
			break
		}

		absPath := filepath.Join(repoRoot, relPath)
		data, err := os.ReadFile(absPath)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %s", relPath, err))
			continue
		}

		fhash := parser.FileHash(data)
		p := parser.NewCodeParser()
		nodes, edges, err := p.ParseBytes(ctx, absPath, data)
		p.Close()
		if err != nil {
			slog.Warn("parse error", "file", relPath, "error", err)
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %s", relPath, err))
			continue
		}

		if err := store.StoreFileNodesEdges(absPath, nodes, edges, fhash); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: store error: %s", relPath, err))
			continue
		}

		result.TotalNodes += len(nodes)
		result.TotalEdges += len(edges)
		result.FilesParsed++

		if (i+1)%50 == 0 || i+1 == len(files) {
			slog.Info("build progress", "completed", i+1, "total", len(files))
		}
	}
	return result
}

func buildParallel(ctx context.Context, repoRoot string, store *graph.Store, files []string) *BuildResult {
	result := &BuildResult{}

	jobs := make([]parser.FileJob, len(files))
	for i, f := range files {
		jobs[i] = parser.FileJob{RelPath: f, RepoRoot: repoRoot}
	}

	pool := parser.NewWorkerPool(config.ParseWorkers)
	for pr := range pool.ParseAll(ctx, jobs) {
		if pr.Err != nil {
			slog.Warn("parse error", "file", pr.FilePath, "error", pr.Err)
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %s", pr.FilePath, pr.Err))
			continue
		}

		absPath := filepath.Join(repoRoot, pr.FilePath)
		if err := store.StoreFileNodesEdges(absPath, pr.Nodes, pr.Edges, pr.FileHash); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: store error: %s", pr.FilePath, err))
			continue
		}

		result.TotalNodes += len(pr.Nodes)
		result.TotalEdges += len(pr.Edges)
		result.FilesParsed++
	}

	return result
}

// IncrementalUpdate re-parses only changed and dependent files.
func IncrementalUpdate(ctx context.Context, repoRoot string, store *graph.Store, base string, changedFiles []string) (*UpdateResult, error) {
	if changedFiles == nil {
		var err error
		changedFiles, err = GetChangedFiles(repoRoot, base)
		if err != nil {
			return nil, fmt.Errorf("getting changed files: %w", err)
		}
	}

	if len(changedFiles) == 0 {
		return &UpdateResult{}, nil
	}

	// Find dependent files
	dependentFiles := make(map[string]struct{})
	for _, relPath := range changedFiles {
		absPath := filepath.Join(repoRoot, relPath)
		deps, err := FindDependents(store, absPath)
		if err != nil {
			slog.Warn("error finding dependents", "file", relPath, "error", err)
			continue
		}
		for _, d := range deps {
			rel, err := filepath.Rel(repoRoot, d)
			if err != nil {
				dependentFiles[d] = struct{}{}
			} else {
				dependentFiles[rel] = struct{}{}
			}
		}
	}

	allFiles := make(map[string]struct{}, len(changedFiles)+len(dependentFiles))
	for _, f := range changedFiles {
		allFiles[f] = struct{}{}
	}
	for f := range dependentFiles {
		allFiles[f] = struct{}{}
	}

	ignorePatterns := LoadIgnorePatterns(repoRoot)
	var toParse []string

	for relPath := range allFiles {
		if ShouldIgnore(relPath, ignorePatterns) {
			continue
		}
		absPath := filepath.Join(repoRoot, relPath)
		info, err := os.Stat(absPath)
		if err != nil || !info.Mode().IsRegular() {
			_ = store.RemoveFileData(absPath)
			continue
		}
		if parser.DetectLanguage(absPath) == "" {
			continue
		}
		// Hash check to skip unchanged files
		data, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		fhash := fmt.Sprintf("%x", sha256.Sum256(data))
		nodes, _ := store.GetNodesByFile(absPath)
		if len(nodes) > 0 && nodes[0].FileHash != nil && *nodes[0].FileHash == fhash {
			continue
		}
		toParse = append(toParse, relPath)
	}

	result := &UpdateResult{
		ChangedFiles: changedFiles,
	}
	for f := range dependentFiles {
		result.DependentFiles = append(result.DependentFiles, f)
	}

	if config.SerialParse() || len(toParse) < 8 {
		for _, relPath := range toParse {
			absPath := filepath.Join(repoRoot, relPath)
			data, err := os.ReadFile(absPath)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: %s", relPath, err))
				continue
			}

			fhash := parser.FileHash(data)
			p := parser.NewCodeParser()
			nodes, edges, err := p.ParseBytes(ctx, absPath, data)
			p.Close()
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: %s", relPath, err))
				continue
			}

			if err := store.StoreFileNodesEdges(absPath, nodes, edges, fhash); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: store error: %s", relPath, err))
				continue
			}
			result.TotalNodes += len(nodes)
			result.TotalEdges += len(edges)
		}
	} else {
		jobs := make([]parser.FileJob, len(toParse))
		for i, f := range toParse {
			jobs[i] = parser.FileJob{RelPath: f, RepoRoot: repoRoot}
		}
		pool := parser.NewWorkerPool(config.ParseWorkers)
		for pr := range pool.ParseAll(ctx, jobs) {
			if pr.Err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: %s", pr.FilePath, pr.Err))
				continue
			}
			absPath := filepath.Join(repoRoot, pr.FilePath)
			if err := store.StoreFileNodesEdges(absPath, pr.Nodes, pr.Edges, pr.FileHash); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: store error: %s", pr.FilePath, err))
				continue
			}
			result.TotalNodes += len(pr.Nodes)
			result.TotalEdges += len(pr.Edges)
		}
	}

	result.FilesUpdated = len(allFiles)

	now := time.Now().Format("2006-01-02T15:04:05")
	_ = store.SetMetadata("last_updated", now)
	_ = store.SetMetadata("last_build_type", "incremental")

	branch, sha := GitBranchInfo(repoRoot)
	if branch != "" {
		_ = store.SetMetadata("git_branch", branch)
	}
	if sha != "" {
		_ = store.SetMetadata("git_head_sha", sha)
	}

	return result, nil
}

// CollectAllFiles collects all parseable files in the repo.
func CollectAllFiles(repoRoot string) ([]string, error) {
	ignorePatterns := LoadIgnorePatterns(repoRoot)

	tracked, _ := GetAllTrackedFiles(repoRoot)

	var candidates []string
	if len(tracked) > 0 {
		candidates = tracked
	} else {
		err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return nil
			}
			candidates = append(candidates, rel)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	var files []string
	for _, relPath := range candidates {
		if ShouldIgnore(relPath, ignorePatterns) {
			continue
		}
		absPath := filepath.Join(repoRoot, relPath)
		info, err := os.Lstat(absPath)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if parser.DetectLanguage(absPath) == "" {
			continue
		}
		if isBinary(absPath) {
			continue
		}
		files = append(files, relPath)
	}

	return files, nil
}

// FindDependents finds files that import from or depend on the given file.
func FindDependents(store *graph.Store, filePath string) ([]string, error) {
	maxHops := config.DependentHops
	allDeps := make(map[string]struct{})
	visited := map[string]struct{}{filePath: {}}
	frontier := map[string]struct{}{filePath: {}}

	for hop := 0; hop < maxHops; hop++ {
		nextFrontier := make(map[string]struct{})
		for fp := range frontier {
			deps, err := singleHopDependents(store, fp)
			if err != nil {
				return nil, err
			}
			for d := range deps {
				if _, ok := visited[d]; !ok {
					allDeps[d] = struct{}{}
					nextFrontier[d] = struct{}{}
				}
			}
		}
		for fp := range nextFrontier {
			visited[fp] = struct{}{}
		}
		frontier = nextFrontier
		if len(frontier) == 0 {
			break
		}
		if len(allDeps) > config.MaxDependentFiles {
			break
		}
	}

	result := make([]string, 0, len(allDeps))
	for d := range allDeps {
		result = append(result, d)
	}
	return result, nil
}

func singleHopDependents(store *graph.Store, filePath string) (map[string]struct{}, error) {
	dependents := make(map[string]struct{})

	edges, err := store.GetEdgesByTarget(filePath)
	if err != nil {
		return nil, err
	}
	for _, e := range edges {
		if e.Kind == string(graph.EdgeImportsFrom) {
			dependents[e.FilePath] = struct{}{}
		}
	}

	nodes, err := store.GetNodesByFile(filePath)
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		nodeEdges, err := store.GetEdgesByTarget(node.QualifiedName)
		if err != nil {
			continue
		}
		for _, e := range nodeEdges {
			switch e.Kind {
			case string(graph.EdgeCalls), string(graph.EdgeImportsFrom),
				string(graph.EdgeInherits), string(graph.EdgeImplements):
				dependents[e.FilePath] = struct{}{}
			}
		}
	}

	delete(dependents, filePath)
	return dependents, nil
}

// LoadIgnorePatterns loads ignore patterns from config and .code-review-graphignore.
func LoadIgnorePatterns(repoRoot string) []string {
	patterns := make([]string, len(config.DefaultIgnorePatterns))
	copy(patterns, config.DefaultIgnorePatterns)

	ignoreFile := filepath.Join(repoRoot, ".code-review-graphignore")
	f, err := os.Open(ignoreFile)
	if err != nil {
		return patterns
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			patterns = append(patterns, line)
		}
	}
	return patterns
}

// ShouldIgnore checks if a path matches any ignore pattern.
func ShouldIgnore(path string, patterns []string) bool {
	for _, p := range patterns {
		matched, _ := filepath.Match(p, path)
		if matched {
			return true
		}
		// Handle dir/** patterns at any depth
		if strings.HasSuffix(p, "/**") {
			prefix := strings.TrimSuffix(p, "/**")
			if !strings.Contains(prefix, "/") && prefix != "" {
				parts := strings.Split(path, "/")
				for _, part := range parts {
					if part == prefix {
						return true
					}
				}
			}
		}
		// Handle *.ext patterns
		if strings.HasPrefix(p, "*.") {
			ext := p[1:]
			if strings.HasSuffix(path, ext) {
				return true
			}
		}
	}
	return false
}

func isBinary(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()

	buf := make([]byte, 8192)
	n, err := f.Read(buf)
	if err != nil {
		return true
	}

	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return true
		}
	}
	return false
}
