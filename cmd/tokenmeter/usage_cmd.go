package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	usagev1 "github.com/ai-process/light-tokenmeter/gen/usage/v1"
	"github.com/ai-process/light-tokenmeter/pkg/client"
)

// dimensions are what a summary can be grouped by, in column order.
var dimensions = []string{"project", "user", "vendor", "model", "action", "status"}

var statuses = map[string]usagev1.Status{
	"success": usagev1.Status_STATUS_SUCCESS, "error": usagev1.Status_STATUS_ERROR,
	"cancelled": usagev1.Status_STATUS_CANCELLED, "timeout": usagev1.Status_STATUS_TIMEOUT,
}

func statusName(s usagev1.Status) string {
	if s == usagev1.Status_STATUS_UNSPECIFIED {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(s.String(), "STATUS_"))
}

// query is the window and filters shared by the usage commands.
type query struct {
	from, to, last                           string
	project, user, vendor, model, action, st string
}

func (q *query) flags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&q.from, "from", "", "start, inclusive: RFC 3339 or YYYY-MM-DD (UTC)")
	f.StringVar(&q.to, "to", "", "end, exclusive: RFC 3339 or YYYY-MM-DD (default now)")
	f.StringVar(&q.last, "last", "", "window ending now, e.g. 24h, 7d, 4w (default 7d)")
	f.StringVar(&q.project, "project", "", "only this project_id")
	f.StringVar(&q.user, "user", "", "only this user_id")
	f.StringVar(&q.vendor, "vendor", "", "only this vendor")
	f.StringVar(&q.model, "model", "", "only this model")
	f.StringVar(&q.action, "action", "", "only this action_id")
	f.StringVar(&q.st, "status", "", "only success, error, cancelled or timeout")
	_ = cmd.RegisterFlagCompletionFunc("status", cobra.FixedCompletions([]string{"success", "error", "cancelled", "timeout"}, cobra.ShellCompDirectiveNoFileComp))
}

func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("time %q: want RFC 3339 (2026-10-01T00:00:00Z) or YYYY-MM-DD", s)
}

// request builds the summary request; groups are dimension names.
func (q *query) request(groups []string) (*usagev1.GetUsageSummaryRequest, error) {
	var from, to time.Time
	var err error
	if q.last != "" && q.from != "" {
		return nil, usageError{errors.New("--last and --from do not go together")}
	}
	to = time.Now().UTC()
	if q.to != "" {
		if to, err = parseTime(q.to); err != nil {
			return nil, usageError{err}
		}
	}
	if q.from != "" {
		if from, err = parseTime(q.from); err != nil {
			return nil, usageError{err}
		}
	} else {
		d, err := client.ParseDuration(firstNonEmpty(q.last, "7d"))
		if err != nil {
			return nil, usageError{err}
		}
		from = to.Add(-d)
	}
	if !from.Before(to) {
		return nil, usageError{fmt.Errorf("the window %s – %s is empty", from.Format(time.RFC3339), to.Format(time.RFC3339))}
	}
	req := &usagev1.GetUsageSummaryRequest{
		From: timestamppb.New(from), To: timestamppb.New(to),
		ProjectId: q.project, UserId: q.user, Vendor: q.vendor, Model: q.model, ActionId: q.action,
	}
	if q.st != "" {
		s, ok := statuses[q.st]
		if !ok {
			return nil, usageError{fmt.Errorf("--status %q: want success, error, cancelled or timeout", q.st)}
		}
		req.Status = s
	}
	for _, g := range groups {
		switch g {
		case "project":
			req.GroupByProjectId = true
		case "user":
			req.GroupByUserId = true
		case "vendor":
			req.GroupByVendor = true
		case "model":
			req.GroupByModel = true
		case "action":
			req.GroupByActionId = true
		case "status":
			req.GroupByStatus = true
		default:
			return nil, usageError{fmt.Errorf("group %q: want %s", g, strings.Join(dimensions, ", "))}
		}
	}
	return req, nil
}

func dimValue(r *usagev1.UsageSummaryRow, dim string) string {
	switch dim {
	case "project":
		return r.GetProjectId()
	case "user":
		return r.GetUserId()
	case "vendor":
		return r.GetVendor()
	case "model":
		return r.GetModel()
	case "action":
		return r.GetActionId()
	case "status":
		return statusName(r.GetStatus())
	}
	return ""
}

