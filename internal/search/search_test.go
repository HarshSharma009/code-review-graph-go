package search

import (
	"testing"

	"github.com/harshsharma/code-review-graph-go/internal/graph"
)

func setupStore(t *testing.T) *graph.Store {
	t.Helper()
	store, err := graph.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	nodes := []graph.NodeInfo{
		{Kind: "Function", Name: "getUserByID", FilePath: "auth/user.go", LineStart: 10, LineEnd: 25, Language: "go"},
		{Kind: "Function", Name: "create_order", FilePath: "orders/service.go", LineStart: 5, LineEnd: 40, Language: "go"},
		{Kind: "Class", Name: "UserService", FilePath: "auth/service.go", LineStart: 1, LineEnd: 100, Language: "go"},
		{Kind: "Type", Name: "OrderStatus", FilePath: "orders/types.go", LineStart: 3, LineEnd: 8, Language: "go"},
		{Kind: "Function", Name: "handleRequest", FilePath: "server/handler.go", LineStart: 15, LineEnd: 45, Language: "go"},
	}
	for _, n := range nodes {
		if _, err := store.UpsertNode(n, "hash123"); err != nil {
			t.Fatalf("failed to upsert node: %v", err)
		}
	}
	return store
}

func TestHybridSearchKeywordFallback(t *testing.T) {
	store := setupStore(t)
	results := HybridSearch(store, "user", "", 10, nil, nil)
	if len(results) == 0 {
		t.Fatal("expected results for 'user'")
	}

	foundGetUser := false
	foundService := false
	for _, r := range results {
		if r.Name == "getUserByID" {
			foundGetUser = true
		}
		if r.Name == "UserService" {
			foundService = true
		}
	}
	if !foundGetUser {
		t.Error("expected getUserByID in results")
	}
	if !foundService {
		t.Error("expected UserService in results")
	}
}

func TestHybridSearchKindFilter(t *testing.T) {
	store := setupStore(t)
	results := HybridSearch(store, "order", "Function", 10, nil, nil)
	for _, r := range results {
		if r.Kind != "Function" {
			t.Errorf("expected kind=Function, got %s", r.Kind)
		}
	}

	results = HybridSearch(store, "order", "Class", 10, nil, nil)
	if len(results) != 0 {
		t.Errorf("expected no Class results for 'order', got %d", len(results))
	}
}

func TestHybridSearchContextFileBoost(t *testing.T) {
	store := setupStore(t)

	results := HybridSearch(store, "user", "", 10, []string{"auth/user.go"}, nil)
	if len(results) == 0 {
		t.Fatal("expected results")
	}

	boostedFound := false
	for _, r := range results {
		if r.FilePath == "auth/user.go" {
			boostedFound = true
			break
		}
	}
	if !boostedFound {
		t.Error("expected context-boosted file in results")
	}
}

func TestHybridSearchEmptyQuery(t *testing.T) {
	store := setupStore(t)
	results := HybridSearch(store, "", "", 10, nil, nil)
	if results != nil {
		t.Errorf("expected nil for empty query, got %d results", len(results))
	}
}

func TestDetectQueryKindBoost(t *testing.T) {
	tests := []struct {
		query    string
		expected map[string]float64
	}{
		{"UserService", map[string]float64{"Class": 1.5, "Type": 1.5}},
		{"get_users", map[string]float64{"Function": 1.5}},
		{"auth.UserService", map[string]float64{"_qualified": 2.0}},
		{"hello", map[string]float64{}},
		{"", map[string]float64{}},
	}

	for _, tc := range tests {
		boosts := detectQueryKindBoost(tc.query)
		for k, v := range tc.expected {
			if boosts[k] != v {
				t.Errorf("query=%q: boost[%s]=%f, want %f", tc.query, k, boosts[k], v)
			}
		}
	}
}

func TestRRFMerge(t *testing.T) {
	list1 := []idScore{{1, 10.0}, {2, 8.0}, {3, 6.0}}
	list2 := []idScore{{2, 9.0}, {4, 7.0}, {1, 5.0}}

	merged := rrfMerge(list1, list2)
	if len(merged) != 4 {
		t.Fatalf("expected 4 merged items, got %d", len(merged))
	}

	// ID 2 and ID 1 appear in both lists, should have highest RRF scores
	topTwo := map[int64]bool{merged[0].id: true, merged[1].id: true}
	if !topTwo[1] || !topTwo[2] {
		t.Errorf("expected IDs 1 and 2 in top-2, got %v and %v", merged[0].id, merged[1].id)
	}
}

func TestRebuildFTSIndex(t *testing.T) {
	store := setupStore(t)
	count, err := RebuildFTSIndex(store)
	if err != nil {
		t.Fatalf("rebuild failed: %v", err)
	}
	if count != 5 {
		t.Errorf("expected 5 indexed rows, got %d", count)
	}

	// Search via FTS should now work
	results := HybridSearch(store, "handleRequest", "", 10, nil, nil)
	if len(results) == 0 {
		t.Fatal("expected FTS results for 'handleRequest'")
	}
	if results[0].Name != "handleRequest" {
		t.Errorf("expected handleRequest first, got %s", results[0].Name)
	}
}
