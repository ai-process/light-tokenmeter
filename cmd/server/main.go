package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/light-tokenmeter/internal/config"
	"github.com/ai-process/light-tokenmeter/internal/database"
	"github.com/ai-process/light-tokenmeter/internal/logger"
	"github.com/ai-process/light-tokenmeter/internal/repository"
	"github.com/ai-process/light-tokenmeter/internal/server"
)

func main() {
	// Load .env file if present
	if err := godotenv.Load(); err != nil {
		// Not fatal - .env might not exist in production
	}

	// Load configuration
	cfg := config.Load()

	// Initialize logger
	logger.Init(cfg)

	log.Info().
		Str("grpc_addr", cfg.GRPCAddr).
		Str("http_addr", cfg.HTTPAddr).
		Str("log_level", cfg.LogLevel).
		Msg("starting light-tokenmeter service")

	// Create root context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run database migrations
	if err := database.RunMigrations(ctx, cfg.DatabaseURL); err != nil {
		log.Fatal().Err(err).Msg("failed to run database migrations")
	}

	// Connect to database
	pool, err := database.Connect(ctx, database.Config{
		URL:      cfg.DatabaseURL,
		MaxConns: cfg.DBMaxConns,
		MinConns: cfg.DBMinConns,
	})
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
	}
	defer pool.Close()

	// Create repository
	repo := repository.New(pool)

	// Create servers
	grpcServer := server.NewGRPCServer(repo, cfg.GRPCAddr)
	httpServer := server.NewHTTPServer(repo, cfg.HTTPAddr)

	// Start gRPC server in goroutine
	grpcErrCh := make(chan error, 1)
	go func() {
		if err := grpcServer.Start(); err != nil {
			grpcErrCh <- err
		}
	}()

	// Start HTTP server in goroutine
	httpErrCh := make(chan error, 1)
	go func() {
		if err := httpServer.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErrCh <- err
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Info().Str("signal", sig.String()).Msg("received shutdown signal")
	case err := <-grpcErrCh:
		log.Fatal().Err(err).Msg("gRPC server error")
	case err := <-httpErrCh:
		log.Fatal().Err(err).Msg("HTTP server error")
	}

	// Graceful shutdown
	log.Info().Dur("timeout", cfg.ShutdownTimeout).Msg("initiating graceful shutdown")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()

	// Stop accepting new connections
	grpcServer.Stop()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("HTTP server shutdown error")
	}

	// Close database connection
	pool.Close()

	// Flush Sentry
	if cfg.SentryDSN != "" {
		sentry.Flush(2 * time.Second)
	}

	log.Info().Msg("shutdown complete")
}
