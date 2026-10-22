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
	"github.com/ai-process/light-tokenmeter/internal/metrics"
	"github.com/ai-process/light-tokenmeter/internal/repository"
	"github.com/ai-process/light-tokenmeter/internal/server"
	"github.com/ai-process/light-tokenmeter/internal/service"
	"github.com/ai-process/light-tokenmeter/internal/storage/clickhouse"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

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
		Str("version", version).
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

	var ch *clickhouseStack
	if cfg.ClickHouseEnabled {
		log.Info().
			Str("addr", cfg.ClickHouseAddr).
			Str("db", cfg.ClickHouseDatabase).
			Int("batch_size", cfg.ClickHouseBatchSize).
			Dur("flush_interval", cfg.ClickHouseFlushInterval).
			Msg("initializing ClickHouse storage")
		// Events reach ClickHouse through the Postgres outbox, so none is lost
		// to a full buffer, a ClickHouse outage or a restart.
		repo.WithOutbox()
		usageSvc.ExpectAnalytics()
		ch = startClickHouse(ctx, cfg, repo, usageSvc)
	}

	metrics.BuildInfo.WithLabelValues(version).Set(1)
	if err := metrics.Serve(ctx, cfg.MetricsAddr); err != nil {
		log.Fatal().Err(err).Str("addr", cfg.MetricsAddr).Msg("failed to start the metrics server")
	}
	if cfg.ClickHouseEnabled {
		metrics.ClickHouseEnabled.Set(1)
		go metrics.WatchOutbox(ctx, repo.OutboxBacklog, 15*time.Second)
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

	// Events not yet copied stay in the outbox for the next start.
	if ch != nil {
		ch.close()
	}

	// Close database connection
	pool.Close()

	// Flush Sentry
	if cfg.SentryDSN != "" {
		sentry.Flush(2 * time.Second)
	}

	log.Info().Msg("shutdown complete")
}

// clickhouseStack connects in the background: a ClickHouse that is down at
// startup must not stop the service, which records to Postgres meanwhile.
type clickhouseStack struct {
	cancel context.CancelFunc
	done   chan struct{}
	client *clickhouse.Client
	repl   *clickhouse.Replicator
}

func startClickHouse(parent context.Context, cfg *config.Config, repo *repository.Repository, svc *service.UsageService) *clickhouseStack {
	ctx, cancel := context.WithCancel(parent)
	st := &clickhouseStack{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(st.done)
		for backoff := time.Second; ; backoff = min(2*backoff, 30*time.Second) {
			client, err := clickhouse.Connect(ctx, clickhouse.Config{
				Addr:     cfg.ClickHouseAddr,
				Database: cfg.ClickHouseDatabase,
				User:     cfg.ClickHouseUser,
				Password: cfg.ClickHousePassword,
			})
			if err == nil {
				err = client.EnsureSchema(ctx)
				var queued int64
				if err == nil {
					queued, err = repo.BackfillOutbox(ctx, client.Empty)
				}
				if queued > 0 {
					log.Info().Int64("events", queued).Msg("ClickHouse is empty: queued the Postgres history for it")
				}
				if err != nil {
					_ = client.Close()
				}
			}
			if err == nil {
				st.client = client
				st.repl = clickhouse.NewReplicator(repo, client.Conn(), cfg.ClickHouseBatchSize, cfg.ClickHouseFlushInterval)
				st.repl.Start()
				// Reports switch to ClickHouse only once it holds the backfilled history.
				for {
					pending, err := repo.BackfillPending(ctx)
					if err == nil && !pending {
						break
					}
					if err != nil {
						log.Warn().Err(err).Msg("cannot check the ClickHouse backfill")
					}
					select {
					case <-time.After(5 * time.Second):
					case <-ctx.Done():
						return
					}
				}
				svc.WithAnalytics(client)
				log.Info().Msg("ClickHouse ready: copying events and serving reports from it")
				return
			}
			if ctx.Err() != nil {
				return
			}
			log.Error().Err(err).Dur("retry_in", backoff).Msg("ClickHouse unavailable; recording to Postgres, reports from Postgres")
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
		}
	}()
	return st
}

func (st *clickhouseStack) close() {
	st.cancel()
	<-st.done
	if st.repl != nil {
		st.repl.Close()
	}
	if st.client != nil {
		if err := st.client.Close(); err != nil {
			log.Error().Err(err).Msg("ClickHouse client close error")
		}
	}
}
