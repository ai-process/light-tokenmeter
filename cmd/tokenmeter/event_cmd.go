package main

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	usagev1 "github.com/ai-process/light-tokenmeter/gen/usage/v1"
)

func (a *app) eventCmd() *cobra.Command {
	ev := &usagev1.UsageEvent{}
	var st, cost, at string
	var meta map[string]string
	send := &cobra.Command{
		Use:   "send --project P --user U --action A --vendor V --model M [--status S] [...]",
		Short: "Record one usage event, to check that writing works",
		Long:  "Record one usage event with a new event_id (or --id). It needs a token with the write scope when auth is on.",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			s, ok := statuses[st]
			if !ok {
				return usageError{fmt.Errorf("--status %q: want success, error, cancelled or timeout", st)}
			}
			ev.Status = s
			if ev.EventId == "" {
				ev.EventId = uuid.NewString()
			}
			if cost != "" {
				usd, err := strconv.ParseFloat(cost, 64)
				if err != nil || !(usd >= 0 && usd < math.MaxInt64/1e9) {
					return usageError{fmt.Errorf("--cost-usd %q: want an amount of 0 or more", cost)}
				}
				ev.CostNanousd = int64(math.Round(usd * 1e9))
			}
			if at != "" {
				t, err := time.Parse(time.RFC3339, at)
				if err != nil {
					return usageError{fmt.Errorf("--at %q: want RFC 3339", at)}
				}
				ev.OccurredAt = timestamppb.New(t)
			}
			ev.Meta = meta
			c, err := a.t.client()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := a.ctx()
			defer cancel()
			res, err := c.Service().RecordUsageEvent(ctx, &usagev1.RecordUsageEventRequest{Event: ev})
			if err != nil {
				return err
			}
			if res.GetCreated() {
				fmt.Fprintf(a.stdout, "Recorded event %s.\n", ev.EventId)
			} else {
				fmt.Fprintf(a.stdout, "Event %s was already recorded; nothing changed.\n", ev.EventId)
			}
			return nil
		},
	}
	f := send.Flags()
	f.StringVar(&ev.EventId, "id", "", "event_id (UUID; default a new one)")
	f.StringVar(&ev.ProjectId, "project", "", "project_id")
	f.StringVar(&ev.UserId, "user", "", "user_id")
	f.StringVar(&ev.ActionId, "action", "", "action_id")
	f.StringVar(&ev.Vendor, "vendor", "", "vendor, e.g. openai")
	f.StringVar(&ev.Model, "model", "", "model")
	f.StringVar(&st, "status", "success", "success, error, cancelled or timeout")
	f.Int64Var(&ev.TokensInput, "tokens-in", 0, "input tokens")
	f.Int64Var(&ev.TokensOutput, "tokens-out", 0, "output tokens")
	f.Int64Var(&ev.DurationMs, "duration-ms", 0, "latency")
	f.StringVar(&cost, "cost-usd", "", "cost in USD")
	f.StringVar(&at, "at", "", "occurred_at, RFC 3339 (default now)")
	f.StringToStringVar(&meta, "meta", nil, "meta key=value, repeatable")
	for _, req := range []string{"project", "user", "action", "vendor", "model"} {
		_ = send.MarkFlagRequired(req)
	}
	cmd := &cobra.Command{Use: "event", Short: "Usage events"}
	cmd.AddCommand(send)
	return cmd
}
