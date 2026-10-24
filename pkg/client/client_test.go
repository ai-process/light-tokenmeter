package client

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	usagev1 "github.com/ai-process/light-tokenmeter/gen/usage/v1"
)

// capped answers windows up to two hours with one request per hour, and caps
// anything longer, as the server does past its row limit.
type capped struct {
	usagev1.UnimplementedUsageServiceServer
	calls atomic.Int32
}

func (c *capped) GetUsageSummary(_ context.Context, req *usagev1.GetUsageSummaryRequest) (*usagev1.GetUsageSummaryResponse, error) {
	c.calls.Add(1)
	from, to := req.GetFrom().AsTime(), req.GetTo().AsTime()
	day := timestamppb.New(from.Truncate(24 * time.Hour))
	if to.Sub(from) > 2*time.Hour {
		return &usagev1.GetUsageSummaryResponse{Truncated: true, Rows: []*usagev1.UsageSummaryRow{{Bucket: day, ProjectId: "p", RequestCount: 1}}}, nil
	}
	h := int64(to.Sub(from) / time.Hour)
	return &usagev1.GetUsageSummaryResponse{Rows: []*usagev1.UsageSummaryRow{{Bucket: day, ProjectId: "p", RequestCount: h, CostNanousd: h * 10}}}, nil
}

func serve(t *testing.T, srv usagev1.UsageServiceServer) *Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	usagev1.RegisterUsageServiceServer(g, srv)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	c, err := New(Options{Addr: "passthrough:///tm", DialOptions: []grpc.DialOption{
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSummarySplitsCappedWindows(t *testing.T) {
	fake := &capped{}
	c := serve(t, fake)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	req := &usagev1.GetUsageSummaryRequest{From: timestamppb.New(from), To: timestamppb.New(from.Add(10 * time.Hour))}
	rows, truncated, err := c.Summary(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(rows) != 1 || rows[0].GetRequestCount() != 10 || rows[0].GetCostNanousd() != 100 {
		t.Fatalf("rows = %v, truncated = %v", rows, truncated)
	}
	if n := fake.calls.Load(); n < 3 {
		t.Fatalf("calls = %d: the window was not split", n)
	}

	// A window that stays capped at MinWindow is reported, not looped on.
	always := &alwaysCapped{}
	rows, truncated, err = serve(t, always).Summary(context.Background(), req)
	if err != nil || !truncated || len(rows) == 0 {
		t.Fatalf("rows = %v, truncated = %v, err = %v", rows, truncated, err)
	}
}

type alwaysCapped struct {
	usagev1.UnimplementedUsageServiceServer
}

func (alwaysCapped) GetUsageSummary(_ context.Context, req *usagev1.GetUsageSummaryRequest) (*usagev1.GetUsageSummaryResponse, error) {
	return &usagev1.GetUsageSummaryResponse{Truncated: true, Rows: []*usagev1.UsageSummaryRow{{Bucket: req.GetFrom(), RequestCount: 1}}}, nil
}

func TestMerge(t *testing.T) {
	d1 := timestamppb.New(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	d2 := timestamppb.New(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	rows := []*usagev1.UsageSummaryRow{
		{Bucket: d1, ProjectId: "a", RequestCount: 1, CostNanousd: 5},
		{Bucket: d2, ProjectId: "a", RequestCount: 2, CostNanousd: 7},
		{Bucket: d1, ProjectId: "b", RequestCount: 4},
		{Bucket: d1, ProjectId: "a", RequestCount: 8, TokensTotal: 3},
	}
	daily := Merge(rows, true)
	if len(daily) != 3 || daily[0].GetRequestCount() != 9 || daily[0].GetTokensTotal() != 3 {
		t.Fatalf("daily = %v", daily)
	}
	total := Merge(rows, false)
	if len(total) != 2 || total[0].GetRequestCount() != 11 || total[0].GetCostNanousd() != 12 || total[0].GetBucket() != nil {
		t.Fatalf("total = %v", total)
	}
	if rows[0].GetRequestCount() != 1 {
		t.Fatal("Merge changed its input")
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "2w": 14 * 24 * time.Hour, "36h": 36 * time.Hour, "90m": 90 * time.Minute} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "d", "0d", "-1d", "1.5d", "7x", "+3d", "-5h", "200000d", "99999999999w"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) accepted", in)
		}
	}
}
