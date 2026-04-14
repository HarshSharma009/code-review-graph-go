package search

import (
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/harshsharma/code-review-graph-go/internal/embeddings"
	"github.com/harshsharma/code-review-graph-go/internal/graph"
)

// Result represents a single search hit with relevance score.
type Result struct {
	Name          string  `json:"name"`
	QualifiedName string  `json:"qualified_name"`
	Kind          string  `json:"kind"`
	FilePath      string  `json:"file_path"`
	LineStart     int     `json:"line_start"`
	LineEnd       int     `json:"line_end"`
	Language      string  `json:"language"`
	Params        *string `json:"params,omitempty"`
	ReturnType    *string `json:"return_type,omitempty"`
	Score         float64 `json:"score"`
}

// --- FTS5 index management ---

// RebuildFTSIndex drops and recreates the FTS5 index from the nodes table.
func RebuildFTSIndex(store *graph.Store) (int, error) {
	db := store.DB()
	if _, err := db.Exec("DROP TABLE IF EXISTS nodes_fts"); err != nil {
		return 0, fmt.Errorf("dropping FTS table: %w", err)
	}
	if _, err := db.Exec(`
		CREATE VIRTUAL TABLE nodes_fts USING fts5(
			name, qualified_name, file_path, signature,
			tokenize='porter unicode61'
		)
	`); err != nil {
		return 0, fmt.Errorf("creating FTS table: %w", err)
	}
	if _, err := db.Exec(`
		INSERT INTO nodes_fts(rowid, name, qualified_name, file_path, signature)
		SELECT id, name, qualified_name, file_path, COALESCE(signature, '')
		FROM nodes
	`); err != nil {
		return 0, fmt.Errorf("populating FTS index: %w", err)
	}

	var count int
	db.QueryRow("SELECT count(*) FROM nodes_fts").Scan(&count) //nolint:errcheck
	slog.Info("FTS index rebuilt", "rows", count)
	return count, nil
}

// --- Query kind boosting ---

var (
	pascalCaseRe = regexp.MustCompile(`^[A-Z][a-z]`)
	snakeCaseRe  = regexp.MustCompile(`[a-zA-Z]`)
)

// detectQueryKindBoost returns kind-specific boost multipliers based on query patterns.
func detectQueryKindBoost(query string) map[string]float64 {
	boosts := make(map[string]float64)
	q := strings.TrimSpace(query)
	if q == "" {
		return boosts
	}

	// PascalCase -> boost Class/Type
	if pascalCaseRe.MatchString(q) && q != strings.ToUpper(q) {
		boosts["Class"] = 1.5
		boosts["Type"] = 1.5
	}

	// snake_case -> boost Function
	if strings.Contains(q, "_") && snakeCaseRe.MatchString(q) {
		boosts["Function"] = 1.5
	}

	// Dotted path -> boost qualified name matches
	if strings.Contains(q, ".") {
		boosts["_qualified"] = 2.0
	}

	return boosts
}

// --- Reciprocal Rank Fusion ---

type idScore struct {
	id    int64
	score float64
}

// rrfMerge merges multiple ranked result lists using Reciprocal Rank Fusion.
func rrfMerge(lists ...[]idScore) []idScore {
	const k = 60
	scores := make(map[int64]float64)
	for _, list := range lists {
		for rank, item := range list {
			scores[item.id] += 1.0 / float64(k+rank+1)
		}
	}

	merged := make([]idScore, 0, len(scores))
	for id, score := range scores {
		merged = append(merged, idScore{id, score})
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].score > merged[j].score })
	return merged
}

// --- FTS5 search ---

func ftsSearch(db *sql.DB, query string, limit int) []idScore {
	// Wrap in double quotes to prevent FTS5 operator injection
	safe := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
	rows, err := db.Query(
		"SELECT rowid, rank FROM nodes_fts WHERE nodes_fts MATCH ? ORDER BY rank LIMIT ?",
		safe, limit,
	)
	if err != nil {
		slog.Debug("FTS5 search failed", "err", err)
		return nil
	}
	defer rows.Close()

	var results []idScore
	for rows.Next() {
		var id int64
		var rank float64
		if rows.Scan(&id, &rank) == nil {
			results = append(results, idScore{id, -rank}) // negate: FTS5 rank is negative BM25
		}
	}
	return results
}

// --- Keyword LIKE fallback ---

