package metrics

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestServeExposesMetrics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	BuildInfo.WithLabelValues("test").Set(1)
	if err := Serve(ctx, addr); err != nil {
		t.Fatal(err)
	}
	var body string
	for i := 0; i < 50; i++ {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{`tokenmeter_build_info{version="test"} 1`, "go_goroutines", "tokenmeter_clickhouse_serving"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	if err := Serve(ctx, ""); err != nil {
		t.Errorf("an empty address must disable the server: %v", err)
	}
}

func TestWatchOutboxSetsGauges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	backlog := func(context.Context) (int64, time.Duration, error) {
		calls++
		if calls == 2 {
			cancel()
			return 0, 0, errors.New("closing")
		}
		return 42, 90 * time.Second, nil
	}
	WatchOutbox(ctx, backlog, time.Millisecond)
	if got := testutil.ToFloat64(OutboxEvents); got != 42 {
		t.Errorf("outbox events %v, want 42", got)
	}
	if got := testutil.ToFloat64(OutboxOldest); got != 90 {
		t.Errorf("outbox oldest %v, want 90", got)
	}
}

// The registry must stay lint-clean: names and help are part of the 1.x API.
func TestMetricsLint(t *testing.T) {
	problems, err := testutil.GatherAndLint(Registry)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		if strings.HasPrefix(p.Metric, "tokenmeter_") {
			t.Errorf("%s: %s", p.Metric, p.Text)
		}
	}
}
