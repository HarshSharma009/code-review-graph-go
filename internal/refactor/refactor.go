package refactor

import (
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/harshsharma/code-review-graph-go/internal/flows"
	"github.com/harshsharma/code-review-graph-go/internal/graph"
)

const ExpirySeconds = 600 // 10 minutes

var (
	mu       sync.Mutex
	pending  = make(map[string]*Preview)
)

// Preview represents a previewed refactoring operation.
type Preview struct {
	RefactorID string         `json:"refactor_id"`
	Type       string         `json:"type"`
	OldName    string         `json:"old_name"`
	NewName    string         `json:"new_name"`
	Edits      []Edit         `json:"edits"`
	Stats      map[string]int `json:"stats"`
	CreatedAt  float64        `json:"created_at"`
}

// Edit represents a single text replacement in a file.
type Edit struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	Old        string `json:"old"`
	New        string `json:"new"`
	Confidence string `json:"confidence"`
}

func genID() string {
	const chars = "abcdef0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func cleanupExpired() {
	now := float64(time.Now().Unix())
	for rid, p := range pending {
		if now-p.CreatedAt > float64(ExpirySeconds) {
			delete(pending, rid)
		}
	}
}

// RenamePreview builds a rename edit list for oldName -> newName.
func RenamePreview(store *graph.Store, oldName, newName string) (*Preview, error) {
	nodes, err := store.SearchNodes(oldName, 10)
	if err != nil {
		return nil, err
	}

	var target *graph.GraphNode
	for i := range nodes {
		if nodes[i].Name == oldName {
			target = &nodes[i]
			break
		}
	}
	if target == nil && len(nodes) > 0 {
		target = &nodes[0]
	}
	if target == nil {
		return nil, fmt.Errorf("node %q not found", oldName)
	}

	var edits []Edit
	seen := make(map[string]struct{})

	// Definition site
	edits = append(edits, Edit{
		File: target.FilePath, Line: target.LineStart,
		Old: oldName, New: newName, Confidence: "high",
	})
	seen[fmt.Sprintf("%s:%d", target.FilePath, target.LineStart)] = struct{}{}

	// Call sites and import sites
	edges, _ := store.GetEdgesByTarget(target.QualifiedName)
	for _, e := range edges {
		key := fmt.Sprintf("%s:%d", e.FilePath, e.Line)
		if _, ok := seen[key]; ok {
			continue
		}
		if e.Kind == "CALLS" || e.Kind == "IMPORTS_FROM" {
			edits = append(edits, Edit{
				File: e.FilePath, Line: e.Line,
				Old: oldName, New: newName, Confidence: "high",
			})
			seen[key] = struct{}{}
		}
	}

	stats := map[string]int{"high": 0, "medium": 0, "low": 0}
	for _, e := range edits {
		stats[e.Confidence]++
	}

	refactorID := genID()
	preview := &Preview{
		RefactorID: refactorID,
		Type:       "rename",
		OldName:    graph.SanitizeName(oldName, 0),
		NewName:    graph.SanitizeName(newName, 0),
		Edits:      edits,
		Stats:      stats,
		CreatedAt:  float64(time.Now().Unix()),
	}

	mu.Lock()
	cleanupExpired()
	pending[refactorID] = preview
	mu.Unlock()

	slog.Info("rename preview created", "id", refactorID, "edits", len(edits))
	return preview, nil
}