func keywordSearch(db *sql.DB, query string, limit int) []idScore {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil
	}

	var conditions []string
	var params []any
	for _, word := range words {
		conditions = append(conditions, "(LOWER(name) LIKE ? OR LOWER(qualified_name) LIKE ?)")
		params = append(params, "%"+word+"%", "%"+word+"%")
	}

	where := strings.Join(conditions, " AND ")
	params = append(params, limit)

	rows, err := db.Query(
		fmt.Sprintf("SELECT id, name, qualified_name FROM nodes WHERE %s LIMIT ?", where), //nolint:gosec
		params...,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	qLower := strings.ToLower(query)
	var results []idScore
	for rows.Next() {
		var id int64
		var name, qn string
		if rows.Scan(&id, &name, &qn) != nil {
			continue
		}
		nameLower := strings.ToLower(name)
		score := 1.0
		if nameLower == qLower {
			score = 3.0
		} else if strings.HasPrefix(nameLower, qLower) {
			score = 2.0
		}
		results = append(results, idScore{id, score})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	return results
}

// --- Embedding search ---

func embeddingSearch(store *graph.Store, embStore *embeddings.Store, query string, limit int) []idScore {
	if embStore == nil || !embStore.Available() || embStore.Count() == 0 {
		return nil
	}

	results, err := embStore.Search(query, limit)
	if err != nil {
		slog.Debug("embedding search failed", "err", err)
		return nil
	}

	var out []idScore
	for _, r := range results {
		n, err := store.GetNode(r.QualifiedName)
		if err != nil || n == nil {
			continue
		}
		out = append(out, idScore{n.ID, r.Score})
	}
	return out
}

// --- Main hybrid search ---

// HybridSearch combines FTS5 BM25, vector embeddings, and keyword matching via RRF.
func HybridSearch(
	store *graph.Store,
	query string,
	kind string,
	limit int,
	contextFiles []string,
	embStore *embeddings.Store,
) []Result {
	if strings.TrimSpace(query) == "" {
		return nil
	}

	db := store.DB()
	fetchLimit := limit * 3

	// Phase 1: Gather ranked lists
	ftsResults := ftsSearch(db, query, fetchLimit)
	embResults := embeddingSearch(store, embStore, query, fetchLimit)

	// Phase 2: Merge via RRF or fallback
	var merged []idScore
	if len(ftsResults) > 0 || len(embResults) > 0 {
		var lists [][]idScore
		if len(ftsResults) > 0 {
			lists = append(lists, ftsResults)
		}
		if len(embResults) > 0 {
			lists = append(lists, embResults)
		}
		merged = rrfMerge(lists...)
	} else {
		merged = keywordSearch(db, query, fetchLimit)
		if len(merged) == 0 {
			return nil
		}
	}

	// Phase 3: Batch-fetch candidate nodes
	candidateIDs := make([]any, len(merged))
	for i, m := range merged {
		candidateIDs[i] = m.id
	}

	nodeRows := make(map[int64]nodeRow)
	batchSize := 450
	for i := 0; i < len(candidateIDs); i += batchSize {
		end := i + batchSize
		if end > len(candidateIDs) {
			end = len(candidateIDs)
		}
		batch := candidateIDs[i:end]
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]

		rows, err := db.Query(
			fmt.Sprintf("SELECT id, kind, name, qualified_name, file_path, line_start, line_end, language, params, return_type FROM nodes WHERE id IN (%s)", placeholders), //nolint:gosec
			batch...,
		)
		if err != nil {
			continue
		}
		for rows.Next() {
			var nr nodeRow
			if rows.Scan(&nr.id, &nr.kind, &nr.name, &nr.qualifiedName, &nr.filePath, &nr.lineStart, &nr.lineEnd, &nr.language, &nr.params, &nr.returnType) == nil {
				nodeRows[nr.id] = nr
			}
		}
		rows.Close()
	}

	// Phase 4: Apply kind boosting and context-file boosting
	kindBoosts := detectQueryKindBoost(query)
	contextSet := make(map[string]struct{}, len(contextFiles))
	for _, f := range contextFiles {
		contextSet[f] = struct{}{}
	}

	type boostedItem struct {
		id    int64
		score float64
	}
	boosted := make([]boostedItem, 0, len(merged))
	for _, m := range merged {
		nr, ok := nodeRows[m.id]
		if !ok {
			continue
		}

		boost := 1.0
		if b, ok := kindBoosts[nr.kind]; ok {
			boost *= b
		}
		if b, ok := kindBoosts["_qualified"]; ok && strings.Contains(query, ".") {
			if strings.Contains(strings.ToLower(nr.qualifiedName), strings.ToLower(query)) {
				boost *= b
			}
		}
		if len(contextSet) > 0 {
			if _, ok := contextSet[nr.filePath]; ok {
				boost *= 1.5
			}
		}
		boosted = append(boosted, boostedItem{m.id, m.score * boost})
	}
	sort.Slice(boosted, func(i, j int) bool { return boosted[i].score > boosted[j].score })

	// Phase 5: Build results with kind filter
	var results []Result
	for _, b := range boosted {
		if len(results) >= limit {
			break
		}
		nr, ok := nodeRows[b.id]
		if !ok {
			continue
		}
		if kind != "" && nr.kind != kind {
			continue
		}
		results = append(results, Result{
			Name:          graph.SanitizeName(nr.name, 0),
			QualifiedName: graph.SanitizeName(nr.qualifiedName, 0),
			Kind:          nr.kind,
			FilePath:      nr.filePath,
			LineStart:     nr.lineStart,
			LineEnd:       nr.lineEnd,
			Language:      nr.language,
			Params:        nr.params,
			ReturnType:    nr.returnType,
			Score:         b.score,
		})
	}

	return results
}

type nodeRow struct {
	id            int64
	kind          string
	name          string
	qualifiedName string
	filePath      string
	lineStart     int
	lineEnd       int
	language      string
	params        *string
	returnType    *string
}
