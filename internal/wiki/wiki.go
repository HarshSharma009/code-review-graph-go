package wiki

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/harshsharma/code-review-graph-go/internal/flows"
	"github.com/harshsharma/code-review-graph-go/internal/graph"
)

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	slug := slugRe.ReplaceAllString(strings.ToLower(name), "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 80 {
		slug = slug[:80]
	}
	if slug == "" {
		slug = "unnamed"
	}
	return slug
}

// Community represents a detected code community from the DB.
type Community struct {
	ID               int64
	Name             string
	Size             int
	Cohesion         float64
	DominantLanguage string
	Description      string
	Members          []string
}

// GetCommunities loads all communities and their members from the store.
func GetCommunities(store *graph.Store) ([]Community, error) {
	db := store.DB()
	rows, err := db.Query("SELECT id, name, size, cohesion, dominant_language, description FROM communities ORDER BY size DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var communities []Community
	for rows.Next() {
		var c Community
		var desc, lang *string
		if err := rows.Scan(&c.ID, &c.Name, &c.Size, &c.Cohesion, &lang, &desc); err != nil {
			continue
		}
		if desc != nil {
			c.Description = *desc
		}
		if lang != nil {
			c.DominantLanguage = *lang
		}

		// Load members
		mRows, err := db.Query("SELECT qualified_name FROM nodes WHERE community_id = ?", c.ID)
		if err == nil {
			for mRows.Next() {
				var qn string
				if mRows.Scan(&qn) == nil {
					c.Members = append(c.Members, qn)
				}
			}
			mRows.Close()
		}
		communities = append(communities, c)
	}
	return communities, nil
}

func generateCommunityPage(store *graph.Store, c Community) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("# %s", c.Name), "")

	// Overview
	lines = append(lines, "## Overview", "")
	if c.Description != "" {
		lines = append(lines, c.Description, "")
	}
	lines = append(lines, fmt.Sprintf("- **Size**: %d nodes", c.Size))
	lines = append(lines, fmt.Sprintf("- **Cohesion**: %.4f", c.Cohesion))
	if c.DominantLanguage != "" {
		lines = append(lines, fmt.Sprintf("- **Dominant Language**: %s", c.DominantLanguage))
	}
	lines = append(lines, "")

	// Members table (top 50)
	lines = append(lines, "## Members", "")
	if len(c.Members) > 0 {
		lines = append(lines, "| Name | Kind | File | Lines |")
		lines = append(lines, "|------|------|------|-------|")
		memberCount := 0
		limit := 50
		if len(c.Members) < limit {
			limit = len(c.Members)
		}
		db := store.DB()
		for _, qn := range c.Members[:limit] {
			var name, kind, fp string
			var ls, le int
			if db.QueryRow(
				"SELECT name, kind, file_path, line_start, line_end FROM nodes WHERE qualified_name = ?", qn,
			).Scan(&name, &kind, &fp, &ls, &le) == nil && kind != "File" {
				lines = append(lines, fmt.Sprintf("| %s | %s | %s | %d-%d |",
					graph.SanitizeName(name, 0), kind, fp, ls, le))
				memberCount++
			}
		}
		if memberCount == 0 {
			lines = lines[:len(lines)-2]
			lines = append(lines, "No non-file members found.")
		}
		if len(c.Members) > 50 {
			lines = append(lines, "", fmt.Sprintf("*... and %d more members.*", len(c.Members)-50))
		}
	} else {
		lines = append(lines, "No members found.")
	}
	lines = append(lines, "")

	// Execution flows
	lines = append(lines, "## Execution Flows", "")
	memberSet := make(map[string]struct{})
	for _, m := range c.Members {
		memberSet[m] = struct{}{}
	}
	allFlows, err := flows.GetFlows(store, "criticality", 200)
	if err == nil && len(allFlows) > 0 {
		var communityFlows []flows.Flow
		db := store.DB()
		for _, f := range allFlows {
			// Check if flow nodes overlap with community members
			for _, nid := range f.Path {
				var qn string
				if db.QueryRow("SELECT qualified_name FROM nodes WHERE id = ?", nid).Scan(&qn) == nil {
					if _, ok := memberSet[qn]; ok {
						communityFlows = append(communityFlows, f)
						break
					}
				}
			}
		}
		if len(communityFlows) > 0 {
			limit := 10
			if len(communityFlows) < limit {
				limit = len(communityFlows)
			}
			for _, f := range communityFlows[:limit] {
				lines = append(lines, fmt.Sprintf("- **%s** (criticality: %.2f, depth: %d)",
					graph.SanitizeName(f.Name, 0), f.Criticality, f.Depth))
			}
			if len(communityFlows) > 10 {
				lines = append(lines, fmt.Sprintf("- *... and %d more flows.*", len(communityFlows)-10))
			}
		} else {
			lines = append(lines, "No execution flows pass through this community.")
		}
	} else {
		lines = append(lines, "Execution flow data not available.")
	}
	lines = append(lines, "")

	// Dependencies
	lines = append(lines, "## Dependencies", "")
	outgoing := make(map[string]int)
	incoming := make(map[string]int)
	db2 := store.DB()
	for _, qn := range c.Members {
		eRows, _ := db2.Query("SELECT target_qualified FROM edges WHERE source_qualified = ?", qn)
		if eRows != nil {
			for eRows.Next() {
				var t string
				eRows.Scan(&t)
				if _, ok := memberSet[t]; !ok {
					outgoing[t]++
				}
			}
			eRows.Close()
		}
		eRows, _ = db2.Query("SELECT source_qualified FROM edges WHERE target_qualified = ?", qn)
		if eRows != nil {
			for eRows.Next() {
				var s string
				eRows.Scan(&s)
				if _, ok := memberSet[s]; !ok {
					incoming[s]++
				}
			}
			eRows.Close()
		}
	}

	if len(outgoing) > 0 {
		lines = append(lines, "### Outgoing", "")
		for _, entry := range topN(outgoing, 15) {
			lines = append(lines, fmt.Sprintf("- `%s` (%d edge(s))", graph.SanitizeName(entry.key, 0), entry.count))
		}
		lines = append(lines, "")
	}
	if len(incoming) > 0 {
		lines = append(lines, "### Incoming", "")
		for _, entry := range topN(incoming, 15) {
			lines = append(lines, fmt.Sprintf("- `%s` (%d edge(s))", graph.SanitizeName(entry.key, 0), entry.count))
		}
		lines = append(lines, "")
	}
	if len(outgoing) == 0 && len(incoming) == 0 {
		lines = append(lines, "No cross-community dependencies detected.", "")
	}

	return strings.Join(lines, "\n")
}

