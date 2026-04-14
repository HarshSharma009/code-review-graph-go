package graph

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"
)

const schemaDDL = `
CREATE TABLE IF NOT EXISTS nodes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    qualified_name TEXT NOT NULL UNIQUE,
    file_path TEXT NOT NULL,
    line_start INTEGER,
    line_end INTEGER,
    language TEXT,
    parent_name TEXT,
    params TEXT,
    return_type TEXT,
    modifiers TEXT,
    is_test INTEGER DEFAULT 0,
    file_hash TEXT,
    extra TEXT DEFAULT '{}',
    updated_at REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS edges (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,
    source_qualified TEXT NOT NULL,
    target_qualified TEXT NOT NULL,
    file_path TEXT NOT NULL,
    line INTEGER DEFAULT 0,
    extra TEXT DEFAULT '{}',
    updated_at REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_nodes_file ON nodes(file_path);
CREATE INDEX IF NOT EXISTS idx_nodes_kind ON nodes(kind);
CREATE INDEX IF NOT EXISTS idx_nodes_qualified ON nodes(qualified_name);
CREATE INDEX IF NOT EXISTS idx_edges_source ON edges(source_qualified);
CREATE INDEX IF NOT EXISTS idx_edges_target ON edges(target_qualified);
CREATE INDEX IF NOT EXISTS idx_edges_kind ON edges(kind);
CREATE INDEX IF NOT EXISTS idx_edges_file ON edges(file_path);
`

// Store is a concurrency-safe SQLite-backed code knowledge graph.
// Writes are serialised via a mutex; reads use the sql.DB connection pool.
type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
}

func NewStore(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	dsn := dbPath + "?_journal_mode=WAL&_synchronous=NORMAL&cache=shared&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if _, err := db.Exec(schemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("initializing schema: %w", err)
	}

	var schemaVer int
	row := db.QueryRow("SELECT value FROM metadata WHERE key = 'schema_version'")
	if err := row.Scan(&schemaVer); err != nil {
		if _, err := db.Exec(
			"INSERT OR IGNORE INTO metadata (key, value) VALUES ('schema_version', '1')",
		); err != nil {
			db.Close()
			return nil, fmt.Errorf("setting initial schema version: %w", err)
		}
	}

	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

// --- Write operations (serialised via writeMu) ---

func (s *Store) UpsertNode(node NodeInfo, fileHash string) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	now := nowUnix()
	qualified := makeQualified(node)
	extra := marshalExtra(node.Extra)

	_, err := s.db.Exec(`
		INSERT INTO nodes
			(kind, name, qualified_name, file_path, line_start, line_end,
			 language, parent_name, params, return_type, modifiers, is_test,
			 file_hash, extra, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(qualified_name) DO UPDATE SET
			kind=excluded.kind, name=excluded.name,
			file_path=excluded.file_path, line_start=excluded.line_start,
			line_end=excluded.line_end, language=excluded.language,
			parent_name=excluded.parent_name, params=excluded.params,
			return_type=excluded.return_type, modifiers=excluded.modifiers,
			is_test=excluded.is_test, file_hash=excluded.file_hash,
			extra=excluded.extra, updated_at=excluded.updated_at`,
		node.Kind, node.Name, qualified, node.FilePath,
		node.LineStart, node.LineEnd, node.Language,
		nullStr(node.ParentName), nullStr(node.Params),
		nullStr(node.ReturnType), nullStr(node.Modifiers),
		boolToInt(node.IsTest), fileHash, extra, now,
	)
	if err != nil {
		return 0, fmt.Errorf("upserting node %q: %w", qualified, err)
	}

	var id int64
	err = s.db.QueryRow("SELECT id FROM nodes WHERE qualified_name = ?", qualified).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("fetching node id for %q: %w", qualified, err)
	}
	return id, nil
}

