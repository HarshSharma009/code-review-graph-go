package flows

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/harshsharma/code-review-graph-go/internal/config"
	"github.com/harshsharma/code-review-graph-go/internal/graph"
)

var frameworkDecoratorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)app\.(get|post|put|delete|patch|route|websocket)`),
	regexp.MustCompile(`(?i)router\.(get|post|put|delete|patch|route)`),
	regexp.MustCompile(`(?i)blueprint\.(route|before_request|after_request)`),
	regexp.MustCompile(`(?i)click\.(command|group)`),
	regexp.MustCompile(`(?i)celery\.(task|shared_task)`),
	regexp.MustCompile(`(?i)api_view`),
	regexp.MustCompile(`(?i)\baction\b`),
	regexp.MustCompile(`(?i)@(Get|Post|Put|Delete|Patch|RequestMapping)`),
}

var entryNamePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^main$`),
	regexp.MustCompile(`^__main__$`),
	regexp.MustCompile(`^test_`),
	regexp.MustCompile(`^Test[A-Z]`),
	regexp.MustCompile(`^on_`),
	regexp.MustCompile(`^handle_`),
}

// HasFrameworkDecorator checks if a node has framework-style decorators in its extra data.
func HasFrameworkDecorator(n graph.GraphNode) bool {
	raw, ok := n.Extra["decorators"]
	if !ok || raw == nil {
		return false
	}
	var decorators []string
	switch v := raw.(type) {
	case string:
		decorators = []string{v}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				decorators = append(decorators, s)
			}
		}
	case []string:
		decorators = v
	}
	for _, dec := range decorators {
		for _, pat := range frameworkDecoratorPatterns {
			if pat.MatchString(dec) {
				return true
			}
		}
	}
	return false
}

// MatchesEntryName checks if a node name matches a conventional entry-point pattern.
func MatchesEntryName(n graph.GraphNode) bool {
	for _, pat := range entryNamePatterns {
		if pat.MatchString(n.Name) {
			return true
		}
	}
	return false
}

type Flow struct {
	ID           int64   `json:"id,omitempty"`
	Name         string  `json:"name"`
	EntryPoint   string  `json:"entry_point"`
	EntryPointID int64   `json:"entry_point_id"`
	Path         []int64 `json:"path"`
	Depth        int     `json:"depth"`
	NodeCount    int     `json:"node_count"`
	FileCount    int     `json:"file_count"`
	Files        []string `json:"files,omitempty"`
	Criticality  float64 `json:"criticality"`
}

// DetectEntryPoints finds functions that are entry points in the graph.
func DetectEntryPoints(store *graph.Store) ([]graph.GraphNode, error) {
	db := store.DB()

	// Build set of all CALLS targets
	calledQN := make(map[string]struct{})
	rows, err := db.Query("SELECT DISTINCT target_qualified FROM edges WHERE kind = 'CALLS'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var qn string
		if err := rows.Scan(&qn); err == nil {
			calledQN[qn] = struct{}{}
		}
	}

	// Get all Function and Test nodes
	candidates, err := getNodesByKinds(db, []string{"Function", "Test"})
	if err != nil {
		return nil, err
	}

	var entries []graph.GraphNode
	seen := make(map[string]struct{})
	for _, n := range candidates {
		isEntry := false
		if _, called := calledQN[n.QualifiedName]; !called {
			isEntry = true
		}
		if HasFrameworkDecorator(n) {
			isEntry = true
		}
		if MatchesEntryName(n) {
			isEntry = true
		}
		if isEntry {
			if _, ok := seen[n.QualifiedName]; !ok {
				entries = append(entries, n)
				seen[n.QualifiedName] = struct{}{}
			}
		}
	}
	return entries, nil
}

func traceSingleFlow(store *graph.Store, ep graph.GraphNode, maxDepth int) *Flow {
	db := store.DB()
	pathIDs := []int64{ep.ID}
	pathQN := []string{ep.QualifiedName}
	visited := map[string]struct{}{ep.QualifiedName: {}}

	type item struct {
		qn    string
		depth int
	}
	queue := []item{{ep.QualifiedName, 0}}
	actualDepth := 0

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth > actualDepth {
			actualDepth = cur.depth
		}
		if cur.depth >= maxDepth {
			continue
		}

		edges, _ := db.Query(
			"SELECT target_qualified FROM edges WHERE source_qualified = ? AND kind = 'CALLS'",
			cur.qn,
		)
		if edges == nil {
			continue
		}
		var targets []string
		for edges.Next() {
			var tqn string
			if edges.Scan(&tqn) == nil {
				targets = append(targets, tqn)
			}
		}
		edges.Close()

		for _, tqn := range targets {
			if _, ok := visited[tqn]; ok {
				continue
			}
			var nid int64
			if db.QueryRow("SELECT id FROM nodes WHERE qualified_name = ?", tqn).Scan(&nid) != nil {
				continue
			}
			visited[tqn] = struct{}{}
			pathIDs = append(pathIDs, nid)
			pathQN = append(pathQN, tqn)
			queue = append(queue, item{tqn, cur.depth + 1})
		}
	}

	if len(pathIDs) < 2 {
		return nil
	}

	fileSet := make(map[string]struct{})
	for _, qn := range pathQN {
		var fp string
		if db.QueryRow("SELECT file_path FROM nodes WHERE qualified_name = ?", qn).Scan(&fp) == nil {
			fileSet[fp] = struct{}{}
		}
	}
	files := make([]string, 0, len(fileSet))
	for f := range fileSet {
		files = append(files, f)
	}

	flow := &Flow{
		Name:         graph.SanitizeName(ep.Name, 0),
		EntryPoint:   ep.QualifiedName,
		EntryPointID: ep.ID,
		Path:         pathIDs,
		Depth:        actualDepth,
		NodeCount:    len(pathIDs),
		FileCount:    len(files),
		Files:        files,
	}
	flow.Criticality = computeCriticality(flow, store)
	return flow
}

