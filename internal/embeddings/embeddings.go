package embeddings

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"

	"github.com/harshsharma/code-review-graph-go/internal/graph"
)

// Provider is the interface for embedding backends.
type Provider interface {
	Embed(texts []string) ([][]float32, error)
	EmbedQuery(text string) ([]float32, error)
	Dimension() int
	Name() string
}

// Store manages vector embeddings for graph nodes in SQLite.
type Store struct {
	provider Provider
	db       *sql.DB
	mu       sync.Mutex
}

const schema = `
CREATE TABLE IF NOT EXISTS embeddings (
    qualified_name TEXT PRIMARY KEY,
    vector BLOB NOT NULL,
    text_hash TEXT NOT NULL,
    provider TEXT NOT NULL DEFAULT 'unknown'
);
`

// NewStore creates an embedding store. provider may be nil if embeddings are disabled.
func NewStore(dbPath string, provider Provider) (*Store, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening embedding db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating embedding schema: %w", err)
	}
	return &Store{provider: provider, db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Available returns true if a provider is configured.
func (s *Store) Available() bool {
	return s.provider != nil
}

func nodeToText(n graph.GraphNode) string {
	parts := []string{n.Name}
	if n.Kind != "File" {
		parts = append(parts, strings.ToLower(n.Kind))
	}
	if n.ParentName != nil && *n.ParentName != "" {
		parts = append(parts, "in "+*n.ParentName)
	}
	if n.Params != nil && *n.Params != "" {
		parts = append(parts, *n.Params)
	}
	if n.ReturnType != nil && *n.ReturnType != "" {
		parts = append(parts, "returns "+*n.ReturnType)
	}
	if n.Language != "" {
		parts = append(parts, n.Language)
	}
	return strings.Join(parts, " ")
}

func textHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", h)
}

// EmbedNodes computes and stores embeddings for a list of nodes.
func (s *Store) EmbedNodes(nodes []graph.GraphNode, batchSize int) (int, error) {
	if s.provider == nil {
		return 0, nil
	}
	if batchSize <= 0 {
		batchSize = 64
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	provName := s.provider.Name()

	type toEmbed struct {
		node     graph.GraphNode
		text     string
		textHash string
	}
	var batch []toEmbed

	for _, n := range nodes {
		if n.Kind == "File" {
			continue
		}
		text := nodeToText(n)
		th := textHash(text)

		var existingHash, existingProvider string
		err := s.db.QueryRow(
			"SELECT text_hash, provider FROM embeddings WHERE qualified_name = ?",
			n.QualifiedName,
		).Scan(&existingHash, &existingProvider)
		if err == nil && existingHash == th && existingProvider == provName {
			continue
		}
		batch = append(batch, toEmbed{n, text, th})
	}

	if len(batch) == 0 {
		return 0, nil
	}

	embedded := 0
	for i := 0; i < len(batch); i += batchSize {
		end := i + batchSize
		if end > len(batch) {
			end = len(batch)
		}
		chunk := batch[i:end]

		texts := make([]string, len(chunk))
		for j, c := range chunk {
			texts[j] = c.text
		}

		vectors, err := s.provider.Embed(texts)
		if err != nil {
			slog.Warn("embedding batch failed", "err", err, "batch_start", i)
			continue
		}

		for j, vec := range vectors {
			blob := encodeVector(vec)
			if _, err := s.db.Exec(
				`INSERT OR REPLACE INTO embeddings (qualified_name, vector, text_hash, provider)
				 VALUES (?, ?, ?, ?)`,
				chunk[j].node.QualifiedName, blob, chunk[j].textHash, provName,
			); err != nil {
				slog.Warn("storing embedding failed", "qn", chunk[j].node.QualifiedName, "err", err)
			}
			embedded++
		}
	}

	return embedded, nil
}

// Search finds nodes by semantic similarity. Returns (qualified_name, score) pairs.
func (s *Store) Search(query string, limit int) ([]SearchResult, error) {
	if s.provider == nil {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	provName := s.provider.Name()
	queryVec, err := s.provider.EmbedQuery(query)
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}

	rows, err := s.db.Query(
		"SELECT qualified_name, vector FROM embeddings WHERE provider = ?", provName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var qn string
		var blob []byte
		if rows.Scan(&qn, &blob) != nil {
			continue
		}
		vec := decodeVector(blob)
		sim := cosineSimilarity(queryVec, vec)
		results = append(results, SearchResult{QualifiedName: qn, Score: sim})
	}

	// Sort by score descending
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].Score > results[i].Score {
				results[i], results[j] = results[j], results[i]
			}
		}
	}
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// SearchResult holds a single search match.
type SearchResult struct {
	QualifiedName string  `json:"qualified_name"`
	Score         float64 `json:"score"`
}

// Count returns the number of stored embeddings.
func (s *Store) Count() int {
	var c int
	s.db.QueryRow("SELECT COUNT(*) FROM embeddings").Scan(&c) //nolint:errcheck
	return c
}

// RemoveNode deletes embeddings for a qualified name.
func (s *Store) RemoveNode(qualifiedName string) {
	s.db.Exec("DELETE FROM embeddings WHERE qualified_name = ?", qualifiedName) //nolint:errcheck
}

// EmbedAllNodes embeds all non-file nodes in the graph.
func EmbedAllNodes(graphStore *graph.Store, embStore *Store) (int, error) {
	if !embStore.Available() {
		return 0, nil
	}

	files, err := graphStore.GetAllFiles()
	if err != nil {
		return 0, err
	}

	var allNodes []graph.GraphNode
	for _, f := range files {
		nodes, err := graphStore.GetNodesByFile(f)
		if err == nil {
			allNodes = append(allNodes, nodes...)
		}
	}
	return embStore.EmbedNodes(allNodes, 64)
}

// SemanticSearch searches nodes using vector similarity, falling back to keyword search.
func SemanticSearch(query string, graphStore *graph.Store, embStore *Store, limit int) ([]map[string]any, error) {
	if embStore != nil && embStore.Available() && embStore.Count() > 0 {
		results, err := embStore.Search(query, limit)
		if err != nil {
			return nil, err
		}
		var output []map[string]any
		for _, r := range results {
			n, err := graphStore.GetNode(r.QualifiedName)
			if err != nil || n == nil {
				continue
			}
			d := graph.NodeToDict(*n)
			d["similarity_score"] = math.Round(r.Score*10000) / 10000
			output = append(output, d)
		}
		return output, nil
	}

	// Fallback to keyword search
	nodes, err := graphStore.SearchNodes(query, limit)
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, len(nodes))
	for i, n := range nodes {
		result[i] = graph.NodeToDict(n)
	}
	return result, nil
}

// GetProvider returns a provider based on environment configuration.
// Returns nil if no embedding support is available.
func GetProvider() Provider {
	providerName := os.Getenv("CRG_EMBEDDING_PROVIDER")
	if providerName == "" {
		providerName = "local"
	}
	// For now, local embeddings require sentence-transformers which is Python-only.
	// Return nil to signal embeddings are unavailable (keyword search fallback).
	slog.Debug("embedding provider requested", "provider", providerName)
	return nil
}

// --- vector encoding ---

func encodeVector(vec []float32) []byte {
	buf := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func decodeVector(blob []byte) []float32 {
	n := len(blob) / 4
	vec := make([]float32, n)
	for i := 0; i < n; i++ {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vec
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
