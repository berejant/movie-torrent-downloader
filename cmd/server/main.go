// Command server runs the movie torrent downloader: an HTTP UI for scheduling
// tracker searches and a worker pool that saves the resulting .torrent files.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/toxa/movie-torrent-downloader/internal/config"
	"github.com/toxa/movie-torrent-downloader/internal/storage"
	"github.com/toxa/movie-torrent-downloader/internal/tracker"
	"github.com/toxa/movie-torrent-downloader/internal/web"
	"github.com/toxa/movie-torrent-downloader/internal/worker"
)

func main() {
	if err := run(); err != nil {
		// Configuration problems arrive here before the logger exists, so the
		// message goes to stderr in plain text where Docker logs will show it.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	slog.SetDefault(logger)
	logger.Info("starting movie torrent downloader", "config", cfg)

	// The output directory must exist and be writable before anything else:
	// on Synology this is where a wrong PUID/PGID shows up.
	if err := os.MkdirAll(cfg.TorrentFilesDir, 0o755); err != nil {
		return fmt.Errorf("create TORRENT_FILES_DIR %s: %w", cfg.TorrentFilesDir, err)
	}
	if err := ensureParentDir(cfg.DBPath); err != nil {
		return err
	}

	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	clients := make([]*tracker.Client, 0, len(cfg.Trackers))
	for _, trackerCfg := range cfg.Trackers {
		client, err := tracker.New(trackerCfg, logger)
		if err != nil {
			return err
		}
		clients = append(clients, client)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool := worker.New(store, clients, cfg, logger)
	if err := pool.Recover(ctx); err != nil {
		return err
	}
	pool.Start(ctx)

	server, err := web.New(store, cfg, pool, logger)
	if err != nil {
		return err
	}

	err = server.Start(ctx)

	// The HTTP server has stopped; let the workers finish their current task.
	logger.Info("waiting for workers to stop")
	pool.Wait()
	logger.Info("shutdown complete")

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// ensureParentDir creates the directory holding the SQLite file.
func ensureParentDir(path string) error {
	dir := dirOf(path)
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create DB_PATH directory %s: %w", dir, err)
	}
	return nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == os.PathSeparator {
			return path[:i]
		}
	}
	return ""
}