func (s *Store) UpsertEdge(edge EdgeInfo) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	now := nowUnix()
	extra := marshalExtra(edge.Extra)

	var existingID int64
	err := s.db.QueryRow(`
		SELECT id FROM edges
		WHERE kind=? AND source_qualified=? AND target_qualified=?
			AND file_path=? AND line=?`,
		edge.Kind, edge.Source, edge.Target, edge.FilePath, edge.Line,
	).Scan(&existingID)

	if err == nil {
		_, err = s.db.Exec(
			"UPDATE edges SET line=?, extra=?, updated_at=? WHERE id=?",
			edge.Line, extra, now, existingID,
		)
		if err != nil {
			return 0, fmt.Errorf("updating edge %d: %w", existingID, err)
		}
		return existingID, nil
	}

	res, err := s.db.Exec(`
		INSERT INTO edges
			(kind, source_qualified, target_qualified, file_path, line, extra, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		edge.Kind, edge.Source, edge.Target, edge.FilePath, edge.Line, extra, now,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting edge: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) RemoveFileData(filePath string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if _, err := s.db.Exec("DELETE FROM nodes WHERE file_path = ?", filePath); err != nil {
		return fmt.Errorf("deleting nodes for %s: %w", filePath, err)
	}
	if _, err := s.db.Exec("DELETE FROM edges WHERE file_path = ?", filePath); err != nil {
		return fmt.Errorf("deleting edges for %s: %w", filePath, err)
	}
	return nil
}

// StoreFileNodesEdges atomically replaces all data for a file.
func (s *Store) StoreFileNodesEdges(filePath string, nodes []NodeInfo, edges []EdgeInfo, fileHash string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}

	if _, err := tx.Exec("DELETE FROM nodes WHERE file_path = ?", filePath); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("clearing nodes for %s: %w", filePath, err)
	}
	if _, err := tx.Exec("DELETE FROM edges WHERE file_path = ?", filePath); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("clearing edges for %s: %w", filePath, err)
	}

	now := nowUnix()
	for _, node := range nodes {
		qualified := makeQualified(node)
		extra := marshalExtra(node.Extra)
		if _, err := tx.Exec(`
			INSERT INTO nodes
				(kind, name, qualified_name, file_path, line_start, line_end,
				 language, parent_name, params, return_type, modifiers, is_test,
				 file_hash, extra, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(qualified_name) DO UPDATE SET
				kind=excluded.kind, name=excluded.name,
				file_path=excluded.file_path, line_start=excluded.line_start,
				line_end=excluded.line_end, language=excluded.language,
				parent_name=excluded.parent_name, params=excluded.params,
				return_type=excluded.return_type, modifiers=excluded.modifiers,
				is_test=excluded.is_test, file_hash=excluded.file_hash,
				extra=excluded.extra, updated_at=excluded.updated_at`,
			node.Kind, node.Name, qualified, filePath,
			node.LineStart, node.LineEnd, node.Language,
			nullStr(node.ParentName), nullStr(node.Params),
			nullStr(node.ReturnType), nullStr(node.Modifiers),
			boolToInt(node.IsTest), fileHash, extra, now,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("inserting node %q: %w", qualified, err)
		}
	}

	for _, edge := range edges {
		extra := marshalExtra(edge.Extra)
		if _, err := tx.Exec(`
			INSERT INTO edges
				(kind, source_qualified, target_qualified, file_path, line, extra, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			edge.Kind, edge.Source, edge.Target, filePath, edge.Line, extra, now,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("inserting edge: %w", err)
		}
	}

	return tx.Commit()
}

func (s *Store) SetMetadata(key, value string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec("INSERT OR REPLACE INTO metadata (key, value) VALUES (?, ?)", key, value)
	return err
}

