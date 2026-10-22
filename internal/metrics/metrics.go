// Package metrics holds the Prometheus metrics, served on METRICS_ADDR. The
// names and labels are part of the 1.x API (README "Metrics").
package metrics

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
)

// Registry holds only tokenmeter's metrics plus the Go and process ones.
var Registry = prometheus.NewRegistry()

var (
	Events = promauto(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tokenmeter_events_total",
		Help: "Usage events received, by result: created, duplicate or invalid.",
	}, []string{"result"}))

	Requests = promauto(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tokenmeter_requests_total",
		Help: "API requests by transport (http, grpc), route or RPC, and status code.",
	}, []string{"transport", "route", "code"}))

	RequestDuration = promauto(prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tokenmeter_request_duration_seconds",
		Help:    "API request duration by transport and route or RPC.",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"transport", "route"}))

	StorageErrors = promauto(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tokenmeter_storage_errors_total",
		Help: "Failed storage operations by store (postgres, clickhouse) and operation.",
	}, []string{"store", "op"}))

	ReadFallbacks = promauto(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tokenmeter_read_fallbacks_total",
		Help: "Reports answered from Postgres because a ClickHouse query failed.",
	}))

	ClickHouseEnabled = promauto(prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tokenmeter_clickhouse_enabled",
		Help: "1 when CLICKHOUSE_ENABLED is set.",
	}))

	ClickHouseUp = promauto(prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tokenmeter_clickhouse_serving",
		Help: "1 while reports are served from ClickHouse, 0 while from Postgres.",
	}))

	Copied = promauto(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tokenmeter_clickhouse_copied_events_total",
		Help: "Events copied from the outbox to ClickHouse.",
	}))

	InsertFailures = promauto(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tokenmeter_clickhouse_insert_failures_total",
		Help: "Failed ClickHouse inserts, by reason: unreachable (retried) or refused (postponed).",
	}, []string{"reason"}))

	OutboxEvents = promauto(prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tokenmeter_clickhouse_outbox_events",
		Help: "Events waiting in the outbox for ClickHouse.",
	}))

	OutboxOldest = promauto(prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tokenmeter_clickhouse_outbox_oldest_seconds",
		Help: "Age of the oldest event waiting for ClickHouse; 0 when none waits.",
	}))

	BuildInfo = promauto(prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tokenmeter_build_info",
		Help: "Always 1; the version label names the running release.",
	}, []string{"version"}))
)

func init() {
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

func promauto[C prometheus.Collector](c C) C {
	Registry.MustRegister(c)
	return c
}

// Serve exposes /metrics and /healthz on addr until ctx ends; "" disables it.
func Serve(ctx context.Context, addr string) error {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("metrics server stopped")
		}
	}()
	log.Info().Str("addr", addr).Msg("serving metrics")
	return nil
}

// Backlog reports the outbox size and the age of its oldest event.
type Backlog func(ctx context.Context) (int64, time.Duration, error)

// WatchOutbox samples the outbox every interval: a query per scrape could
// pile up on a large outbox.
func WatchOutbox(ctx context.Context, backlog Backlog, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		q, cancel := context.WithTimeout(ctx, interval)
		n, oldest, err := backlog(q)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			StorageErrors.WithLabelValues("postgres", "outbox_backlog").Inc()
		} else {
			OutboxEvents.Set(float64(n))
			OutboxOldest.Set(oldest.Seconds())
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}