// FindDeadCode finds functions/classes with no callers, test refs, importers, or references.
func FindDeadCode(store *graph.Store, kind, filePattern string) ([]map[string]any, error) {
	db := store.DB()

	query := "SELECT id, kind, name, qualified_name, file_path, line_start, line_end, is_test, extra FROM nodes WHERE kind IN ('Function', 'Class')"
	var args []any
	if kind != "" {
		query = "SELECT id, kind, name, qualified_name, file_path, line_start, line_end, is_test, extra FROM nodes WHERE kind = ?"
		args = append(args, kind)
	}
	if filePattern != "" {
		query += " AND file_path LIKE ?"
		args = append(args, "%"+filePattern+"%")
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var dead []map[string]any
	for rows.Next() {
		var n graph.GraphNode
		var isTest int
		var extraStr string
		if err := rows.Scan(&n.ID, &n.Kind, &n.Name, &n.QualifiedName, &n.FilePath, &n.LineStart, &n.LineEnd, &isTest, &extraStr); err != nil {
			continue
		}
		n.IsTest = isTest != 0
		if n.IsTest {
			continue
		}

		if flows.HasFrameworkDecorator(n) || flows.MatchesEntryName(n) {
			continue
		}

		// Check incoming edges
		var hasCaller, hasTest, hasImporter, hasRef bool
		eRows, _ := db.Query("SELECT kind FROM edges WHERE target_qualified = ?", n.QualifiedName)
		if eRows != nil {
			for eRows.Next() {
				var ek string
				eRows.Scan(&ek)
				switch ek {
				case "CALLS":
					hasCaller = true
				case "TESTED_BY":
					hasTest = true
				case "IMPORTS_FROM":
					hasImporter = true
				case "REFERENCES":
					hasRef = true
				}
			}
			eRows.Close()
		}

		if !hasCaller && !hasTest && !hasImporter && !hasRef {
			dead = append(dead, map[string]any{
				"name":           graph.SanitizeName(n.Name, 0),
				"qualified_name": graph.SanitizeName(n.QualifiedName, 0),
				"kind":           n.Kind,
				"file":           n.FilePath,
				"line":           n.LineStart,
			})
		}
	}
	return dead, nil
}

// SuggestRefactorings produces community-driven refactoring suggestions.
func SuggestRefactorings(store *graph.Store) ([]map[string]any, error) {
	var suggestions []map[string]any

	// Dead code suggestions
	dead, err := FindDeadCode(store, "", "")
	if err != nil {
		return nil, err
	}
	for _, d := range dead {
		suggestions = append(suggestions, map[string]any{
			"type":        "remove",
			"description": fmt.Sprintf("Remove unused %s '%s'", strings.ToLower(d["kind"].(string)), d["name"]),
			"symbols":     []string{d["qualified_name"].(string)},
			"rationale":   "No callers, no test references, no importers, not an entry point.",
		})
	}

	// Cross-community move suggestions
	db := store.DB()
	cRows, err := db.Query("SELECT id, name FROM communities")
	if err == nil {
		defer cRows.Close()
		communityNames := make(map[int64]string)
		var communityIDs []int64
		for cRows.Next() {
			var id int64
			var name string
			if cRows.Scan(&id, &name) == nil {
				communityNames[id] = name
				communityIDs = append(communityIDs, id)
			}
		}

		if len(communityIDs) > 0 {
			// Build node -> community mapping
			nodeCommunity := make(map[string]int64)
			nRows, _ := db.Query("SELECT qualified_name, community_id FROM nodes WHERE community_id IS NOT NULL")
			if nRows != nil {
				for nRows.Next() {
					var qn string
					var cid int64
					if nRows.Scan(&qn, &cid) == nil {
						nodeCommunity[qn] = cid
					}
				}
				nRows.Close()
			}

			// Check functions called only by members of a different community
			fRows, _ := db.Query("SELECT qualified_name, name FROM nodes WHERE kind = 'Function'")
			if fRows != nil {
				for fRows.Next() {
					var qn, name string
					if fRows.Scan(&qn, &name) != nil {
						continue
					}
					fCom, ok := nodeCommunity[qn]
					if !ok {
						continue
					}

					eRows, _ := db.Query("SELECT source_qualified FROM edges WHERE target_qualified = ? AND kind = 'CALLS'", qn)
					if eRows == nil {
						continue
					}
					callerComs := make(map[int64]struct{})
					hasCaller := false
					for eRows.Next() {
						var src string
						eRows.Scan(&src)
						hasCaller = true
						if cc, ok := nodeCommunity[src]; ok {
							callerComs[cc] = struct{}{}
						}
					}
					eRows.Close()

					if !hasCaller || len(callerComs) != 1 {
						continue
					}
					for tgtCom := range callerComs {
						if tgtCom != fCom {
							suggestions = append(suggestions, map[string]any{
								"type":        "move",
								"description": fmt.Sprintf("Move '%s' from '%s' to '%s'", graph.SanitizeName(name, 0), communityNames[fCom], communityNames[tgtCom]),
								"symbols":     []string{graph.SanitizeName(qn, 0)},
								"rationale":   fmt.Sprintf("Function is in community '%s' but only called by members of community '%s'.", communityNames[fCom], communityNames[tgtCom]),
							})
						}
					}
				}
				fRows.Close()
			}
		}
	}

	return suggestions, nil
}

// ApplyRefactor applies a previously previewed refactoring to source files.
func ApplyRefactor(refactorID string, repoRoot string, dryRun bool) map[string]any {
	absRoot, _ := filepath.Abs(repoRoot)

	mu.Lock()
	cleanupExpired()
	preview, ok := pending[refactorID]
	mu.Unlock()

	if !ok {
		return map[string]any{"status": "error", "error": fmt.Sprintf("Refactor '%s' not found or expired.", refactorID)}
	}

	age := float64(time.Now().Unix()) - preview.CreatedAt
	if age > float64(ExpirySeconds) {
		mu.Lock()
		delete(pending, refactorID)
		mu.Unlock()
		return map[string]any{"status": "error", "error": fmt.Sprintf("Refactor '%s' has expired.", refactorID)}
	}

	edits := preview.Edits
	if len(edits) == 0 {
		return map[string]any{"status": "ok", "applied": 0, "files_modified": []string{}, "edits_applied": 0}
	}

	// Path traversal validation
	for _, edit := range edits {
		absEdit, _ := filepath.Abs(edit.File)
		if !strings.HasPrefix(absEdit, absRoot) {
			return map[string]any{"status": "error", "error": fmt.Sprintf("Edit path '%s' is outside repo root.", edit.File)}
		}
	}

	// Group edits by file
	editsByFile := make(map[string][]Edit)
	for _, e := range edits {
		editsByFile[e.File] = append(editsByFile[e.File], e)
	}

	// Compute new content for each file
	type planned struct {
		original string
		content  string
		count    int
	}
	plans := make(map[string]planned)
	for file, fileEdits := range editsByFile {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		content := string(data)
		original := content
		editCount := 0
		for _, edit := range fileEdits {
			if !strings.Contains(content, edit.Old) {
				continue
			}
			if edit.Line > 0 {
				lines := strings.SplitAfter(content, "\n")
				idx := edit.Line - 1
				if idx >= 0 && idx < len(lines) && strings.Contains(lines[idx], edit.Old) {
					lines[idx] = strings.Replace(lines[idx], edit.Old, edit.New, 1)
					content = strings.Join(lines, "")
				} else {
					content = strings.Replace(content, edit.Old, edit.New, 1)
				}
			} else {
				content = strings.Replace(content, edit.Old, edit.New, 1)
			}
			editCount++
		}
		if editCount > 0 {
			plans[file] = planned{original, content, editCount}
		}
	}

	if dryRun {
		wouldModify := make([]string, 0, len(plans))
		for f := range plans {
			wouldModify = append(wouldModify, f)
		}
		totalEdits := 0
		for _, p := range plans {
			totalEdits += p.count
		}
		return map[string]any{
			"status":        "ok",
			"dry_run":       true,
			"applied":       0,
			"edits_applied": totalEdits,
			"would_modify":  wouldModify,
			"files_modified": []string{},
		}
	}

	// Write files
	filesModified := make(map[string]struct{})
	editsApplied := 0
	for file, p := range plans {
		if err := os.WriteFile(file, []byte(p.content), 0o644); err != nil {
			slog.Error("apply refactor: write failed", "file", file, "err", err)
			continue
		}
		editsApplied += p.count
		filesModified[file] = struct{}{}
	}

	mu.Lock()
	delete(pending, refactorID)
	mu.Unlock()

	modList := make([]string, 0, len(filesModified))
	for f := range filesModified {
		modList = append(modList, f)
	}

	return map[string]any{
		"status":         "ok",
		"applied":        editsApplied,
		"files_modified": modList,
		"edits_applied":  editsApplied,
	}
}