func (s *Store) GetMetadata(key string) (string, error) {
	var val string
	err := s.db.QueryRow("SELECT value FROM metadata WHERE key=?", key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

// --- Read operations (concurrent-safe via sql.DB pool) ---

func (s *Store) GetNode(qualifiedName string) (*GraphNode, error) {
	row := s.db.QueryRow("SELECT * FROM nodes WHERE qualified_name = ?", qualifiedName)
	return scanNode(row)
}

func (s *Store) GetNodesByFile(filePath string) ([]GraphNode, error) {
	rows, err := s.db.Query("SELECT * FROM nodes WHERE file_path = ?", filePath)
	if err != nil {
		return nil, fmt.Errorf("querying nodes by file: %w", err)
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (s *Store) GetEdgesBySource(qualifiedName string) ([]GraphEdge, error) {
	rows, err := s.db.Query("SELECT * FROM edges WHERE source_qualified = ?", qualifiedName)
	if err != nil {
		return nil, fmt.Errorf("querying edges by source: %w", err)
	}
	defer rows.Close()
	return scanEdges(rows)
}

func (s *Store) GetEdgesByTarget(qualifiedName string) ([]GraphEdge, error) {
	rows, err := s.db.Query("SELECT * FROM edges WHERE target_qualified = ?", qualifiedName)
	if err != nil {
		return nil, fmt.Errorf("querying edges by target: %w", err)
	}
	defer rows.Close()
	return scanEdges(rows)
}

func (s *Store) GetAllFiles() ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT file_path FROM nodes WHERE kind = 'File'")
	if err != nil {
		return nil, fmt.Errorf("querying all files: %w", err)
	}
	defer rows.Close()

	var files []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, fmt.Errorf("scanning file path: %w", err)
		}
		files = append(files, fp)
	}
	return files, rows.Err()
}

func (s *Store) SearchNodes(query string, limit int) ([]GraphNode, error) {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil, nil
	}

	var conditions []string
	var params []any
	for _, word := range words {
		conditions = append(conditions,
			"(LOWER(name) LIKE ? OR LOWER(qualified_name) LIKE ?)",
		)
		params = append(params, "%"+word+"%", "%"+word+"%")
	}

	where := strings.Join(conditions, " AND ")
	params = append(params, limit)

	rows, err := s.db.Query(
		fmt.Sprintf("SELECT * FROM nodes WHERE %s LIMIT ?", where), //nolint:gosec
		params...,
	)
	if err != nil {
		return nil, fmt.Errorf("searching nodes: %w", err)
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (s *Store) GetStats() (*GraphStats, error) {
	stats := &GraphStats{
		NodesByKind: make(map[string]int),
		EdgesByKind: make(map[string]int),
	}

	if err := s.db.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&stats.TotalNodes); err != nil {
		return nil, fmt.Errorf("counting nodes: %w", err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM edges").Scan(&stats.TotalEdges); err != nil {
		return nil, fmt.Errorf("counting edges: %w", err)
	}

	rows, err := s.db.Query("SELECT kind, COUNT(*) as cnt FROM nodes GROUP BY kind")
	if err != nil {
		return nil, fmt.Errorf("grouping nodes: %w", err)
	}
	for rows.Next() {
		var kind string
		var cnt int
		if err := rows.Scan(&kind, &cnt); err != nil {
			rows.Close()
			return nil, err
		}
		stats.NodesByKind[kind] = cnt
	}
	rows.Close()

	rows, err = s.db.Query("SELECT kind, COUNT(*) as cnt FROM edges GROUP BY kind")
	if err != nil {
		return nil, fmt.Errorf("grouping edges: %w", err)
	}
	for rows.Next() {
		var kind string
		var cnt int
		if err := rows.Scan(&kind, &cnt); err != nil {
			rows.Close()
			return nil, err
		}
		stats.EdgesByKind[kind] = cnt
	}
	rows.Close()

	langRows, err := s.db.Query(
		"SELECT DISTINCT language FROM nodes WHERE language IS NOT NULL AND language != ''",
	)
	if err != nil {
		return nil, fmt.Errorf("querying languages: %w", err)
	}
	for langRows.Next() {
		var lang string
		if err := langRows.Scan(&lang); err != nil {
			langRows.Close()
			return nil, err
		}
		stats.Languages = append(stats.Languages, lang)
	}
	langRows.Close()

	if err := s.db.QueryRow("SELECT COUNT(*) FROM nodes WHERE kind = 'File'").Scan(&stats.FilesCount); err != nil {
		return nil, fmt.Errorf("counting files: %w", err)
	}

	stats.LastUpdated, _ = s.GetMetadata("last_updated")

	return stats, nil
}

func (s *Store) GetEdgesAmong(qualifiedNames map[string]struct{}) ([]GraphEdge, error) {
	if len(qualifiedNames) == 0 {
		return nil, nil
	}

	qns := make([]string, 0, len(qualifiedNames))
	for qn := range qualifiedNames {
		qns = append(qns, qn)
	}

	var result []GraphEdge
	const batchSize = 450
	for i := 0; i < len(qns); i += batchSize {
		end := i + batchSize
		if end > len(qns) {
			end = len(qns)
		}
		batch := qns[i:end]

		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]

		params := make([]any, len(batch))
		for j, qn := range batch {
			params[j] = qn
		}

		rows, err := s.db.Query(
			fmt.Sprintf("SELECT * FROM edges WHERE source_qualified IN (%s)", placeholders), //nolint:gosec
			params...,
		)
		if err != nil {
			return nil, fmt.Errorf("querying edges among: %w", err)
		}

		edges, err := scanEdges(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}

		for _, e := range edges {
			if _, ok := qualifiedNames[e.TargetQualified]; ok {
				result = append(result, e)
			}
		}
	}
	return result, nil
}

func (s *Store) BatchGetNodes(qualifiedNames map[string]struct{}) ([]GraphNode, error) {
	if len(qualifiedNames) == 0 {
		return nil, nil
	}

	qns := make([]string, 0, len(qualifiedNames))
	for qn := range qualifiedNames {
		qns = append(qns, qn)
	}

	var result []GraphNode
	const batchSize = 450
	for i := 0; i < len(qns); i += batchSize {
		end := i + batchSize
		if end > len(qns) {
			end = len(qns)
		}
		batch := qns[i:end]

		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]

		params := make([]any, len(batch))
		for j, qn := range batch {
			params[j] = qn
		}

		rows, err := s.db.Query(
			fmt.Sprintf("SELECT * FROM nodes WHERE qualified_name IN (%s)", placeholders), //nolint:gosec
			params...,
		)
		if err != nil {
			return nil, fmt.Errorf("batch get nodes: %w", err)
		}

		nodes, err := scanNodes(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		result = append(result, nodes...)
	}
	return result, nil
}

// --- Impact / Graph traversal ---

func (s *Store) GetImpactRadius(changedFiles []string, maxDepth, maxNodes int) (*ImpactResult, error) {
	if len(changedFiles) == 0 {
		return &ImpactResult{}, nil
	}

	seeds := make(map[string]struct{})
	for _, f := range changedFiles {
		nodes, err := s.GetNodesByFile(f)
		if err != nil {
			return nil, fmt.Errorf("getting nodes for file %s: %w", f, err)
		}
		for _, n := range nodes {
			seeds[n.QualifiedName] = struct{}{}
		}
	}

	if len(seeds) == 0 {
		return &ImpactResult{}, nil
	}

	// Create temp table for seeds
	if _, err := s.db.Exec("CREATE TEMP TABLE IF NOT EXISTS _impact_seeds (qn TEXT PRIMARY KEY)"); err != nil {
		return nil, fmt.Errorf("creating temp table: %w", err)
	}
	if _, err := s.db.Exec("DELETE FROM _impact_seeds"); err != nil {
		return nil, fmt.Errorf("clearing temp table: %w", err)
	}

	seedList := make([]string, 0, len(seeds))
	for qn := range seeds {
		seedList = append(seedList, qn)
	}

	const batchSize = 450
	for i := 0; i < len(seedList); i += batchSize {
		end := i + batchSize
		if end > len(seedList) {
			end = len(seedList)
		}
		batch := seedList[i:end]
		placeholders := strings.Repeat("(?),", len(batch))
		placeholders = placeholders[:len(placeholders)-1]

		params := make([]any, len(batch))
		for j, qn := range batch {
			params[j] = qn
		}

		if _, err := s.db.Exec(
			fmt.Sprintf("INSERT OR IGNORE INTO _impact_seeds (qn) VALUES %s", placeholders), //nolint:gosec
			params...,
		); err != nil {
			return nil, fmt.Errorf("inserting seeds: %w", err)
		}
	}

	// Recursive CTE for impact radius
	rows, err := s.db.Query(`
		WITH RECURSIVE impacted(node_qn, depth) AS (
			SELECT qn, 0 FROM _impact_seeds
			UNION
			SELECT e.target_qualified, i.depth + 1
			FROM impacted i
			JOIN edges e ON e.source_qualified = i.node_qn
			WHERE i.depth < ?
			UNION
			SELECT e.source_qualified, i.depth + 1
			FROM impacted i
			JOIN edges e ON e.target_qualified = i.node_qn
			WHERE i.depth < ?
		)
		SELECT DISTINCT node_qn, MIN(depth) AS min_depth
		FROM impacted
		GROUP BY node_qn
		LIMIT ?`,
		maxDepth, maxDepth, maxNodes+len(seeds),
	)
	if err != nil {
		return nil, fmt.Errorf("running impact CTE: %w", err)
	}
	defer rows.Close()

	impactedQNs := make(map[string]struct{})
	for rows.Next() {
		var qn string
		var depth int
		if err := rows.Scan(&qn, &depth); err != nil {
			return nil, fmt.Errorf("scanning impact row: %w", err)
		}
		if _, isSeed := seeds[qn]; !isSeed {
			impactedQNs[qn] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	changedNodes, err := s.BatchGetNodes(seeds)
	if err != nil {
		return nil, fmt.Errorf("fetching changed nodes: %w", err)
	}

	impactedNodes, err := s.BatchGetNodes(impactedQNs)
	if err != nil {
		return nil, fmt.Errorf("fetching impacted nodes: %w", err)
	}

	totalImpacted := len(impactedNodes)
	truncated := totalImpacted > maxNodes
	if truncated {
		impactedNodes = impactedNodes[:maxNodes]
	}

	fileSet := make(map[string]struct{})
	for _, n := range impactedNodes {
		fileSet[n.FilePath] = struct{}{}
	}
	impactedFiles := make([]string, 0, len(fileSet))
	for f := range fileSet {
		impactedFiles = append(impactedFiles, f)
	}

	allQNs := make(map[string]struct{}, len(seeds)+len(impactedNodes))
	for qn := range seeds {
		allQNs[qn] = struct{}{}
	}
	for _, n := range impactedNodes {
		allQNs[n.QualifiedName] = struct{}{}
	}

	relevantEdges, err := s.GetEdgesAmong(allQNs)
	if err != nil {
		return nil, fmt.Errorf("fetching relevant edges: %w", err)
	}

	return &ImpactResult{
		ChangedNodes:  changedNodes,
		ImpactedNodes: impactedNodes,
		ImpactedFiles: impactedFiles,
		Edges:         relevantEdges,
		Truncated:     truncated,
		TotalImpacted: totalImpacted,
	}, nil
}

// --- Helpers ---

func makeQualified(node NodeInfo) string {
	if node.Kind == string(NodeFile) {
		return node.FilePath
	}
	if node.ParentName != "" {
		return fmt.Sprintf("%s::%s.%s", node.FilePath, node.ParentName, node.Name)
	}
	return fmt.Sprintf("%s::%s", node.FilePath, node.Name)
}

func marshalExtra(extra map[string]any) string {
	if extra == nil || len(extra) == 0 {
		return "{}"
	}
	b, err := json.Marshal(extra)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SanitizeName strips ASCII control characters and truncates for safety.
func SanitizeName(s string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = 256
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, ch := range s {
		if ch == '\t' || ch == '\n' || ch >= 0x20 {
			b.WriteRune(ch)
		}
	}
	result := b.String()
	if len(result) > maxLen {
		return result[:maxLen]
	}
	return result
}

func NodeToDict(n GraphNode) map[string]any {
	return map[string]any{
		"id":             n.ID,
		"kind":           n.Kind,
		"name":           SanitizeName(n.Name, 256),
		"qualified_name": SanitizeName(n.QualifiedName, 256),
		"file_path":      n.FilePath,
		"line_start":     n.LineStart,
		"line_end":       n.LineEnd,
		"language":       n.Language,
		"parent_name":    n.ParentName,
		"is_test":        n.IsTest,
	}
}

func EdgeToDict(e GraphEdge) map[string]any {
	return map[string]any{
		"id":        e.ID,
		"kind":      e.Kind,
		"source":    SanitizeName(e.SourceQualified, 256),
		"target":    SanitizeName(e.TargetQualified, 256),
		"file_path": e.FilePath,
		"line":      e.Line,
	}
}

func scanNode(row *sql.Row) (*GraphNode, error) {
	n := &GraphNode{}
	var extraStr string
	var isTest int
	err := row.Scan(
		&n.ID, &n.Kind, &n.Name, &n.QualifiedName, &n.FilePath,
		&n.LineStart, &n.LineEnd, &n.Language, &n.ParentName,
		&n.Params, &n.ReturnType,
		new(sql.NullString), // modifiers
		&isTest, &n.FileHash, &extraStr, new(float64), // updated_at
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scanning node: %w", err)
	}
	n.IsTest = isTest != 0
	n.Extra = parseExtra(extraStr)
	return n, nil
}

func scanNodes(rows *sql.Rows) ([]GraphNode, error) {
	var nodes []GraphNode
	for rows.Next() {
		var n GraphNode
		var extraStr string
		var isTest int
		err := rows.Scan(
			&n.ID, &n.Kind, &n.Name, &n.QualifiedName, &n.FilePath,
			&n.LineStart, &n.LineEnd, &n.Language, &n.ParentName,
			&n.Params, &n.ReturnType,
			new(sql.NullString), // modifiers
			&isTest, &n.FileHash, &extraStr, new(float64), // updated_at
		)
		if err != nil {
			return nil, fmt.Errorf("scanning node row: %w", err)
		}
		n.IsTest = isTest != 0
		n.Extra = parseExtra(extraStr)
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func scanEdges(rows *sql.Rows) ([]GraphEdge, error) {
	var edges []GraphEdge
	for rows.Next() {
		var e GraphEdge
		var extraStr string
		err := rows.Scan(
			&e.ID, &e.Kind, &e.SourceQualified, &e.TargetQualified,
			&e.FilePath, &e.Line, &extraStr, new(float64), // updated_at
		)
		if err != nil {
			return nil, fmt.Errorf("scanning edge row: %w", err)
		}
		e.Extra = parseExtra(extraStr)
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

func parseExtra(s string) map[string]any {
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		slog.Debug("failed to parse extra json", "error", err)
		return nil
	}
	return m
}
