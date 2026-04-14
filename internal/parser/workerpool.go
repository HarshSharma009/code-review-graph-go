package parser

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/harshsharma/code-review-graph-go/internal/graph"
)

// WorkerPool manages a pool of goroutines for parallel file parsing.
// Each goroutine holds its own CodeParser instance (Tree-sitter parsers are
// thread-safe at the object level, but we avoid sharing to eliminate lock contention).
type WorkerPool struct {
	numWorkers int
}

func NewWorkerPool(numWorkers int) *WorkerPool {
	if numWorkers <= 0 {
		numWorkers = runtime.NumCPU()
	}
	return &WorkerPool{numWorkers: numWorkers}
}

type FileJob struct {
	RelPath  string
	RepoRoot string
}

// ParseAll parses files concurrently and returns results via a channel.
// The caller should range over the returned channel until it is closed.
func (wp *WorkerPool) ParseAll(ctx context.Context, jobs []FileJob) <-chan ParseResult {
	results := make(chan ParseResult, wp.numWorkers*2)
	jobsCh := make(chan FileJob, wp.numWorkers)

	var wg sync.WaitGroup
	var parsed atomic.Int64

	for i := 0; i < wp.numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			parser := NewCodeParser()
			defer parser.Close()

			for job := range jobsCh {
				if ctx.Err() != nil {
					return
				}

				absPath := job.RepoRoot + "/" + job.RelPath
				data, err := os.ReadFile(absPath)
				if err != nil {
					results <- ParseResult{
						FilePath: job.RelPath,
						Err:      err,
					}
					continue
				}

				fhash := FileHash(data)
				nodes, edges, err := parser.ParseBytes(ctx, absPath, data)

				results <- ParseResult{
					FilePath: job.RelPath,
					Nodes:    nodes,
					Edges:    edges,
					FileHash: fhash,
					Err:      err,
				}

				count := parsed.Add(1)
				if count%200 == 0 {
					slog.Info("parse progress", "completed", count, "total", len(jobs))
				}
			}
		}()
	}

	go func() {
		defer close(jobsCh)
		for _, job := range jobs {
			select {
			case jobsCh <- job:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	return results
}

// ParseSingle parses a single file synchronously. Used for small file counts
// or serial mode.
func ParseSingle(ctx context.Context, repoRoot, relPath string) ParseResult {
	absPath := repoRoot + "/" + relPath
	data, err := os.ReadFile(absPath)
	if err != nil {
		return ParseResult{FilePath: relPath, Err: err}
	}

	fhash := FileHash(data)
	parser := NewCodeParser()
	defer parser.Close()

	nodes, edges, err := parser.ParseBytes(ctx, absPath, data)
	return ParseResult{
		FilePath: relPath,
		Nodes:    nodes,
		Edges:    edges,
		FileHash: fhash,
		Err:      err,
	}
}

// ParseFileToStore is a convenience that parses a single file and stores
// the result. Used by the incremental watcher.
func ParseFileToStore(ctx context.Context, store *graph.Store, absPath string, source []byte) (int, int, error) {
	fhash := FileHash(source)
	parser := NewCodeParser()
	defer parser.Close()

	nodes, edges, err := parser.ParseBytes(ctx, absPath, source)
	if err != nil {
		return 0, 0, err
	}

	if err := store.StoreFileNodesEdges(absPath, nodes, edges, fhash); err != nil {
		return 0, 0, err
	}

	return len(nodes), len(edges), nil
}
