package incremental

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/harshsharma/code-review-graph-go/internal/graph"
	"github.com/harshsharma/code-review-graph-go/internal/parser"
)

const debounceInterval = 300 * time.Millisecond

// Watch monitors the repository for file changes and auto-updates the graph.
// It uses a debounce window to batch rapid-fire saves into a single update.
func Watch(ctx context.Context, repoRoot string, store *graph.Store) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	ignorePatterns := LoadIgnorePatterns(repoRoot)

	// Add all subdirectories
	err = filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		if ShouldIgnore(rel+"/", ignorePatterns) {
			return filepath.SkipDir
		}
		return watcher.Add(path)
	})
	if err != nil {
		return err
	}

	slog.Info("watching for changes", "path", repoRoot)

	var (
		mu      sync.Mutex
		pending = make(map[string]struct{})
		timer   *time.Timer
	)

	flush := func() {
		mu.Lock()
		paths := make([]string, 0, len(pending))
		for p := range pending {
			paths = append(paths, p)
		}
		pending = make(map[string]struct{})
		mu.Unlock()

		for _, absPath := range paths {
			updateFile(ctx, store, repoRoot, absPath)
		}
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("watch stopped")
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			absPath := event.Name
			rel, err := filepath.Rel(repoRoot, absPath)
			if err != nil {
				continue
			}

			if ShouldIgnore(rel, ignorePatterns) {
				continue
			}

			if event.Has(fsnotify.Remove) {
				if err := store.RemoveFileData(absPath); err == nil {
					slog.Info("removed", "file", rel)
				}
				continue
			}

			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
				continue
			}

			info, err := os.Stat(absPath)
			if err != nil || info.IsDir() {
				continue
			}

			if parser.DetectLanguage(absPath) == "" {
				continue
			}

			mu.Lock()
			pending[absPath] = struct{}{}
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounceInterval, flush)
			mu.Unlock()

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.Error("watcher error", "error", err)
		}
	}
}

func updateFile(ctx context.Context, store *graph.Store, repoRoot, absPath string) {
	info, err := os.Stat(absPath)
	if err != nil || !info.Mode().IsRegular() {
		return
	}

	if isBinary(absPath) {
		return
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		slog.Error("error reading file", "path", absPath, "error", err)
		return
	}

	nNodes, nEdges, err := parser.ParseFileToStore(ctx, store, absPath, data)
	if err != nil {
		slog.Error("error updating file", "path", absPath, "error", err)
		return
	}

	now := time.Now().Format("2006-01-02T15:04:05")
	_ = store.SetMetadata("last_updated", now)

	rel, _ := filepath.Rel(repoRoot, absPath)
	slog.Info("updated", "file", rel, "nodes", nNodes, "edges", nEdges)
}