// GenerateResult holds counts from wiki generation.
type GenerateResult struct {
	PagesGenerated int `json:"pages_generated"`
	PagesUpdated   int `json:"pages_updated"`
	PagesUnchanged int `json:"pages_unchanged"`
}

// GenerateWiki generates a markdown wiki from the community structure.
func GenerateWiki(store *graph.Store, wikiDir string) (*GenerateResult, error) {
	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating wiki dir: %w", err)
	}

	communities, err := GetCommunities(store)
	if err != nil {
		return nil, fmt.Errorf("loading communities: %w", err)
	}

	result := &GenerateResult{}
	type pageEntry struct {
		slug string
		name string
		size int
	}
	var pages []pageEntry
	usedSlugs := make(map[string]struct{})

	for _, c := range communities {
		baseSlug := slugify(c.Name)
		slug := baseSlug
		suffix := 2
		for {
			if _, ok := usedSlugs[slug]; !ok {
				break
			}
			slug = fmt.Sprintf("%s-%d", baseSlug, suffix)
			suffix++
		}
		usedSlugs[slug] = struct{}{}

		filename := slug + ".md"
		fpath := filepath.Join(wikiDir, filename)
		content := generateCommunityPage(store, c)

		if existing, err := os.ReadFile(fpath); err == nil {
			if string(existing) == content {
				result.PagesUnchanged++
				pages = append(pages, pageEntry{slug, c.Name, c.Size})
				continue
			}
			result.PagesUpdated++
		} else {
			result.PagesGenerated++
		}

		if err := os.WriteFile(fpath, []byte(content), 0o644); err != nil {
			slog.Warn("failed to write wiki page", "path", fpath, "err", err)
		}
		pages = append(pages, pageEntry{slug, c.Name, c.Size})
	}

	// Generate index.md
	sort.Slice(pages, func(i, j int) bool { return pages[i].name < pages[j].name })

	var idx []string
	idx = append(idx, "# Code Wiki", "")
	idx = append(idx, "Auto-generated documentation from the code knowledge graph community structure.", "")
	idx = append(idx, fmt.Sprintf("**Total communities**: %d", len(communities)), "")
	idx = append(idx, "## Communities", "")
	idx = append(idx, "| Community | Size | Link |")
	idx = append(idx, "|-----------|------|------|")
	for _, p := range pages {
		idx = append(idx, fmt.Sprintf("| %s | %d | [%s.md](%s.md) |", p.name, p.size, p.slug, p.slug))
	}
	idx = append(idx, "")

	indexContent := strings.Join(idx, "\n")
	indexPath := filepath.Join(wikiDir, "index.md")
	if existing, err := os.ReadFile(indexPath); err == nil {
		if string(existing) == indexContent {
			result.PagesUnchanged++
		} else {
			os.WriteFile(indexPath, []byte(indexContent), 0o644) //nolint:errcheck
			result.PagesUpdated++
		}
	} else {
		os.WriteFile(indexPath, []byte(indexContent), 0o644) //nolint:errcheck
		result.PagesGenerated++
	}

	return result, nil
}

// GetWikiPage retrieves a specific wiki page by community name.
func GetWikiPage(wikiDir, pageName string) (string, error) {
	slug := slugify(pageName)
	fpath := filepath.Join(wikiDir, slug+".md")
	if data, err := os.ReadFile(fpath); err == nil {
		return string(data), nil
	}

	// Fallback: search for partial match
	entries, err := os.ReadDir(wikiDir)
	if err != nil {
		return "", fmt.Errorf("reading wiki dir: %w", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") && strings.Contains(e.Name(), slug) {
			data, err := os.ReadFile(filepath.Join(wikiDir, e.Name()))
			if err == nil {
				return string(data), nil
			}
		}
	}
	return "", fmt.Errorf("wiki page %q not found", pageName)
}

type kv struct {
	key   string
	count int
}

func topN(m map[string]int, n int) []kv {
	pairs := make([]kv, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].count > pairs[j].count })
	if len(pairs) > n {
		pairs = pairs[:n]
	}
	return pairs
}
