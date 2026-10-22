//go:build upgrade

// Package upgrade drives scripts/upgrade-test.sh: an older release records
// events (TestSeed), this build must keep and serve them (TestVerify), and the
// older release must still serve on the migrated schema (TestServe).
package upgrade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

type event struct {
	EventID      string `json:"event_id"`
	UserID       string `json:"user_id"`
	ProjectID    string `json:"project_id"`
	ActionID     string `json:"action_id"`
	Vendor       string `json:"vendor"`
	Model        string `json:"model"`
	Status       string `json:"status"`
	TokensInput  int64  `json:"tokens_input"`
	TokensOutput int64  `json:"tokens_output"`
	CostNanoUSD  int64  `json:"cost_nanousd"`
}

type totals struct{ Requests, Tokens, Cost int64 }

type state struct {
	Want   totals
	Seeded []event // resent after the upgrade: every one must be a duplicate
	// The old release predates cost_nanousd: it stores no cost and reports
	// none, so cost is compared only while this build answers.
	OldHasNoCost bool
}

func base(t *testing.T) string {
	addr := os.Getenv("TM_HTTP")
	if addr == "" {
		t.Skip("run through scripts/upgrade-test.sh")
	}
	return "http://" + addr
}

func load(t *testing.T) state {
	var st state
	b, err := os.ReadFile(os.Getenv("UPGRADE_STATE"))
	if err == nil && len(b) > 0 {
		err = json.Unmarshal(b, &st)
	}
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return st
}

func save(t *testing.T, st state) {
	b, _ := json.Marshal(st)
	if err := os.WriteFile(os.Getenv("UPGRADE_STATE"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newEvents(n int, model string) []event {
	out := make([]event, n)
	for i := range out {
		out[i] = event{EventID: uuid.NewString(), UserID: fmt.Sprintf("u%d", i%5), ProjectID: "upgrade",
			ActionID: "a", Vendor: "openai", Model: model, Status: "success",
			TokensInput: int64(i%11 + 1), TokensOutput: 7, CostNanoUSD: int64(i * 1000)}
	}
	return out
}

func add(w *totals, events []event) {
	for _, e := range events {
		w.Requests++
		w.Tokens += e.TokensInput + e.TokensOutput
		w.Cost += e.CostNanoUSD
	}
}

// record posts a batch and returns how many were created and duplicates.
func record(t *testing.T, events []event) (created, dups int) {
	body, _ := json.Marshal(map[string]any{"events": events})
	resp, err := http.Post(base(t)+"/v1/usage/events:batch", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Created    int `json:"created_count"`
		Duplicates int `json:"duplicate_count"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
		t.Fatalf("record batch: HTTP %d", resp.StatusCode)
	}
	return out.Created, out.Duplicates
}

func summary(t *testing.T) (totals, error) {
	q := url.Values{}
	q.Set("from", time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339))
	q.Set("to", time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339))
	q.Set("project_id", "upgrade")
	resp, err := http.Get(base(t) + "/v1/usage/summary?" + q.Encode())
	if err != nil {
		return totals{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Rows []struct {
			Tokens   int64 `json:"tokens_total"`
			Requests int64 `json:"request_count"`
			Cost     int64 `json:"cost_nanousd"`
		} `json:"rows"`
	}
	if resp.StatusCode != http.StatusOK {
		return totals{}, fmt.Errorf("summary: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return totals{}, err
	}
	var got totals
	for _, r := range out.Rows {
		got.Requests += r.Requests
		got.Tokens += r.Tokens
		got.Cost += r.Cost
	}
	return got, nil
}

// waitFor allows for ClickHouse lag (the old release buffers, this one copies
// through the outbox), but not for a wrong total.
func waitFor(t *testing.T, want totals, ignoreCost bool) {
	t.Helper()
	var got totals
	var err error
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		got, err = summary(t)
		if ignoreCost {
			got.Cost = want.Cost
		}
		if err == nil && got == want {
			return
		}
	}
	t.Fatalf("summary totals %+v (err %v), want %+v", got, err, want)
}

func TestSeed(t *testing.T) {
	var st state
	for i := 0; i < 3; i++ {
		batch := newEvents(100, "seed")
		if created, _ := record(t, batch); created != len(batch) {
			t.Fatalf("seed batch %d: created %d of %d", i, created, len(batch))
		}
		add(&st.Want, batch)
		st.Seeded = append(st.Seeded, batch[:20]...)
	}
	waitFor(t, st.Want, true)
	if got, err := summary(t); err == nil && got.Cost == 0 {
		st.OldHasNoCost, st.Want.Cost = true, 0
	}
	waitFor(t, st.Want, false)
	save(t, st)
}

func TestVerify(t *testing.T) {
	st := load(t)
	waitFor(t, st.Want, false)

	created, dups := record(t, st.Seeded)
	if created != 0 || dups != len(st.Seeded) {
		t.Fatalf("resending events from before the upgrade: created %d, duplicates %d", created, dups)
	}
	batch := newEvents(150, "after")
	if created, _ := record(t, batch); created != len(batch) {
		t.Fatalf("created %d of %d", created, len(batch))
	}
	add(&st.Want, batch)
	waitFor(t, st.Want, false)
	save(t, st)
}

func TestServe(t *testing.T) {
	st := load(t)
	waitFor(t, st.Want, st.OldHasNoCost)
	batch := newEvents(50, "rolling")
	if created, _ := record(t, batch); created != len(batch) {
		t.Fatalf("old release on the migrated schema: created %d of %d", created, len(batch))
	}
	add(&st.Want, batch)
	waitFor(t, st.Want, st.OldHasNoCost)
}