// TraceFlows traces execution flows from every entry point via forward BFS.
func TraceFlows(store *graph.Store, maxDepth int) ([]Flow, error) {
	eps, err := DetectEntryPoints(store)
	if err != nil {
		return nil, err
	}

	var flows []Flow
	for _, ep := range eps {
		f := traceSingleFlow(store, ep, maxDepth)
		if f != nil {
			flows = append(flows, *f)
		}
	}

	// Sort by criticality descending (simple bubble sort for stability)
	for i := 0; i < len(flows); i++ {
		for j := i + 1; j < len(flows); j++ {
			if flows[j].Criticality > flows[i].Criticality {
				flows[i], flows[j] = flows[j], flows[i]
			}
		}
	}
	return flows, nil
}

func computeCriticality(flow *Flow, store *graph.Store) float64 {
	if len(flow.Path) == 0 {
		return 0
	}
	db := store.DB()

	type nodeInfo struct {
		qn   string
		name string
		fp   string
	}
	var nodes []nodeInfo
	for _, nid := range flow.Path {
		var qn, name, fp string
		if db.QueryRow("SELECT qualified_name, name, file_path FROM nodes WHERE id = ?", nid).Scan(&qn, &name, &fp) == nil {
			nodes = append(nodes, nodeInfo{qn, name, fp})
		}
	}
	if len(nodes) == 0 {
		return 0
	}

	// File spread: 1 file => 0, 5+ => 1
	fileSet := make(map[string]struct{})
	for _, n := range nodes {
		fileSet[n.fp] = struct{}{}
	}
	fc := float64(len(fileSet))
	fileSpread := 0.0
	if fc > 1 {
		fileSpread = min64((fc-1)/4.0, 1.0)
	}

	// External calls: calls to nodes not in the graph
	externalCount := 0
	for _, n := range nodes {
		rows, _ := db.Query(
			"SELECT target_qualified FROM edges WHERE source_qualified = ? AND kind = 'CALLS'", n.qn)
		if rows == nil {
			continue
		}
		for rows.Next() {
			var tqn string
			rows.Scan(&tqn)
			var exists int
			if db.QueryRow("SELECT 1 FROM nodes WHERE qualified_name = ?", tqn).Scan(&exists) != nil {
				externalCount++
			}
		}
		rows.Close()
	}
	externalScore := min64(float64(externalCount)/5.0, 1.0)

	// Security sensitivity
	securityHits := 0
	for _, n := range nodes {
		nl := strings.ToLower(n.name)
		ql := strings.ToLower(n.qn)
		for kw := range config.SecurityKeywords {
			if strings.Contains(nl, kw) || strings.Contains(ql, kw) {
				securityHits++
				break
			}
		}
	}
	securityScore := min64(float64(securityHits)/float64(max(len(nodes), 1)), 1.0)

	// Test coverage gap
	testedCount := 0
	for _, n := range nodes {
		var exists int
		if db.QueryRow(
			"SELECT 1 FROM edges WHERE target_qualified = ? AND kind = 'TESTED_BY' LIMIT 1", n.qn,
		).Scan(&exists) == nil {
			testedCount++
		}
	}
	coverage := float64(testedCount) / float64(max(len(nodes), 1))
	testGap := 1.0 - coverage

	// Depth
	depthScore := min64(float64(flow.Depth)/10.0, 1.0)

	crit := fileSpread*0.30 + externalScore*0.20 + securityScore*0.25 + testGap*0.15 + depthScore*0.10
	if crit < 0 {
		crit = 0
	}
	if crit > 1 {
		crit = 1
	}
	return crit
}

