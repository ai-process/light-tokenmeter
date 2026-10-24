// Package client is a Go client for light-tokenmeter's gRPC API: recording
// usage events and reading summaries, with large windows fetched in parts so
// a summary is not cut at the server's row cap.
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	usagev1 "github.com/ai-process/light-tokenmeter/gen/usage/v1"
)

// MaxBatch is the most events one RecordUsageEvents call takes.
const MaxBatch = 500

// MinWindow is the shortest window Summary splits a truncated one into.
const MinWindow = time.Hour

// Options say where light-tokenmeter is and how to reach it.
type Options struct {
	Addr      string      // gRPC host:port
	Token     string      // bearer token; empty when the service runs without auth
	TLS       bool        // use TLS
	TLSConfig *tls.Config // optional TLS settings; nil means system roots, TLS 1.2+
	// DialOptions are added after the client's own, for tests and custom transports.
	DialOptions []grpc.DialOption
}

// Client talks to one light-tokenmeter. It is safe for concurrent use.
type Client struct {
	conn *grpc.ClientConn
	svc  usagev1.UsageServiceClient
}

// New connects lazily: the first call dials.
func New(o Options) (*Client, error) {
	if o.Addr == "" {
		return nil, errors.New("client: Addr is required")
	}
	creds := insecure.NewCredentials()
	if o.TLS {
		cfg := o.TLSConfig
		if cfg == nil {
			cfg = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		creds = credentials.NewTLS(cfg)
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if o.Token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearer{token: o.Token, secure: o.TLS}))
	}
	conn, err := grpc.NewClient(o.Addr, append(opts, o.DialOptions...)...)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, svc: usagev1.NewUsageServiceClient(conn)}, nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Service is the generated client, for calls this package does not wrap.
func (c *Client) Service() usagev1.UsageServiceClient { return c.svc }

type bearer struct {
	token  string
	secure bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}
func (b bearer) RequireTransportSecurity() bool { return b.secure }

// Record sends events in batches of MaxBatch and counts what was new. A
// refused batch stops it; the batches before it stay recorded.
func (c *Client) Record(ctx context.Context, events ...*usagev1.UsageEvent) (created, duplicates int, err error) {
	for len(events) > 0 {
		n := min(len(events), MaxBatch)
		res, err := c.svc.RecordUsageEvents(ctx, &usagev1.RecordUsageEventsRequest{Events: events[:n]})
		if err != nil {
			return created, duplicates, err
		}
		created += int(res.GetCreatedCount())
		duplicates += int(res.GetDuplicateCount())
		events = events[n:]
	}
	return created, duplicates, nil
}

// Summary returns the rows of req's window. When the server caps a window
// (truncated), Summary fetches its halves and merges them, down to MinWindow;
// truncated is true only if a window that short is still capped.
func (c *Client) Summary(ctx context.Context, req *usagev1.GetUsageSummaryRequest) (rows []*usagev1.UsageSummaryRow, truncated bool, err error) {
	res, err := c.svc.GetUsageSummary(ctx, req)
	if err != nil {
		return nil, false, err
	}
	if !res.GetTruncated() {
		return res.GetRows(), false, nil
	}
	from, to := req.GetFrom().AsTime(), req.GetTo().AsTime()
	mid := from.Add(to.Sub(from) / 2).Truncate(time.Hour)
	if to.Sub(from) <= MinWindow || !mid.After(from) || !mid.Before(to) {
		return res.GetRows(), true, nil
	}
	left := proto.Clone(req).(*usagev1.GetUsageSummaryRequest)
	left.To = timestamppb.New(mid)
	right := proto.Clone(req).(*usagev1.GetUsageSummaryRequest)
	right.From = timestamppb.New(mid)
	a, ta, err := c.Summary(ctx, left)
	if err != nil {
		return nil, false, err
	}
	b, tb, err := c.Summary(ctx, right)
	if err != nil {
		return nil, false, err
	}
	// A day split at mid shows up in both halves; its parts add up.
	return Merge(append(a, b...), true), ta || tb, nil
}

// ByMeta returns what the events tagged meta_key=meta_value used.
func (c *Client) ByMeta(ctx context.Context, req *usagev1.GetUsageByMetaRequest) (*usagev1.GetUsageByMetaResponse, error) {
	return c.svc.GetUsageByMeta(ctx, req)
}

// Merge sums rows that have the same dimensions, and the same day when
// byDay is set; without it every row covers the whole window. The order is
// that of each key's first row.
func Merge(rows []*usagev1.UsageSummaryRow, byDay bool) []*usagev1.UsageSummaryRow {
	type key struct {
		day                                  int64
		project, user, vendor, model, action string
		status                               usagev1.Status
	}
	index := map[key]*usagev1.UsageSummaryRow{}
	var out []*usagev1.UsageSummaryRow
	for _, r := range rows {
		k := key{project: r.GetProjectId(), user: r.GetUserId(), vendor: r.GetVendor(), model: r.GetModel(),
			action: r.GetActionId(), status: r.GetStatus()}
		if byDay {
			k.day = r.GetBucket().GetSeconds()
		}
		m, ok := index[k]
		if !ok {
			m = proto.Clone(r).(*usagev1.UsageSummaryRow)
			if !byDay {
				m.Bucket = nil
			}
			index[k] = m
			out = append(out, m)
			continue
		}
		m.TokensInput += r.GetTokensInput()
		m.TokensOutput += r.GetTokensOutput()
		m.TokensTotal += r.GetTokensTotal()
		m.RequestCount += r.GetRequestCount()
		m.SumDurationMs += r.GetSumDurationMs()
		m.CostNanousd += r.GetCostNanousd()
		m.CacheReadTokens += r.GetCacheReadTokens()
		m.CacheWriteTokens += r.GetCacheWriteTokens()
	}
	return out
}

// USD converts nano-USD, the API's unit of cost, to dollars.
func USD(nanousd int64) float64 { return float64(nanousd) / 1e9 }

// Window is [from, to) for a duration ending now, in UTC, e.g. Last(7*24*time.Hour).
func Last(d time.Duration) (from, to time.Time) {
	to = time.Now().UTC()
	return to.Add(-d), to
}

// ParseDuration reads Go durations plus d (days) and w (weeks): 7d, 2w, 36h.
func ParseDuration(s string) (time.Duration, error) {
	if n := len(s); n > 1 && (s[n-1] == 'd' || s[n-1] == 'w') {
		var v int
		if _, err := fmt.Sscanf(s[:n-1], "%d", &v); err != nil || v <= 0 || fmt.Sprint(v) != s[:n-1] {
			return 0, fmt.Errorf("duration %q: want e.g. 7d, 2w or 36h", s)
		}
		unit := 24 * time.Hour
		if s[n-1] == 'w' {
			unit *= 7
		}
		return time.Duration(v) * unit, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("duration %q: want e.g. 7d, 2w or 36h", s)
	}
	return d, nil
}
