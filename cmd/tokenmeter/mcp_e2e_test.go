//go:build e2e

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPStdio runs the built binary's tokenmeter mcp over stdio and checks
// that usage_summary agrees with GET /v1/usage/summary on the same window.
// scripts/cli-e2e.sh runs it against a live service.
func TestMCPStdio(t *testing.T) {
	bin := os.Getenv("TOKENMETER_E2E_BIN")
	if bin == "" {
		t.Skip("TOKENMETER_E2E_BIN is set by scripts/cli-e2e.sh")
	}
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e"}, nil).Connect(ctx,
		&mcp.CommandTransport{Command: exec.Command(bin, "mcp")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	to := time.Now().UTC().Truncate(time.Second).Add(time.Minute)
	from := to.Add(-24 * time.Hour)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "usage_summary", Arguments: map[string]any{
		"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339), "group_by": []string{"project"}}})
	if err != nil || res.IsError {
		t.Fatalf("usage_summary: %v %+v", err, res)
	}
	var rep struct {
		Rows []struct {
			Project     string `json:"project"`
			CostNanoUSD int64  `json:"cost_nanousd"`
			Requests    int64  `json:"requests"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &rep); err != nil {
		t.Fatal(err)
	}

	q := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}, "group_by_project_id": {"true"}}
	req, _ := http.NewRequest(http.MethodGet, os.Getenv("TOKENMETER_HTTP")+"/v1/usage/summary?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+os.Getenv("TOKENMETER_TOKEN"))
	hr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer hr.Body.Close()
	var api struct {
		Rows []struct {
			ProjectID    string          `json:"project_id"`
			CostNanousd  json.RawMessage `json:"cost_nanousd"`
			RequestCount json.RawMessage `json:"request_count"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(hr.Body).Decode(&api); err != nil {
		t.Fatal(err)
	}
	num := func(raw json.RawMessage) int64 {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			n, _ := strconv.ParseInt(s, 10, 64)
			return n
		}
		var n int64
		_ = json.Unmarshal(raw, &n)
		return n
	}
	want := map[string][2]int64{}
	for _, r := range api.Rows {
		w := want[r.ProjectID]
		want[r.ProjectID] = [2]int64{w[0] + num(r.CostNanousd), w[1] + num(r.RequestCount)}
	}
	if len(rep.Rows) != len(want) || len(want) == 0 {
		t.Fatalf("projects: MCP %d, API %d", len(rep.Rows), len(want))
	}
	for _, r := range rep.Rows {
		if w := want[r.Project]; w != [2]int64{r.CostNanoUSD, r.Requests} {
			t.Errorf("%s: MCP %d nano-USD over %d requests, API %d over %d", r.Project, r.CostNanoUSD, r.Requests, w[0], w[1])
		}
	}
}