// StoreFlows clears existing flows and persists new ones. Returns count stored.
func StoreFlows(store *graph.Store, flows []Flow) (int, error) {
	db := store.DB()
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec("DELETE FROM flow_memberships"); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("DELETE FROM flows"); err != nil {
		return 0, err
	}

	count := 0
	for _, f := range flows {
		pathJSON, _ := json.Marshal(f.Path)
		res, err := tx.Exec(
			`INSERT INTO flows (name, entry_point_id, depth, node_count, file_count, criticality, path_json)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			f.Name, f.EntryPointID, f.Depth, f.NodeCount, f.FileCount, f.Criticality, string(pathJSON),
		)
		if err != nil {
			return 0, fmt.Errorf("inserting flow %q: %w", f.Name, err)
		}
		flowID, _ := res.LastInsertId()

		for pos, nodeID := range f.Path {
			if _, err := tx.Exec(
				"INSERT OR IGNORE INTO flow_memberships (flow_id, node_id, position) VALUES (?, ?, ?)",
				flowID, nodeID, pos,
			); err != nil {
				return 0, err
			}
		}
		count++
	}

	return count, tx.Commit()
}

// GetFlows retrieves stored flows from the database.
func GetFlows(store *graph.Store, sortBy string, limit int) ([]Flow, error) {
	allowed := map[string]bool{"criticality": true, "depth": true, "node_count": true, "file_count": true, "name": true}
	if !allowed[sortBy] {
		sortBy = "criticality"
	}
	order := "DESC"
	if sortBy == "name" {
		order = "ASC"
	}

	db := store.DB()
	rows, err := db.Query(
		fmt.Sprintf("SELECT id, name, entry_point_id, depth, node_count, file_count, criticality, path_json FROM flows ORDER BY %s %s LIMIT ?", sortBy, order), //nolint:gosec
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var flows []Flow
	for rows.Next() {
		var f Flow
		var pathJSON string
		if err := rows.Scan(&f.ID, &f.Name, &f.EntryPointID, &f.Depth, &f.NodeCount, &f.FileCount, &f.Criticality, &pathJSON); err != nil {
			continue
		}
		json.Unmarshal([]byte(pathJSON), &f.Path) //nolint:errcheck
		f.Name = graph.SanitizeName(f.Name, 0)
		flows = append(flows, f)
	}
	return flows, nil
}

// GetFlowByID retrieves a single flow with full step details.
func GetFlowByID(store *graph.Store, flowID int64) (*Flow, []map[string]any, error) {
	db := store.DB()
	var f Flow
	var pathJSON string
	err := db.QueryRow(
		"SELECT id, name, entry_point_id, depth, node_count, file_count, criticality, path_json FROM flows WHERE id = ?",
		flowID,
	).Scan(&f.ID, &f.Name, &f.EntryPointID, &f.Depth, &f.NodeCount, &f.FileCount, &f.Criticality, &pathJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	json.Unmarshal([]byte(pathJSON), &f.Path) //nolint:errcheck
	f.Name = graph.SanitizeName(f.Name, 0)

	var steps []map[string]any
	for _, nid := range f.Path {
		var name, kind, fp, qn string
		var ls, le int
		if db.QueryRow(
			"SELECT name, kind, file_path, line_start, line_end, qualified_name FROM nodes WHERE id = ?", nid,
		).Scan(&name, &kind, &fp, &ls, &le, &qn) == nil {
			steps = append(steps, map[string]any{
				"node_id":        nid,
				"name":           graph.SanitizeName(name, 0),
				"kind":           kind,
				"file":           fp,
				"line_start":     ls,
				"line_end":       le,
				"qualified_name": graph.SanitizeName(qn, 0),
			})
		}
	}
	return &f, steps, nil
}

// GetAffectedFlows finds flows that include nodes from the given changed files.
func GetAffectedFlows(store *graph.Store, changedFiles []string) ([]Flow, error) {
	if len(changedFiles) == 0 {
		return nil, nil
	}
	db := store.DB()

	placeholders := strings.Repeat("?,", len(changedFiles))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, len(changedFiles))
	for i, f := range changedFiles {
		args[i] = f
	}

	query := fmt.Sprintf(
		`SELECT DISTINCT f.id FROM flows f
		 JOIN flow_memberships fm ON fm.flow_id = f.id
		 JOIN nodes n ON n.id = fm.node_id
		 WHERE n.file_path IN (%s)`, placeholders) //nolint:gosec
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var affected []Flow
	for rows.Next() {
		var fid int64
		if rows.Scan(&fid) == nil {
			f, _, err := GetFlowByID(store, fid)
			if err == nil && f != nil {
				affected = append(affected, *f)
			}
		}
	}

	// Sort by criticality descending
	for i := 0; i < len(affected); i++ {
		for j := i + 1; j < len(affected); j++ {
			if affected[j].Criticality > affected[i].Criticality {
				affected[i], affected[j] = affected[j], affected[i]
			}
		}
	}
	return affected, nil
}

// --- helpers ---

func getNodesByKinds(db *sql.DB, kinds []string) ([]graph.GraphNode, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(kinds))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = k
	}

	rows, err := db.Query(
		fmt.Sprintf("SELECT id, kind, name, qualified_name, file_path, line_start, line_end, language, extra FROM nodes WHERE kind IN (%s)", placeholders), //nolint:gosec
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []graph.GraphNode
	for rows.Next() {
		var n graph.GraphNode
		var extraStr string
		if err := rows.Scan(&n.ID, &n.Kind, &n.Name, &n.QualifiedName, &n.FilePath, &n.LineStart, &n.LineEnd, &n.Language, &extraStr); err != nil {
			continue
		}
		if extraStr != "" {
			json.Unmarshal([]byte(extraStr), &n.Extra) //nolint:errcheck
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func init() {
	slog.Debug("flows package loaded")
}
