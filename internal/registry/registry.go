package registry

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

var (
	defaultDir  = filepath.Join(homeDir(), ".code-review-graph")
	defaultPath = filepath.Join(defaultDir, "registry.json")
)

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	return h
}

// RepoEntry represents a registered repository.
type RepoEntry struct {
	Path  string `json:"path"`
	Alias string `json:"alias,omitempty"`
}

// Registry manages a JSON-based registry of code-review-graph repositories.
type Registry struct {
	path  string
	mu    sync.Mutex
	repos []RepoEntry
}

type registryFile struct {
	Repos []RepoEntry `json:"repos"`
}

// New creates or loads a registry from the given path. Pass "" for the default location.
func New(path string) *Registry {
	if path == "" {
		path = defaultPath
	}
	r := &Registry{path: path}
	r.load()
	return r
}

func (r *Registry) load() {
	data, err := os.ReadFile(r.path)
	if err != nil {
		r.repos = nil
		return
	}
	var f registryFile
	if err := json.Unmarshal(data, &f); err != nil {
		slog.Warn("invalid registry file, starting fresh", "path", r.path, "err", err)
		r.repos = nil
		return
	}
	r.repos = f.Repos
}

func (r *Registry) save() error {
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(registryFile{Repos: r.repos}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.path, append(data, '\n'), 0o644)
}

// Register adds a repository path to the registry.
func (r *Registry) Register(path, alias string) (*RepoEntry, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving path: %w", err)
	}

	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("path is not a directory: %s", resolved)
	}

	// Validate it looks like a repo
	gitDir := filepath.Join(resolved, ".git")
	crgDir := filepath.Join(resolved, ".code-review-graph")
	if _, err := os.Stat(gitDir); err != nil {
		if _, err := os.Stat(crgDir); err != nil {
			return nil, fmt.Errorf("path does not look like a repository (no .git or .code-review-graph): %s", resolved)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check for duplicate
	for i, e := range r.repos {
		if e.Path == resolved {
			if alias != "" {
				r.repos[i].Alias = alias
				r.save() //nolint:errcheck
			}
			return &r.repos[i], nil
		}
	}

	entry := RepoEntry{Path: resolved, Alias: alias}
	r.repos = append(r.repos, entry)
	if err := r.save(); err != nil {
		return nil, fmt.Errorf("saving registry: %w", err)
	}
	return &entry, nil
}

// Unregister removes a repository by path or alias.
func (r *Registry) Unregister(pathOrAlias string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	resolved, _ := filepath.Abs(pathOrAlias)
	original := len(r.repos)
	filtered := make([]RepoEntry, 0, len(r.repos))
	for _, e := range r.repos {
		if e.Path != resolved && e.Alias != pathOrAlias {
			filtered = append(filtered, e)
		}
	}

	if len(filtered) < original {
		r.repos = filtered
		r.save() //nolint:errcheck
		return true
	}
	return false
}

// ListRepos returns all registered repositories.
func (r *Registry) ListRepos() []RepoEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]RepoEntry, len(r.repos))
	copy(result, r.repos)
	return result
}

// FindByAlias looks up a repository by its alias.
func (r *Registry) FindByAlias(alias string) *RepoEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.repos {
		if e.Alias == alias {
			entry := e
			return &entry
		}
	}
	return nil
}

// FindByPath looks up a repository by its path.
func (r *Registry) FindByPath(path string) *RepoEntry {
	resolved, _ := filepath.Abs(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.repos {
		if e.Path == resolved {
			entry := e
			return &entry
		}
	}
	return nil
}

// ResolveRepo resolves a repo parameter (alias or path) to an absolute path.
func ResolveRepo(reg *Registry, repo, cwd string) string {
	if repo != "" {
		if entry := reg.FindByAlias(repo); entry != nil {
			return entry.Path
		}
		abs, err := filepath.Abs(repo)
		if err == nil {
			if info, err := os.Stat(abs); err == nil && info.IsDir() {
				return abs
			}
		}
	}
	if cwd != "" {
		abs, _ := filepath.Abs(cwd)
		return abs
	}
	return ""
}

// CrossRepoSearch searches for code entities across all registered repos.
func CrossRepoSearch(reg *Registry, query string, limit int) ([]map[string]any, error) {
	repos := reg.ListRepos()
	var allResults []map[string]any

	for _, repo := range repos {
		dbPath := filepath.Join(repo.Path, ".code-review-graph", "graph.db")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}

		store, err := graph_open(dbPath)
		if err != nil {
			slog.Warn("cross-repo: failed to open store", "path", dbPath, "err", err)
			continue
		}

		nodes, err := store.SearchNodes(query, limit)
		if err != nil {
			store.Close()
			continue
		}

		for _, n := range nodes {
			d := graphNodeToDict(n)
			d["repo_path"] = repo.Path
			d["repo_alias"] = repo.Alias
			allResults = append(allResults, d)
		}
		store.Close()
	}

	if len(allResults) > limit {
		allResults = allResults[:limit]
	}
	return allResults, nil
}

// These use the graph package indirectly to avoid circular imports.
// We access them through a thin adapter layer.

type graphStore interface {
	SearchNodes(query string, limit int) ([]graphNode, error)
	Close() error
}

type graphNode struct {
	ID            int64
	Kind          string
	Name          string
	QualifiedName string
	FilePath      string
	LineStart     int
	LineEnd       int
	Language      string
}

// graph_open opens a graph store by path. Uses the graph package.
var graph_open = func(dbPath string) (*graphStoreAdapter, error) {
	store, err := openGraphStore(dbPath)
	if err != nil {
		return nil, err
	}
	return &graphStoreAdapter{store: store}, nil
}

type graphStoreAdapter struct {
	store interface {
		SearchNodes(query string, limit int) ([]graphAdapterNode, error)
		Close() error
	}
}

type graphAdapterNode = graphNode

func (a *graphStoreAdapter) SearchNodes(query string, limit int) ([]graphNode, error) {
	return a.store.SearchNodes(query, limit)
}

func (a *graphStoreAdapter) Close() error {
	return a.store.Close()
}

func graphNodeToDict(n graphNode) map[string]any {
	return map[string]any{
		"name":           n.Name,
		"qualified_name": n.QualifiedName,
		"kind":           n.Kind,
		"file_path":      n.FilePath,
		"line_start":     n.LineStart,
		"line_end":       n.LineEnd,
		"language":       n.Language,
	}
}

// openGraphStore is set by the init function in the adapter package to avoid circular imports.
// For now it returns an error — the adapter must be wired in main.
var openGraphStore = func(dbPath string) (interface {
	SearchNodes(query string, limit int) ([]graphAdapterNode, error)
	Close() error
}, error) {
	return nil, fmt.Errorf("graph store adapter not initialized")
}
