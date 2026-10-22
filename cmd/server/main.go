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

	"github.com/ai-process/light-tokenmeter/internal/auth"
	"github.com/ai-process/light-tokenmeter/internal/config"
	"github.com/ai-process/light-tokenmeter/internal/database"
	"github.com/ai-process/light-tokenmeter/internal/logger"
	"github.com/ai-process/light-tokenmeter/internal/repository"
	"github.com/ai-process/light-tokenmeter/internal/server"
	"github.com/ai-process/light-tokenmeter/internal/service"
	"github.com/ai-process/light-tokenmeter/internal/storage/clickhouse"
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

	verifier, err := auth.Load(cfg.Tokens, cfg.TokensFile)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid token configuration")
	}
	if !verifier.Enabled() {
		log.Warn().Msg("TOKENMETER_TOKENS is empty: authentication is off and anyone who reaches the ports can write and read usage")
	}

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

	// Create usage service
	usageSvc := service.NewUsageService(repo)

	var chClient *clickhouse.Client
	var chWriter *clickhouse.BatchWriter

	if cfg.ClickHouseEnabled {
		log.Info().
			Str("addr", cfg.ClickHouseAddr).
			Str("db", cfg.ClickHouseDatabase).
			Int("batch_size", cfg.ClickHouseBatchSize).
			Dur("flush_interval", cfg.ClickHouseFlushInterval).
			Msg("initializing ClickHouse storage")

		var err error
		chClient, err = clickhouse.Connect(ctx, clickhouse.Config{
			Addr:          cfg.ClickHouseAddr,
			Database:      cfg.ClickHouseDatabase,
			User:          cfg.ClickHouseUser,
			Password:      cfg.ClickHousePassword,
			BatchSize:     cfg.ClickHouseBatchSize,
			FlushInterval: cfg.ClickHouseFlushInterval,
		})
		if err != nil {
			log.Fatal().Err(err).Msg("failed to connect to ClickHouse")
		}

		if err := chClient.EnsureSchema(ctx); err != nil {
			log.Fatal().Err(err).Msg("failed to run ClickHouse migrations")
		}

		chWriter = clickhouse.NewBatchWriter(chClient.Conn(), cfg.ClickHouseBatchSize, cfg.ClickHouseFlushInterval)
		chWriter.Start()

		usageSvc = usageSvc.WithAnalytics(chClient).WithEventSink(chWriter)
	}

	// Create servers
	grpcServer := server.NewGRPCServerWithService(usageSvc, cfg.GRPCAddr, verifier)
	httpServer := server.NewHTTPServerWithService(usageSvc, repo, cfg.HTTPAddr, verifier)

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

	// Flush and close ClickHouse batch writer and client
	if chWriter != nil {
		if err := chWriter.Close(); err != nil {
			log.Error().Err(err).Msg("ClickHouse batch writer close error")
		}
	}
	if chClient != nil {
		if err := chClient.Close(); err != nil {
			log.Error().Err(err).Msg("ClickHouse client close error")
		}
	}

	// Close database connection
	pool.Close()

	// Flush Sentry
	if cfg.SentryDSN != "" {
		sentry.Flush(2 * time.Second)
	}

	log.Info().Msg("shutdown complete")
}
