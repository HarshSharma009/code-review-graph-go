package graph

import (
	"os"
	"path/filepath"
	"testing"
)

func tempDB(t *testing.T) (*Store, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	return store, func() {
		store.Close()
		os.RemoveAll(dir)
	}
}

func TestNewStore(t *testing.T) {
	t.Parallel()
	store, cleanup := tempDB(t)
	defer cleanup()

	if store.db == nil {
		t.Fatal("db is nil")
	}
}

func TestUpsertAndGetNode(t *testing.T) {
	t.Parallel()
	store, cleanup := tempDB(t)
	defer cleanup()

	node := NodeInfo{
		Kind:     "Function",
		Name:     "main",
		FilePath: "cmd/main.go",
		LineStart: 10,
		LineEnd:   20,
		Language:  "go",
	}

	id, err := store.UpsertNode(node, "abc123")
	if err != nil {
		t.Fatalf("upsert node: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	got, err := store.GetNode("cmd/main.go::main")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got == nil {
		t.Fatal("node not found")
	}
	if got.Name != "main" {
		t.Errorf("expected name 'main', got %q", got.Name)
	}
	if got.Kind != "Function" {
		t.Errorf("expected kind 'Function', got %q", got.Kind)
	}
}

func TestUpsertAndGetEdge(t *testing.T) {
	t.Parallel()
	store, cleanup := tempDB(t)
	defer cleanup()

	edge := EdgeInfo{
		Kind:     "CALLS",
		Source:   "a.go::main",
		Target:   "b.go::helper",
		FilePath: "a.go",
		Line:     15,
	}

	id, err := store.UpsertEdge(edge)
	if err != nil {
		t.Fatalf("upsert edge: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	edges, err := store.GetEdgesBySource("a.go::main")
	if err != nil {
		t.Fatalf("get edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].TargetQualified != "b.go::helper" {
		t.Errorf("expected target 'b.go::helper', got %q", edges[0].TargetQualified)
	}
}

func TestStoreFileNodesEdges(t *testing.T) {
	t.Parallel()
	store, cleanup := tempDB(t)
	defer cleanup()

	nodes := []NodeInfo{
		{Kind: "File", Name: "main.go", FilePath: "main.go", LineStart: 1, LineEnd: 50, Language: "go"},
		{Kind: "Function", Name: "main", FilePath: "main.go", LineStart: 10, LineEnd: 20, Language: "go"},
	}
	edges := []EdgeInfo{
		{Kind: "CONTAINS", Source: "main.go", Target: "main.go::main", FilePath: "main.go", Line: 10},
	}

	if err := store.StoreFileNodesEdges("main.go", nodes, edges, "hash123"); err != nil {
		t.Fatalf("store file data: %v", err)
	}

	fileNodes, err := store.GetNodesByFile("main.go")
	if err != nil {
		t.Fatalf("get nodes by file: %v", err)
	}
	if len(fileNodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(fileNodes))
	}
}

func TestGetStats(t *testing.T) {
	t.Parallel()
	store, cleanup := tempDB(t)
	defer cleanup()

	nodes := []NodeInfo{
		{Kind: "File", Name: "app.py", FilePath: "app.py", LineStart: 1, LineEnd: 100, Language: "python"},
		{Kind: "Function", Name: "run", FilePath: "app.py", LineStart: 10, LineEnd: 30, Language: "python"},
		{Kind: "Class", Name: "App", FilePath: "app.py", LineStart: 40, LineEnd: 90, Language: "python"},
	}
	edges := []EdgeInfo{
		{Kind: "CONTAINS", Source: "app.py", Target: "app.py::run", FilePath: "app.py"},
		{Kind: "CALLS", Source: "app.py::run", Target: "app.py::App", FilePath: "app.py"},
	}

	if err := store.StoreFileNodesEdges("app.py", nodes, edges, "hash"); err != nil {
		t.Fatalf("store: %v", err)
	}

	stats, err := store.GetStats()
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}

	if stats.TotalNodes != 3 {
		t.Errorf("expected 3 nodes, got %d", stats.TotalNodes)
	}
	if stats.TotalEdges != 2 {
		t.Errorf("expected 2 edges, got %d", stats.TotalEdges)
	}
	if stats.FilesCount != 1 {
		t.Errorf("expected 1 file, got %d", stats.FilesCount)
	}
}

func TestSearchNodes(t *testing.T) {
	t.Parallel()
	store, cleanup := tempDB(t)
	defer cleanup()

	nodes := []NodeInfo{
		{Kind: "Function", Name: "handleRequest", FilePath: "handler.go", LineStart: 1, LineEnd: 10, Language: "go"},
		{Kind: "Function", Name: "processData", FilePath: "data.go", LineStart: 1, LineEnd: 10, Language: "go"},
	}

	if err := store.StoreFileNodesEdges("handler.go", nodes[:1], nil, "h1"); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreFileNodesEdges("data.go", nodes[1:], nil, "h2"); err != nil {
		t.Fatal(err)
	}

	results, err := store.SearchNodes("handle", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Name != "handleRequest" {
		t.Errorf("expected 'handleRequest', got %q", results[0].Name)
	}
}

func TestRemoveFileData(t *testing.T) {
	t.Parallel()
	store, cleanup := tempDB(t)
	defer cleanup()

	nodes := []NodeInfo{
		{Kind: "File", Name: "old.go", FilePath: "old.go", LineStart: 1, LineEnd: 10, Language: "go"},
	}
	if err := store.StoreFileNodesEdges("old.go", nodes, nil, "h"); err != nil {
		t.Fatal(err)
	}

	if err := store.RemoveFileData("old.go"); err != nil {
		t.Fatal(err)
	}

	fileNodes, err := store.GetNodesByFile("old.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(fileNodes) != 0 {
		t.Errorf("expected 0 nodes after removal, got %d", len(fileNodes))
	}
}

func TestSanitizeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"hello", 256, "hello"},
		{"a\x00b\x01c", 256, "abc"},
		{"tab\there", 256, "tab\there"},
		{"long string", 4, "long"},
	}

	for _, tc := range tests {
		got := SanitizeName(tc.input, tc.maxLen)
		if got != tc.want {
			t.Errorf("SanitizeName(%q, %d) = %q, want %q", tc.input, tc.maxLen, got, tc.want)
		}
	}
}

func TestMetadata(t *testing.T) {
	t.Parallel()
	store, cleanup := tempDB(t)
	defer cleanup()

	if err := store.SetMetadata("test_key", "test_value"); err != nil {
		t.Fatal(err)
	}

	val, err := store.GetMetadata("test_key")
	if err != nil {
		t.Fatal(err)
	}
	if val != "test_value" {
		t.Errorf("expected 'test_value', got %q", val)
	}

	val, err = store.GetMetadata("nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if val != "" {
		t.Errorf("expected empty string for missing key, got %q", val)
	}
}
