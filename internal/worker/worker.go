// Package worker runs the search-and-download pipeline.
//
// Workers are stateless: everything they need lives in the request row, and
// every retry is persisted as next_attempt_at rather than slept in memory.
// That is what lets a restart pick the schedule back up instead of losing it.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/toxa/movie-torrent-downloader/internal/config"
	"github.com/toxa/movie-torrent-downloader/internal/storage"
	"github.com/toxa/movie-torrent-downloader/internal/tracker"
)

// idlePoll is how often an idle worker re-checks the queue when no submission
// has woken it. Notify() makes new work visible immediately; this is only the
// safety net that picks up due retries.
const idlePoll = 2 * time.Second

// Pool is the set of workers serving one tracker.
type Pool struct {
	store   *storage.Store
	tracker *tracker.Client
	cfg     config.Config
	logger  *slog.Logger

	wake chan struct{}
	wg   sync.WaitGroup
}

// New creates the pool. Call Start to run it.
func New(store *storage.Store, client *tracker.Client, cfg config.Config, logger *slog.Logger) *Pool {
	return &Pool{
		store:   store,
		tracker: client,
		cfg:     cfg,
		logger:  logger.With("component", "worker"),
		wake:    make(chan struct{}, 1),
	}
}

// Recover prepares the queue after a restart: tasks that were mid-flight go
// back to QUEUED, and half-written temp files are removed. Tasks are short and
// cheap to repeat, so they restart from the beginning rather than resuming.
func (p *Pool) Recover(ctx context.Context) error {
	requeued, err := p.store.RequeueInFlight(ctx)
	if err != nil {
		return err
	}
	if requeued > 0 {
		p.logger.Info("re-queued in-flight requests after restart", "count", requeued)
	}

	removed, err := cleanTempFiles(p.cfg.TorrentFilesDir)
	if err != nil {
		// A dirty temp file is not worth refusing to start over.
		p.logger.Warn("could not clean temporary files", "err", err)
	} else if removed > 0 {
		p.logger.Info("removed orphaned temporary files", "count", removed)
	}

	return nil
}

// Start launches the configured number of workers and returns immediately.
func (p *Pool) Start(ctx context.Context) {
	for i := range p.cfg.Tracker.Workers {
		p.wg.Add(1)
		go p.run(ctx, i+1)
	}
	p.logger.Info("worker pool started",
		"workers", p.cfg.Tracker.Workers,
		"rps", p.cfg.Tracker.RPS,
	)
}

// Wait blocks until every worker has stopped.
func (p *Pool) Wait() { p.wg.Wait() }

// Notify tells the pool that new work may be available. It never blocks.
func (p *Pool) Notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Pool) run(ctx context.Context, id int) {
	defer p.wg.Done()

	logger := p.logger.With("worker", id)
	timer := time.NewTimer(idlePoll)
	defer timer.Stop()

	for {
		request, ok, err := p.store.ClaimNext(ctx, p.cfg.Tracker.Name)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			logger.Error("claim failed", "err", err)
		case ok:
			p.process(ctx, logger, request)
			continue // keep draining while work remains
		}

		// Nothing to do: sleep until woken, until the next poll, or shutdown.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(idlePoll)

		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-timer.C:
		}
	}
}

// process runs one request end to end.
func (p *Pool) process(ctx context.Context, logger *slog.Logger, request storage.Request) {
	logger = logger.With("request_id", request.ID, "query", request.Query)
	logger.Info("processing request", "attempt", request.AttemptCount)

	// Bound a single attempt so one stuck request cannot hold a worker forever.
	taskCtx, cancel := context.WithTimeout(ctx, 4*p.cfg.Tracker.Timeout())
	defer cancel()

	torrents, err := p.tracker.Search(taskCtx, request.Query)
	if err != nil {
		p.handleError(ctx, logger, request, err, "search")
		return
	}

	best, err := p.tracker.SelectBest(torrents)
	if err != nil {
		p.handleError(ctx, logger, request, err, "select")
		return
	}

	result := storage.Result{
		Title:    best.Title,
		TopicURL: best.TopicURL,
		Size:     best.SizeBytes,
		Quality:  best.Quality,
		Codec:    best.Codec,
	}
	if err := p.store.MarkFound(ctx, request.ID, result); err != nil {
		logger.Error("could not record found result", "err", err)
		return
	}
	logger.Info("selected release",
		"title", best.Title,
		"quality", best.Quality,
		"codec", best.Codec,
		"candidates", len(torrents),
	)

	path, err := p.tracker.Download(taskCtx, best, p.cfg.TorrentFilesDir, request.ID)
	if err != nil {
		p.handleError(ctx, logger, request, err, "download")
		return
	}

	if err := p.store.MarkDownloaded(ctx, request.ID, path); err != nil {
		logger.Error("could not record download", "err", err)
		return
	}
	logger.Info("saved torrent file", "path", path)
}

// handleError maps a pipeline failure onto the right terminal or retry state.
func (p *Pool) handleError(
	ctx context.Context,
	logger *slog.Logger,
	request storage.Request,
	err error,
	stage string,
) {
	// A shutdown is not a task failure. Leaving the request in SEARCHING means
	// Recover picks it up on the next start.
	if ctx.Err() != nil {
		logger.Info("shutting down mid-request, leaving it for restart", "stage", stage)
		return
	}

	switch {
	case errors.Is(err, tracker.ErrNoResults):
		logger.Info("no matching release", "stage", stage)
		if err := p.store.MarkNotFound(ctx, request.ID, "tracker returned no usable match"); err != nil {
			logger.Error("could not record not-found", "err", err)
		}
		return

	case !tracker.IsTransient(err):
		logger.Warn("permanent failure", "stage", stage, "err", err)
		if err := p.store.MarkFailed(ctx, request.ID, storage.TrimError(err)); err != nil {
			logger.Error("could not record failure", "err", err)
		}
		return
	}

	if request.AttemptCount >= p.cfg.Retry.MaxAttempts {
		logger.Warn("retries exhausted", "stage", stage, "attempts", request.AttemptCount, "err", err)
		if err := p.store.MarkFailed(ctx, request.ID, storage.TrimError(err)); err != nil {
			logger.Error("could not record failure", "err", err)
		}
		return
	}

	// AttemptCount was incremented when the request was claimed, so the next
	// attempt's delay is the one for the attempt we are about to schedule.
	delay := storage.Backoff(request.AttemptCount, p.cfg.Retry.BaseSeconds, p.cfg.Retry.MaxBackoffSeconds)
	next := time.Now().UTC().Add(delay)

	logger.Info("scheduling retry",
		"stage", stage,
		"attempt", request.AttemptCount,
		"delay", delay.Round(time.Second),
		"err", err,
	)
	if err := p.store.ScheduleRetry(ctx, request.ID, next, storage.TrimError(err)); err != nil {
		logger.Error("could not schedule retry", "err", err)
	}
}

// cleanTempFiles removes leftovers from an interrupted download. Nothing else
// is running at startup, so every match is stale by definition.
func cleanTempFiles(dir string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(dir, ".torrent-*.tmp"))
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, path := range matches {
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	return removed, nil
}
