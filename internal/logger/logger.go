package logger

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	sentryzerolog "github.com/getsentry/sentry-go/zerolog"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/light-tokenmeter/internal/config"
)

// Init initializes the global logger with proper configuration
func Init(cfg *config.Config) {
	zerolog.TimeFieldFormat = time.RFC3339

	logLevel := "info"
	if cfg != nil && cfg.LogLevel != "" {
		logLevel = cfg.LogLevel
	}

	var level zerolog.Level
	switch strings.ToLower(logLevel) {
	case "debug":
		level = zerolog.DebugLevel
	case "info":
		level = zerolog.InfoLevel
	case "warn":
		level = zerolog.WarnLevel
	case "error":
		level = zerolog.ErrorLevel
	default:
		level = zerolog.InfoLevel
	}

	zerolog.SetGlobalLevel(level)

	var writers []io.Writer

	if cfg != nil && cfg.LogFormat == "console" {
		writers = append(writers, zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: time.RFC3339,
		})
	} else {
		writers = append(writers, os.Stdout)
	}

	// Initialize Sentry if DSN is provided
	var sentryWriter *sentryzerolog.Writer
	if cfg != nil && cfg.SentryDSN != "" {
		err := sentry.Init(sentry.ClientOptions{
			Dsn:              cfg.SentryDSN,
			Debug:            cfg.SentryDebug,
			EnableTracing:    cfg.SentryTracing,
			TracesSampleRate: 1.0,
			Environment:      cfg.SentryEnvironment,
			Release:          cfg.SentryRelease,
		})
		if err != nil {
			log.Fatal().Err(err).Msg("sentry.Init failed")
		}

		sentryWriter, err = sentryzerolog.New(sentryzerolog.Config{
			ClientOptions: sentry.ClientOptions{
				Dsn: cfg.SentryDSN,
			},
			Options: sentryzerolog.Options{
				Levels:          []zerolog.Level{zerolog.ErrorLevel, zerolog.FatalLevel, zerolog.PanicLevel},
				WithBreadcrumbs: cfg.SentryWithBreadcrumbs,
				FlushTimeout:    3 * time.Second,
			},
		})
		if err != nil {
			log.Fatal().Err(err).Msg("failed to create sentry writer")
		}

		log.Info().Msg("Sentry integration enabled")
	}

	if sentryWriter != nil {
		writers = append(writers, sentryWriter)
	}

	log.Logger = log.Output(zerolog.MultiLevelWriter(writers...))

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	log.Logger = log.Logger.With().Str("host", hostname).Str("service", "light-tokenmeter").Logger()

	zerolog.DefaultContextLogger = &log.Logger
}
