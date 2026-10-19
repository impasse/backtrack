package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"backtrack/internal/api"
	"backtrack/internal/clips"
	"backtrack/internal/config"
	"backtrack/internal/events"
	"backtrack/internal/ingest"
	"backtrack/internal/retention"
	"backtrack/internal/storage"
	"backtrack/internal/thumbnail"
)

func main() {
	configPath := flag.String("config", "configs/dvr.yaml", "path to YAML configuration")
	flag.Parse()
	bootstrapLogger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(*configPath)
	if err != nil {
		bootstrapLogger.Error("configuration invalid", "error", err)
		os.Exit(2)
	}
	logger := configuredLogger(cfg)
	store, err := storage.Open(cfg.Storage.Root, cfg.Storage.Database)
	if err != nil {
		logger.Error("open storage", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.Recovery.ScanOnStartup {
		if err = store.RecoverWithOptions(ctx, cfg.Recovery.IntegrityCheck, cfg.Recovery.QuarantineInvalidSegments); err != nil {
			logger.Error("startup recovery failed; refusing ingest", "error", err)
			os.Exit(1)
		}
	} else if cfg.Recovery.IntegrityCheck {
		if err = store.IntegrityCheck(ctx); err != nil {
			logger.Error("database integrity check failed; refusing ingest", "error", err)
			os.Exit(1)
		}
	}
	if err = store.EndIncompleteSessions(ctx); err != nil {
		logger.Error("recover sessions", "error", err)
		os.Exit(1)
	}
	eventHub := events.New()
	ingestServer := ingest.New(cfg.Server.RTMPListen, cfg, store, logger, eventHub)
	thumbnailService := thumbnail.New(store, logger)
	ingestServer.SetSegmentCommittedHandler(thumbnailService.Enqueue)
	clipService := clips.New(cfg, store, logger, eventHub)
	httpServer := api.New(cfg, store, logger, ingestServer, clipService, eventHub)
	retentionManager := retention.New(cfg, store, logger, eventHub)
	var background sync.WaitGroup
	background.Add(3)
	go func() {
		defer background.Done()
		retentionManager.Run(ctx)
	}()
	go func() {
		defer background.Done()
		clipService.Run(ctx)
	}()
	go func() {
		defer background.Done()
		thumbnailService.Run(ctx)
	}()
	errCh := make(chan error, 2)
	go func() { errCh <- httpServer.Serve(ctx) }()
	go func() { errCh <- ingestServer.ListenAndServe(ctx) }()
	firstErr := <-errCh
	stop()
	secondErr := <-errCh
	background.Wait()
	if firstErr != nil {
		logger.Error("server stopped", "error", firstErr)
		_ = store.Close()
		os.Exit(1)
	}
	if secondErr != nil {
		logger.Error("server stopped", "error", secondErr)
		_ = store.Close()
		os.Exit(1)
	}
}

func configuredLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Logging.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	options := &slog.HandlerOptions{Level: level}
	if cfg.Logging.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, options))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, options))
}
