package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/godwit-email/godwit/core/store"
)

// The fork rewrites' backfill (FORK.md, "Fork rewrites"; ADR 0053) runs in
// the background after Open, one short transaction at a time, so a click
// waits for at most one batch. A batch aims at rewriteBudget of work and
// carries at most rewriteBytes of data; the row count adapts to the time
// the last batch took.
const (
	rewriteBudget  = 50 * time.Millisecond
	rewriteBytes   = 2 << 20
	rewriteMaxRows = 256
	rewritePause   = 25 * time.Millisecond // between two batches: other statements run
	rewriteRetry   = time.Minute           // after an error, or while the disk is full
	rewriteMinFree = 512 << 20             // the move needs about one batch and the WAL: stop well before full
)

// startForkRewrites runs every unfinished rewrite's backfill to its end, or
// until the service closes; the next start goes on from where it stopped.
func (s *Service) startForkRewrites() {
	ctx, end, err := s.begin(context.Background(), 0)
	if err != nil {
		return
	}
	go func() {
		defer end()
		for _, r := range store.ForkRewrites {
			s.runRewrite(ctx, r)
		}
	}()
}

func (s *Service) runRewrite(ctx context.Context, r store.ForkRewrite) {
	log := slog.Default().With("rewrite", r.Name)
	state, err := s.db.RewriteState(ctx, r.Name)
	if err != nil || state != store.RewriteMoving {
		return
	}
	dir := ""
	if path, err := s.db.Path(ctx); err == nil && path != "" {
		dir = filepath.Dir(path)
	}
	log.Info("fork rewrite: moving data in the background")
	var (
		start       = time.Now()
		rowsMoved   int
		bytesMoved  int64
		batches     int
		rows        = 32
		diskWarned  bool
		errorWarned bool
	)
	for {
		if dir != "" {
			if free, ok := freeBytes(dir); ok && free < rewriteMinFree {
				if !diskWarned {
					log.Warn("fork rewrite: paused, the disk is almost full", "free_mb", free>>20)
					diskWarned = true
				}
				if !sleepCtx(ctx, rewriteRetry) {
					return
				}
				continue
			}
		}
		t := time.Now()
		res, err := r.Backfill(ctx, s.db, store.Batch{Rows: rows, Bytes: rewriteBytes})
		took := time.Since(t)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !errorWarned {
				log.Warn("fork rewrite: a batch failed; trying again later", "err", err)
				errorWarned = true
			}
			if !sleepCtx(ctx, rewriteRetry) {
				return
			}
			continue
		}
		batches++
		rowsMoved += res.Rows
		bytesMoved += res.Bytes
		if !res.More {
			log.Info("fork rewrite: done", "rows", rowsMoved, "mb", bytesMoved>>20, "batches", batches, "took", time.Since(start).Round(time.Millisecond))
			return
		}
		rows = nextBatchRows(rows, res, took)
		if !sleepCtx(ctx, rewritePause) {
			return
		}
	}
}

// nextBatchRows is the row count of the next batch: scaled down to fit
// rewriteBudget after a slow one, doubled after a quick one that the byte
// limit did not cut.
func nextBatchRows(rows int, res store.BatchResult, took time.Duration) int {
	switch {
	case took > rewriteBudget:
		rows = max(1, int(int64(rows)*int64(rewriteBudget)/int64(took)))
	case took < rewriteBudget/2 && res.Rows >= rows && res.Bytes < rewriteBytes/2:
		rows = min(rewriteMaxRows, rows*2)
	}
	return rows
}

// sleepCtx waits d, false when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
