package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func mustParse(t *testing.T, spec string) *Verifier {
	t.Helper()
	v, err := Parse(spec)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return v
}

func TestParse(t *testing.T) {
	v := mustParse(t, "ktms:write:"+hash("w")+",\n# comment\nconsole:read:"+hash("r")+"\nops:write+read:"+hash("rw"))
	if !v.Enabled() {
		t.Fatal("verifier with tokens reports disabled")
	}
	if got := len(v.byHash); got != 3 {
		t.Fatalf("parsed %d tokens, want 3", got)
	}

	for _, bad := range []string{
		"ktms:write",                    // two fields
		"ktms:write:plaintext-token",    // not a hash
		"ktms:admin:" + hash("x"),       // unknown scope
		":write:" + hash("x"),           // no name
		"ktms:write:" + hash("x")[:62],  // short hash
		"ktms:write:" + hash("x") + ":", // four fields
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted a bad entry", bad)
		}
	}

	if v := mustParse(t, " \n "); v.Enabled() {
		t.Error("blank spec enables auth")
	}
}

func TestParseErrorHidesToken(t *testing.T) {
	_, err := Parse("ktms:write:sk-live-secret")
	if err == nil || strings.Contains(err.Error(), "sk-live-secret") {
		t.Fatalf("error must reject the entry without echoing it: %v", err)
	}
	_, err = Parse("sk-live-secret-without-colons")
	if err == nil || strings.Contains(err.Error(), "without-colons") {
		t.Fatalf("error must reject the entry without echoing it: %v", err)
	}
}

func TestLoadMergesFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(file, []byte("# rotated in\nnew:write:"+hash("new")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := Load("old:write:"+hash("old"), file)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tok := range []string{"old", "new"} {
		if _, err := v.Check(tok, Write); err != nil {
			t.Errorf("token %q from inline+file: %v", tok, err)
		}
	}
	if _, err := Load("", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing tokens file was ignored")
	}
}

func TestCheck(t *testing.T) {
	v := mustParse(t, "ktms:write:"+hash("w")+",console:read:"+hash("r"))
	cases := []struct {
		bearer string
		scope  Scope
		want   error
	}{
		{"w", Write, nil},
		{"r", Read, nil},
		{"w", Read, ErrForbidden},
		{"r", Write, ErrForbidden},
		{"", Write, ErrUnauthenticated},
		{"nope", Read, ErrUnauthenticated},
	}
	for _, c := range cases {
		if _, err := v.Check(c.bearer, c.scope); err != c.want {
			t.Errorf("Check(%q, %s) = %v, want %v", c.bearer, c.scope, err, c.want)
		}
	}

	var off *Verifier
	if _, err := off.Check("", Write); err != nil {
		t.Errorf("nil verifier must allow: %v", err)
	}
}

func TestRequire(t *testing.T) {
	v := mustParse(t, "ktms:write:"+hash("w"))
	h := Require(v, Write, func(w http.ResponseWriter, status int, code, _ string) {
		http.Error(w, code, status)
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	cases := map[string]int{
		"":           http.StatusUnauthorized,
		"Bearer bad": http.StatusUnauthorized,
		"Basic w":    http.StatusUnauthorized,
		"Bearer w":   http.StatusNoContent,
		"bearer  w ": http.StatusNoContent,
	}
	for header, want := range cases {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Authorization %q: status %d, want %d", header, w.Code, want)
		}
		if want == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("Authorization %q: 401 without WWW-Authenticate", header)
		}
	}

	readOnly := Require(mustParse(t, "console:read:"+hash("r")), Write, func(w http.ResponseWriter, status int, code, _ string) {
		http.Error(w, code, status)
	}, http.NotFoundHandler())
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer r")
	w := httptest.NewRecorder()
	readOnly.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("read token on a write route: status %d, want 403", w.Code)
	}
}

func TestUnaryInterceptor(t *testing.T) {
	v := mustParse(t, "ktms:write:"+hash("w"))
	icpt := UnaryInterceptor(v, map[string]Scope{"/svc/Write": Write, "/svc/Read": Read})
	ok := func(context.Context, any) (any, error) { return "ok", nil }
	call := func(method, header string) codes.Code {
		ctx := context.Background()
		if header != "" {
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", header))
		}
		_, err := icpt(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, ok)
		return status.Code(err)
	}

	cases := []struct {
		method, header string
		want           codes.Code
	}{
		{"/svc/Write", "Bearer w", codes.OK},
		{"/svc/Write", "", codes.Unauthenticated},
		{"/svc/Write", "Bearer bad", codes.Unauthenticated},
		{"/svc/Read", "Bearer w", codes.PermissionDenied},
		{"/svc/Unlisted", "Bearer w", codes.PermissionDenied},
		{"/grpc.health.v1.Health/Check", "", codes.OK},
	}
	for _, c := range cases {
		if got := call(c.method, c.header); got != c.want {
			t.Errorf("%s with %q: %v, want %v", c.method, c.header, got, c.want)
		}
	}

	open := UnaryInterceptor(nil, nil)
	if _, err := open(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/svc/Unlisted"}, ok); err != nil {
		t.Errorf("auth off must allow every method: %v", err)
	}
}