// ordered keeps the dimensions asked for in column order.
func ordered(groups []string) []string {
	var out []string
	for _, d := range dimensions {
		for _, g := range groups {
			if g == d {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// fetch runs the summary and warns when even the smallest window was capped.
func (a *app) fetch(req *usagev1.GetUsageSummaryRequest) ([]*usagev1.UsageSummaryRow, bool, error) {
	c, err := a.t.client()
	if err != nil {
		return nil, false, err
	}
	defer c.Close()
	ctx, cancel := a.ctx()
	defer cancel()
	rows, truncated, err := c.Summary(ctx, req)
	if err != nil {
		return nil, false, err
	}
	if truncated {
		fmt.Fprintf(a.stderr, "tokenmeter: warning: an hour of this query still exceeds the server's row cap; totals are incomplete, narrow the filters\n")
	}
	return rows, truncated, nil
}

func (a *app) usageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "usage", Short: "Usage reports: summary, top, by-meta, export"}
	cmd.AddCommand(a.summaryCmd(), a.topCmd(), a.byMetaCmd(), a.exportCmd())
	return cmd
}

func (a *app) summaryCmd() *cobra.Command {
	var q query
	var groups []string
	var daily bool
	var format string
	cmd := &cobra.Command{
		Use:   "summary [--last 7d | --from T --to T] [filters] [--group-by DIM,...] [--daily]",
		Short: "Requests, tokens and cost over a window, by the dimensions you group by",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			req, err := q.request(groups)
			if err != nil {
				return err
			}
			rows, truncated, err := a.fetch(req)
			if err != nil {
				return err
			}
			rows = client.Merge(rows, daily)
			if daily {
				sort.SliceStable(rows, func(i, j int) bool { return rows[i].GetBucket().GetSeconds() < rows[j].GetBucket().GetSeconds() })
			} else {
				sort.SliceStable(rows, func(i, j int) bool { return rows[i].GetCostNanousd() > rows[j].GetCostNanousd() })
			}
			if p.json() {
				return p.data(map[string]any{"from": req.GetFrom(), "to": req.GetTo(), "rows": messages(rows), "truncated": truncated})
			}
			dims := ordered(groups)
			var header []string
			if daily {
				header = append(header, "DAY")
			}
			for _, d := range dims {
				header = append(header, strings.ToUpper(d))
			}
			header = append(header, "REQUESTS", "TOKENS IN", "TOKENS OUT", "CACHE READ", "COST USD", "USD PER REQUEST", "AVG MS")
			var out [][]string
			total := &usagev1.UsageSummaryRow{}
			for _, r := range rows {
				var line []string
				if daily {
					line = append(line, r.GetBucket().AsTime().UTC().Format(time.DateOnly))
				}
				for _, d := range dims {
					line = append(line, dimValue(r, d))
				}
				out = append(out, append(line, a.figures(p, r)...))
				total = client.Merge([]*usagev1.UsageSummaryRow{total, stripped(r)}, false)[0]
			}
			if p.format == "table" && len(rows) > 1 {
				line := make([]string, len(header)-7)
				if len(line) > 0 {
					line[0] = "TOTAL"
				}
				out = append(out, append(line, a.figures(p, total)...))
			}
			return p.rows(header, out)
		},
	}
	q.flags(cmd)
	cmd.Flags().StringSliceVar(&groups, "group-by", nil, "project, user, vendor, model, action, status")
	cmd.Flags().BoolVar(&daily, "daily", false, "one row per UTC day")
	_ = cmd.RegisterFlagCompletionFunc("group-by", cobra.FixedCompletions(dimensions, cobra.ShellCompDirectiveNoFileComp))
	outputFlag(cmd, &format)
	return cmd
}

// stripped is a row's figures without its dimensions, for totals.
func stripped(r *usagev1.UsageSummaryRow) *usagev1.UsageSummaryRow {
	return &usagev1.UsageSummaryRow{TokensInput: r.GetTokensInput(), TokensOutput: r.GetTokensOutput(), TokensTotal: r.GetTokensTotal(),
		RequestCount: r.GetRequestCount(), SumDurationMs: r.GetSumDurationMs(), CostNanousd: r.GetCostNanousd(),
		CacheReadTokens: r.GetCacheReadTokens(), CacheWriteTokens: r.GetCacheWriteTokens()}
}

func (a *app) figures(p *printer, r *usagev1.UsageSummaryRow) []string {
	perReq, avg := "", ""
	if n := r.GetRequestCount(); n > 0 {
		perReq = p.usd(r.GetCostNanousd() / n)
		avg = strconv.FormatInt(r.GetSumDurationMs()/n, 10)
	}
	return []string{strconv.FormatInt(r.GetRequestCount(), 10), strconv.FormatInt(r.GetTokensInput(), 10),
		strconv.FormatInt(r.GetTokensOutput(), 10), strconv.FormatInt(r.GetCacheReadTokens(), 10),
		p.usd(r.GetCostNanousd()), perReq, avg}
}

func (a *app) topCmd() *cobra.Command {
	var q query
	var by, dim, format string
	var limit int
	cmd := &cobra.Command{
		Use:   "top [--by cost|tokens|requests] [--dimension DIM] [--limit N]",
		Short: "Who or what used the most over a window",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			measure := map[string]func(*usagev1.UsageSummaryRow) int64{
				"cost":     (*usagev1.UsageSummaryRow).GetCostNanousd,
				"tokens":   (*usagev1.UsageSummaryRow).GetTokensTotal,
				"requests": (*usagev1.UsageSummaryRow).GetRequestCount,
			}[by]
			if measure == nil {
				return usageError{fmt.Errorf("--by %q: want cost, tokens or requests", by)}
			}
			req, err := q.request([]string{dim})
			if err != nil {
				return err
			}
			rows, truncated, err := a.fetch(req)
			if err != nil {
				return err
			}
			rows = client.Merge(rows, false)
			var sum int64
			for _, r := range rows {
				sum += measure(r)
			}
			sort.SliceStable(rows, func(i, j int) bool { return measure(rows[i]) > measure(rows[j]) })
			if limit > 0 && len(rows) > limit {
				rows = rows[:limit]
			}
			if p.json() {
				return p.data(map[string]any{"from": req.GetFrom(), "to": req.GetTo(), "by": by, "dimension": dim, "rows": messages(rows), "truncated": truncated})
			}
			var out [][]string
			for i, r := range rows {
				share := ""
				if sum > 0 {
					share = fmt.Sprintf("%.1f%%", 100*float64(measure(r))/float64(sum))
				}
				out = append(out, []string{strconv.Itoa(i + 1), dimValue(r, dim), strconv.FormatInt(r.GetRequestCount(), 10),
					strconv.FormatInt(r.GetTokensTotal(), 10), p.usd(r.GetCostNanousd()), share})
			}
			return p.rows([]string{"RANK", strings.ToUpper(dim), "REQUESTS", "TOKENS", "COST USD", "SHARE OF " + strings.ToUpper(by)}, out)
		},
	}
	q.flags(cmd)
	cmd.Flags().StringVar(&by, "by", "cost", "cost, tokens or requests")
	cmd.Flags().StringVar(&dim, "dimension", "project", strings.Join(dimensions, ", "))
	cmd.Flags().IntVar(&limit, "limit", 10, "rows to show, 0 for all")
	_ = cmd.RegisterFlagCompletionFunc("by", cobra.FixedCompletions([]string{"cost", "tokens", "requests"}, cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("dimension", cobra.FixedCompletions(dimensions, cobra.ShellCompDirectiveNoFileComp))
	outputFlag(cmd, &format)
	return cmd
}

func (a *app) byMetaCmd() *cobra.Command {
	req := &usagev1.GetUsageByMetaRequest{}
	var format string
	cmd := &cobra.Command{
		Use:   "by-meta --key KEY --value VALUE [--group-by KEY] [--project ID]",
		Short: "What the events tagged meta KEY=VALUE used, by vendor and model",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			c, err := a.t.client()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := a.ctx()
			defer cancel()
			res, err := c.ByMeta(ctx, req)
			if err != nil {
				return err
			}
			if res.GetTruncated() {
				fmt.Fprintln(a.stderr, "tokenmeter: warning: the result exceeds the server's row cap and is incomplete; filter by --project")
			}
			if p.json() {
				return p.data(res)
			}
			var header []string
			if req.GroupByMetaKey != "" {
				header = append(header, strings.ToUpper(req.GroupByMetaKey))
			}
			header = append(header, "VENDOR", "MODEL", "REQUESTS", "TOKENS IN", "TOKENS OUT", "COST USD")
			var out [][]string
			for _, r := range res.GetRows() {
				var line []string
				if req.GroupByMetaKey != "" {
					line = append(line, r.GetGroupValue())
				}
				out = append(out, append(line, r.GetVendor(), r.GetModel(), strconv.FormatInt(r.GetRequestCount(), 10),
					strconv.FormatInt(r.GetTokensInput(), 10), strconv.FormatInt(r.GetTokensOutput(), 10), p.usd(r.GetCostNanousd())))
			}
			return p.rows(header, out)
		},
	}
	cmd.Flags().StringVar(&req.MetaKey, "key", "", "meta key, e.g. job_id")
	cmd.Flags().StringVar(&req.MetaValue, "value", "", "its value")
	cmd.Flags().StringVar(&req.GroupByMetaKey, "group-by", "", "a second meta key to split by, e.g. task_id")
	cmd.Flags().StringVar(&req.ProjectId, "project", "", "only this project_id")
	_ = cmd.MarkFlagRequired("key")
	_ = cmd.MarkFlagRequired("value")
	outputFlag(cmd, &format)
	return cmd
}

func (a *app) exportCmd() *cobra.Command {
	var q query
	var groups []string
	var format, out string
	cmd := &cobra.Command{
		Use:   "export [--format csv|json] [--out FILE] [window and filters] [--group-by DIM,...]",
		Short: "Daily usage rows for a spreadsheet or a script; grouped by every dimension by default",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if format != "csv" && format != "json" {
				return usageError{fmt.Errorf("--format %q: want csv or json", format)}
			}
			if len(groups) == 0 {
				groups = dimensions
			}
			req, err := q.request(groups)
			if err != nil {
				return err
			}
			rows, truncated, err := a.fetch(req)
			if err != nil {
				return err
			}
			rows = client.Merge(rows, true)
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].GetBucket().GetSeconds() < rows[j].GetBucket().GetSeconds() })
			if out == "" {
				return writeExport(&printer{w: a.stdout, format: format}, req, groups, rows, truncated)
			}
			// Into a temporary file renamed at the end, so a failed write leaves no partial export.
			f, err := os.CreateTemp(filepath.Dir(out), "."+filepath.Base(out)+".*")
			if err != nil {
				return err
			}
			defer os.Remove(f.Name())
			if err := writeExport(&printer{w: f, format: format}, req, groups, rows, truncated); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			return os.Rename(f.Name(), out)
		},
	}
	q.flags(cmd)
	cmd.Flags().StringSliceVar(&groups, "group-by", nil, "dimensions to keep (default all)")
	cmd.Flags().StringVar(&format, "format", "csv", "csv or json")
	cmd.Flags().StringVar(&out, "out", "", "write to this file instead of stdout")
	_ = cmd.RegisterFlagCompletionFunc("format", cobra.FixedCompletions([]string{"csv", "json"}, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

func writeExport(p *printer, req *usagev1.GetUsageSummaryRequest, groups []string, rows []*usagev1.UsageSummaryRow, truncated bool) error {
	if p.format == "json" {
		return p.data(map[string]any{"from": req.GetFrom(), "to": req.GetTo(), "rows": messages(rows), "truncated": truncated})
	}
	dims := ordered(groups)
	header := []string{"day"}
	for _, d := range dims {
		header = append(header, d)
	}
	header = append(header, "requests", "tokens_input", "tokens_output", "tokens_total", "cache_read_tokens",
		"cache_write_tokens", "sum_duration_ms", "cost_usd", "cost_nanousd")
	var lines [][]string
	for _, r := range rows {
		line := []string{r.GetBucket().AsTime().UTC().Format(time.DateOnly)}
		for _, d := range dims {
			line = append(line, dimValue(r, d))
		}
		lines = append(lines, append(line, strconv.FormatInt(r.GetRequestCount(), 10), strconv.FormatInt(r.GetTokensInput(), 10),
			strconv.FormatInt(r.GetTokensOutput(), 10), strconv.FormatInt(r.GetTokensTotal(), 10),
			strconv.FormatInt(r.GetCacheReadTokens(), 10), strconv.FormatInt(r.GetCacheWriteTokens(), 10),
			strconv.FormatInt(r.GetSumDurationMs(), 10), exactUSD(r.GetCostNanousd()), strconv.FormatInt(r.GetCostNanousd(), 10)))
	}
	return p.rows(header, lines)
}
