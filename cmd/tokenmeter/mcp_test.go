package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/timestamppb"

	usagev1 "github.com/ai-process/light-tokenmeter/gen/usage/v1"
)

// mcpApp is an app wired as tokenmeter mcp wires itself, without serving.
func mcpApp(t *testing.T) (*app, error) {
	t.Helper()
	a := &app{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard, timeout: 30 * time.Second}
	a.t.warn = io.Discard
	c, err := a.t.client()
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := a.checkReadOnly(c); err != nil {
		return nil, err
	}
	a.shared = c
	return a, nil
}

func mcpSession(t *testing.T, a *app) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	go func() { _ = a.mcpServer().Run(context.Background(), st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func tool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, v any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		t.Fatalf("%s: %s", name, text)
	}
	if err := json.Unmarshal([]byte(text), v); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, text)
	}
}

func TestMCPAgainstTheService(t *testing.T) {
	isolate(t)
	tokens := map[string][]string{}
	for _, s := range []string{"write", "read", "write,read"} {
		_, out, _ := call("token", "new", "--name", strings.ReplaceAll(s, ",", "-"), "--scope", s)
		tokens[s] = strings.Split(strings.TrimSpace(out), "\n")
	}
	addr, base := serve(t, tokens["write"][1]+","+tokens["read"][1]+","+tokens["write,read"][1])
	t.Setenv("TOKENMETER_ADDR", addr)
	t.Setenv("TOKENMETER_HTTP", base)

	t.Setenv("TOKENMETER_TOKEN", tokens["write"][0])
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	for _, e := range [][]string{
		{"p1", "summarize", "success", "0.01", "job_id=j1"}, {"p1", "summarize", "error", "0.02", "job_id=j1"},
		{"p2", "classify", "success", "0.5", "job_id=j2"},
	} {
		if code, _, errs := call("event", "send", "--project", e[0], "--user", "u", "--action", e[1], "--vendor", "openai", "--model", "m",
			"--status", e[2], "--cost-usd", e[3], "--tokens-in", "100", "--tokens-out", "20", "--at", at, "--meta", e[4]); code != 0 {
			t.Fatalf("event send: %s", errs)
		}
	}

	// Tokens that can write are refused; so is one that cannot read.
	for _, s := range []string{"write", "write,read"} {
		t.Setenv("TOKENMETER_TOKEN", tokens[s][0])
		if _, err := mcpApp(t); err == nil {
			t.Fatalf("a %s token was accepted", s)
		}
	}
	t.Setenv("TOKENMETER_TOKEN", tokens["read"][0])
	a, err := mcpApp(t)
	if err != nil {
		t.Fatalf("a read token was refused: %v", err)
	}
	cs := mcpSession(t, a)
	tl, _ := cs.ListTools(context.Background(), nil)
	var names []string
	for _, x := range tl.Tools {
		names = append(names, x.Name)
		if !x.Annotations.ReadOnlyHint {
			t.Errorf("%s is not read-only", x.Name)
		}
	}
	slices.Sort(names)
	if want := []string{"cost_per_action", "health", "top_spenders", "usage_by_meta", "usage_summary"}; !slices.Equal(names, want) {
		t.Fatalf("tools = %v", names)
	}

	// The tool's sums are the API's.
	var rep struct {
		Rows []struct {
			Project     string `json:"project"`
			CostUSD     string `json:"cost_usd"`
			CostNanoUSD int64  `json:"cost_nanousd"`
			Requests    int64  `json:"requests"`
		} `json:"rows"`
		Total struct {
			CostUSD string `json:"cost_usd"`
		} `json:"total"`
	}
	tool(t, cs, "usage_summary", map[string]any{"last": "1d", "group_by": []string{"project"}}, &rep)
	now := time.Now().UTC()
	api, _, err := a.shared.Summary(context.Background(), &usagev1.GetUsageSummaryRequest{
		From: tsOf(now.Add(-24 * time.Hour)), To: tsOf(now.Add(time.Minute)), GroupByProjectId: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{}
	for _, r := range api {
		want[r.GetProjectId()] += r.GetCostNanousd()
	}
	for _, r := range rep.Rows {
		if want[r.Project] != r.CostNanoUSD {
			t.Errorf("%s: tool %d, API %d", r.Project, r.CostNanoUSD, want[r.Project])
		}
	}
	if len(rep.Rows) != 2 || rep.Rows[0].Project != "p2" || rep.Rows[1].CostUSD != "0.03" || rep.Total.CostUSD != "0.53" {
		t.Fatalf("usage_summary = %+v", rep)
	}

	var top struct {
		Rows []struct {
			Project string `json:"project"`
			Share   string `json:"share"`
		} `json:"rows"`
	}
	tool(t, cs, "top_spenders", map[string]any{"last": "1d"}, &top)
	if len(top.Rows) != 2 || top.Rows[0].Project != "p2" || top.Rows[0].Share != "94.3%" {
		t.Fatalf("top_spenders = %+v", top)
	}

	var actions struct {
		Actions []struct {
			Action        string `json:"action"`
			FailedShare   string `json:"failed_share"`
			FailedCostUSD string `json:"failed_cost_usd"`
			TokensPerReq  int64  `json:"tokens_per_request"`
		} `json:"actions"`
	}
	tool(t, cs, "cost_per_action", map[string]any{"last": "1d"}, &actions)
	var sum *struct {
		Action        string `json:"action"`
		FailedShare   string `json:"failed_share"`
		FailedCostUSD string `json:"failed_cost_usd"`
		TokensPerReq  int64  `json:"tokens_per_request"`
	}
	for i := range actions.Actions {
		if actions.Actions[i].Action == "summarize" {
			sum = &actions.Actions[i]
		}
	}
	if sum == nil || sum.FailedShare != "50.0%" || sum.FailedCostUSD != "0.02" || sum.TokensPerReq != 120 {
		t.Fatalf("cost_per_action = %+v", actions)
	}

	var meta struct {
		Total string `json:"total_cost_usd"`
	}
	tool(t, cs, "usage_by_meta", map[string]any{"key": "job_id", "value": "j1"}, &meta)
	if meta.Total != "0.03" {
		t.Fatalf("usage_by_meta total = %q", meta.Total)
	}
	var health struct {
		Ready bool `json:"ready"`
	}
	tool(t, cs, "health", nil, &health)
	if !health.Ready {
		t.Fatal("health not ready")
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "usage_summary", Arguments: map[string]any{"status": "fine"}})
	if err != nil || !res.IsError {
		t.Fatalf("a bad status was not a tool error: %v", err)
	}
	gp, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "usage_report", Arguments: map[string]string{"question": "cost of job j1"}})
	if err != nil || !strings.Contains(gp.Messages[0].Content.(*mcp.TextContent).Text, "cost of job j1") {
		t.Fatalf("usage_report prompt: %v", err)
	}
}

func TestMCPOverHTTP(t *testing.T) {
	isolate(t)
	_, out, _ := call("token", "new", "--name", "r", "--scope", "read")
	r := strings.Split(strings.TrimSpace(out), "\n")
	addr, base := serve(t, r[1])
	t.Setenv("TOKENMETER_ADDR", addr)
	t.Setenv("TOKENMETER_HTTP", base)
	t.Setenv("TOKENMETER_TOKEN", r[0])
	a, err := mcpApp(t)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	listen := l.Addr().String()
	_ = l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	token := "0123456789abcdef-test"
	go func() { _ = a.serveMCP(ctx, a.mcpServer(), listen, token) }()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", listen); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	res, err := http.Post("http://"+listen+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %v", err)
	}
	res.Body.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: "http://" + listen + "/mcp", HTTPClient: &http.Client{Transport: bearerRT{token}}, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	var rep struct {
		Note string `json:"note"`
	}
	tool(t, cs, "usage_summary", map[string]any{"last": "1d"}, &rep)
	if rep.Note != "no traffic in this window" {
		t.Fatalf("empty window: %+v", rep)
	}
}

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func tsOf(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }
