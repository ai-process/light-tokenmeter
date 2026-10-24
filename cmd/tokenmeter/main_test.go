package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ai-process/light-tokenmeter/internal/auth"
	"github.com/ai-process/light-tokenmeter/internal/repository"
	"github.com/ai-process/light-tokenmeter/internal/server"
	"github.com/ai-process/light-tokenmeter/internal/service"
	"github.com/ai-process/light-tokenmeter/internal/testdb"
)

func callIn(stdin string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func call(args ...string) (int, string, string) { return callIn("", args...) }

// isolate points the CLI at an empty contexts file and clears the environment.
func isolate(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("TOKENMETER_CONFIG", p)
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(p))
	for _, k := range []string{"TOKENMETER_ADDR", "TOKENMETER_TLS", "TOKENMETER_HTTP", "TOKENMETER_TOKEN", "TOKENMETER_CONTEXT"} {
		t.Setenv(k, "")
	}
	return p
}

func TestUsageErrors(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{
		{"nonsense"}, {"usage"}, {"usage", "sumary"}, {"usage", "summary", "--status", "ok"},
		{"usage", "summary", "--group-by", "colour"}, {"usage", "summary", "--last", "7x"},
		{"usage", "summary", "--from", "2026-10-02", "--to", "2026-10-01"}, {"usage", "summary", "--last", "1d", "--from", "2026-10-01"},
		{"usage", "top", "--by", "vibes"}, {"usage", "by-meta", "--key", "job_id"}, {"usage", "export", "--format", "xml"},
		{"token", "new", "--name", "x"}, {"token", "new", "--name", "a:b", "--scope", "read"}, {"token", "new", "--name", "x", "--scope", "admin"},
		{"event", "send", "--project", "p"}, {"usage", "summary", "-o", "yaml"},
	} {
		if code, _, errs := call(args...); code != exitUsage {
			t.Errorf("tokenmeter %v = %d (%s), want %d", args, code, errs, exitUsage)
		}
	}
	for _, args := range [][]string{{"version"}, {"completion", "fish"}, {"--help"}} {
		if code, _, errs := call(args...); code != 0 {
			t.Errorf("tokenmeter %v = %d: %s", args, code, errs)
		}
	}
}

func TestTokenNewAndHash(t *testing.T) {
	isolate(t)
	code, out, _ := call("token", "new", "--name", "ktms", "--scope", "write,read")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || len(lines[0]) != 64 {
		t.Fatalf("token new = %d %q", code, out)
	}
	tok, line := lines[0], lines[1]
	sum := sha256.Sum256([]byte(tok))
	if line != "ktms:write+read:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("entry %q", line)
	}
	if _, err := auth.Parse(line); err != nil {
		t.Fatalf("the service does not accept the entry: %v", err)
	}
	if _, out, _ := callIn(tok+"\n", "token", "hash"); strings.TrimSpace(out) != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash = %q", out)
	}
	if _, out, _ := callIn(tok+"\n", "token", "hash", "--name", "ktms", "--scope", "write,read"); strings.TrimSpace(out) != line {
		t.Fatalf("hash entry = %q", out)
	}
	_, out, _ = call("token", "new", "--name", "c", "--scope", "read", "-o", "json")
	var v struct{ Token, Entry string }
	if err := json.Unmarshal([]byte(out), &v); err != nil || !strings.HasPrefix(v.Entry, "c:read:") || len(v.Token) != 64 {
		t.Fatalf("token new -o json = %s", out)
	}
}

func TestContexts(t *testing.T) {
	p := isolate(t)
	if code, _, errs := call("context", "set", "prod", "--addr", "tm.example.com:443", "--tls", "--token", "env:PROD_TM"); code != 0 {
		t.Fatalf("context set = %d %s", code, errs)
	}
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("contexts file: %v", err)
	}
	t.Setenv("PROD_TM", "tok-env")
	tg := target{}
	if err := tg.resolve(); err != nil {
		t.Fatal(err)
	}
	if tg.addr != "tm.example.com:443" || !tg.tls || tg.http != "https://tm.example.com:8080" {
		t.Fatalf("resolved %+v", &tg)
	}
	if tok, _ := tg.token(); tok != "tok-env" {
		t.Fatalf("token = %q", tok)
	}
	if code, _, errs := callIn("tok-file\n", "context", "store-token", "prod", "--file"); code != 0 {
		t.Fatalf("store-token = %d %s", code, errs)
	}
	tg = target{}
	if tok, err := tg.token(); err != nil || tok != "tok-file" {
		t.Fatalf("token after store-token = %q %v", tok, err)
	}
	t.Setenv("TOKENMETER_TOKEN", "tok-flagenv")
	tg = target{}
	if tok, _ := tg.token(); tok != "tok-flagenv" {
		t.Fatalf("the environment does not win over the context: %q", tok)
	}
	if _, out, _ := call("context", "list"); !strings.Contains(out, "*  prod") {
		t.Fatalf("context list: %s", out)
	}

	// A context asked for by name beats the shell's address and token.
	t.Setenv("TOKENMETER_ADDR", "localhost:9090")
	tg = target{context: "prod"}
	if tok, _ := tg.token(); tok != "tok-file" || tg.addr != "tm.example.com:443" {
		t.Fatalf("named context: addr %s token %q", tg.addr, tok)
	}
	t.Setenv("TOKENMETER_TLS", "yes please")
	if code, _, _ := call("usage", "summary"); code != exitUsage {
		t.Fatalf("bad TOKENMETER_TLS = %d, want %d", code, exitUsage)
	}
	if info, err := os.Stat(filepath.Join(filepath.Dir(p), "tokenmeter", "prod.token")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", info.Mode(), err)
	}
}

// serve runs the real gRPC and HTTP servers over a fresh database with these
// token entries, and returns their addresses.
func serve(t *testing.T, tokens string) (grpcAddr, httpBase string) {
	t.Helper()
	pool := testdb.Postgres(t)
	verifier, err := auth.Parse(tokens)
	if err != nil {
		t.Fatal(err)
	}
	repo := repository.New(pool)
	svc := service.NewUsageService(repo)
	free := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	grpcAddr, httpAddr := free(), free()
	g := server.NewGRPCServerWithService(svc, grpcAddr, verifier)
	h := server.NewHTTPServerWithService(svc, repo, httpAddr, verifier)
	go func() { _ = g.Start() }()
	go func() { _ = h.Start() }()
	t.Cleanup(g.Stop)
	for _, a := range []string{grpcAddr, httpAddr} {
		for i := 0; i < 100; i++ {
			if c, err := net.Dial("tcp", a); err == nil {
				_ = c.Close()
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return grpcAddr, "http://" + httpAddr
}

func TestAgainstTheService(t *testing.T) {
	isolate(t)
	_, out, _ := call("token", "new", "--name", "writer", "--scope", "write")
	w := strings.Split(strings.TrimSpace(out), "\n")
	_, out, _ = call("token", "new", "--name", "reader", "--scope", "read")
	r := strings.Split(strings.TrimSpace(out), "\n")
	addr, base := serve(t, w[1]+","+r[1])
	t.Setenv("TOKENMETER_ADDR", addr)
	t.Setenv("TOKENMETER_HTTP", base)
	must := func(want int, args ...string) string {
		t.Helper()
		code, out, errs := call(args...)
		if code != want {
			t.Fatalf("tokenmeter %s = %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, want, out, errs)
		}
		return out
	}

	t.Setenv("TOKENMETER_TOKEN", w[0])
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	send := func(project, model, cost string, meta ...string) {
		args := []string{"event", "send", "--project", project, "--user", "u1", "--action", "a", "--vendor", "openai",
			"--model", model, "--tokens-in", "100", "--tokens-out", "50", "--cost-usd", cost, "--at", at}
		for _, m := range meta {
			args = append(args, "--meta", m)
		}
		must(0, args...)
	}
	send("p1", "m1", "0.01", "job_id=j1", "task_id=t1")
	send("p1", "m2", "0.02", "job_id=j1", "task_id=t2")
	send("p2", "m1", "0.5")
	if out := must(0, "event", "send", "--id", "550e8400-e29b-41d4-a716-446655440000", "--project", "p", "--user", "u", "--action", "a", "--vendor", "v", "--model", "m"); !strings.Contains(out, "Recorded") {
		t.Fatalf("event send: %s", out)
	}
	if out := must(0, "event", "send", "--id", "550e8400-e29b-41d4-a716-446655440000", "--project", "p", "--user", "u", "--action", "a", "--vendor", "v", "--model", "m"); !strings.Contains(out, "already") {
		t.Fatalf("duplicate event send: %s", out)
	}
	must(exitAuth, "usage", "summary") // a write token cannot read

	t.Setenv("TOKENMETER_TOKEN", r[0])
	must(exitAuth, "event", "send", "--project", "p", "--user", "u", "--action", "a", "--vendor", "v", "--model", "m")
	var sum struct {
		Rows []struct {
			ProjectID    string `json:"project_id"`
			CostNanousd  string `json:"cost_nanousd"`
			RequestCount string `json:"request_count"`
		} `json:"rows"`
	}
	out = must(0, "usage", "summary", "--last", "1d", "--group-by", "project", "-o", "json")
	if err := json.Unmarshal([]byte(out), &sum); err != nil {
		t.Fatalf("summary json: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, row := range sum.Rows {
		got[row.ProjectID] = row.CostNanousd + "/" + row.RequestCount
	}
	if got["p1"] != "30000000/2" || got["p2"] != "500000000/1" {
		t.Fatalf("summary by project = %v", got)
	}
	if out := must(0, "usage", "summary", "--last", "1d", "--group-by", "project"); !strings.Contains(out, "$0.0300") || !strings.Contains(out, "TOTAL") {
		t.Fatalf("summary table:\n%s", out)
	}
	if out := must(0, "usage", "top", "--last", "1d", "--by", "cost", "--dimension", "project"); !strings.Contains(strings.Split(out, "\n")[1], "p2") {
		t.Fatalf("top:\n%s", out)
	}
	if out := must(0, "usage", "by-meta", "--key", "job_id", "--value", "j1", "--group-by", "task_id"); !strings.Contains(out, "t1") || !strings.Contains(out, "t2") || !strings.Contains(out, "$0.0200") {
		t.Fatalf("by-meta:\n%s", out)
	}

	out = must(0, "usage", "export", "--last", "1d", "--group-by", "project")
	recs, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil || len(recs) < 3 || recs[0][0] != "day" {
		t.Fatalf("export csv: %v\n%s", err, out)
	}
	usd := map[string]string{}
	for _, rec := range recs[1:] {
		usd[rec[1]] = rec[len(rec)-2]
	}
	if usd["p1"] != "0.03" || usd["p2"] != "0.5" {
		t.Fatalf("export cost_usd = %v", usd)
	}
	if out := must(0, "usage", "export", "--last", "1d", "--format", "json"); !strings.Contains(out, `"rows"`) {
		t.Fatalf("export json: %s", out)
	}

	if out := must(0, "health"); !strings.Contains(out, "version dev") {
		t.Fatalf("health:\n%s", out)
	}
	t.Setenv("TOKENMETER_TOKEN", "")
	must(exitAuth, "usage", "summary")
}
