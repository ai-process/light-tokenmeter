// Package auth checks bearer tokens. Only SHA-256 hashes of the tokens are
// configured, so the service's settings never hold a usable secret.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Scope is what a token may do.
type Scope string

const (
	Write Scope = "write" // record usage events
	Read  Scope = "read"  // query summaries and meta usage
)

var (
	ErrUnauthenticated = errors.New("auth: missing or unknown token")
	ErrForbidden       = errors.New("auth: token lacks the scope")
)

type token struct {
	name   string
	scopes map[Scope]bool
}

// Verifier holds the configured tokens; one with none lets every call in.
type Verifier struct {
	byHash map[[sha256.Size]byte]token
}

// Parse reads comma- or newline-separated entries name:scope[+scope]:sha256hex,
// for example "ktms:write:9f86d0…,console:read:…". Several entries may share a
// name, which is how a token is rotated without downtime.
func Parse(spec string) (*Verifier, error) {
	v := &Verifier{byHash: map[[sha256.Size]byte]token{}}
	for i, entry := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		entry = strings.TrimSpace(entry)
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 3 || parts[0] == "" {
			// The entry may be a pasted raw token, so only its position is shown.
			return nil, fmt.Errorf("token entry %d: want name:scope[+scope]:sha256hex", i+1)
		}
		sum, err := hex.DecodeString(parts[2])
		if err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("token %q: the third field must be the token's SHA-256 in hex (64 characters), not the token", parts[0])
		}
		t := token{name: parts[0], scopes: map[Scope]bool{}}
		for _, s := range strings.Split(parts[1], "+") {
			switch Scope(s) {
			case Write, Read:
				t.scopes[Scope(s)] = true
			default:
				return nil, fmt.Errorf("token %q: unknown scope %q (want write, read or write+read)", parts[0], s)
			}
		}
		var key [sha256.Size]byte
		copy(key[:], sum)
		v.byHash[key] = t
	}
	return v, nil
}

// Load combines inline entries with those in file (one per line, # starts
// a comment), so a token can live in a mounted secret.
func Load(inline, file string) (*Verifier, error) {
	spec := inline
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read tokens file: %w", err)
		}
		spec += "\n" + string(b)
	}
	return Parse(spec)
}

// Enabled reports whether any token is configured.
func (v *Verifier) Enabled() bool { return v != nil && len(v.byHash) > 0 }

// Check returns the token's name when bearer may act with scope.
func (v *Verifier) Check(bearer string, scope Scope) (string, error) {
	if !v.Enabled() {
		return "", nil
	}
	if bearer == "" {
		return "", ErrUnauthenticated
	}
	t, ok := v.byHash[sha256.Sum256([]byte(bearer))]
	if !ok {
		return "", ErrUnauthenticated
	}
	if !t.scopes[scope] {
		return t.name, ErrForbidden
	}
	return t.name, nil
}

func bearer(header string) string {
	scheme, value, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

// Require wraps an HTTP handler with a scope check. onError writes the
// refusal in the service's own error shape.
func Require(v *Verifier, scope Scope, onError func(w http.ResponseWriter, status int, code, message string), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := v.Check(bearer(r.Header.Get("Authorization")), scope)
		switch {
		case errors.Is(err, ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Bearer realm="light-tokenmeter"`)
			onError(w, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required")
		case errors.Is(err, ErrForbidden):
			onError(w, http.StatusForbidden, "forbidden", "the token lacks the "+string(scope)+" scope")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// UnaryInterceptor checks the scope each gRPC method needs. A method missing
// from scopes is refused, so a new RPC cannot ship open by accident; health
// and reflection stay reachable.
func UnaryInterceptor(v *Verifier, scopes map[string]Scope) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if isExempt(info.FullMethod) || !v.Enabled() {
			return handler(ctx, req)
		}
		scope, ok := scopes[info.FullMethod]
		if !ok {
			return nil, status.Errorf(codes.PermissionDenied, "no scope is configured for %s", info.FullMethod)
		}
		var header string
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get("authorization"); len(vals) > 0 {
				header = vals[0]
			}
		}
		switch _, err := v.Check(bearer(header), scope); {
		case errors.Is(err, ErrUnauthenticated):
			return nil, status.Error(codes.Unauthenticated, "a valid bearer token is required")
		case errors.Is(err, ErrForbidden):
			return nil, status.Error(codes.PermissionDenied, "the token lacks the "+string(scope)+" scope")
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor refuses every stream while auth is on: the service has
// no streaming RPCs, so one can only be reflection, which stays open.
func StreamInterceptor(v *Verifier) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if isExempt(info.FullMethod) || !v.Enabled() {
			return handler(srv, ss)
		}
		return status.Errorf(codes.PermissionDenied, "no scope is configured for %s", info.FullMethod)
	}
}

func isExempt(method string) bool {
	return strings.HasPrefix(method, "/grpc.health.") || strings.HasPrefix(method, "/grpc.reflection.")
}
