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

	"github.com/rs/zerolog/log"
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

// Parse reads entries name:scope[+scope]:sha256hex, separated by commas or
// newlines, for example "ktms:write:9f86d0…,console:read:…". A line starting
// with # is a comment. Several entries may be active at once for rotation.
func Parse(spec string) (*Verifier, error) {
	v := &Verifier{byHash: map[[sha256.Size]byte]token{}}
	n := 0
	for _, line := range strings.Split(spec, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		for _, entry := range strings.Split(line, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			n++
			// Errors name the entry by position only: a mistyped entry may hold a raw token.
			parts := strings.Split(entry, ":")
			if len(parts) != 3 || parts[0] == "" {
				return nil, fmt.Errorf("token entry %d: want name:scope[+scope]:sha256hex", n)
			}
			sum, err := hex.DecodeString(parts[2])
			if err != nil || len(sum) != sha256.Size {
				return nil, fmt.Errorf("token entry %d: the third field must be the token's SHA-256 in hex (64 characters), not the token", n)
			}
			t := token{name: parts[0], scopes: map[Scope]bool{}}
			for _, sc := range strings.Split(parts[1], "+") {
				switch Scope(sc) {
				case Write, Read:
					t.scopes[Scope(sc)] = true
				default:
					return nil, fmt.Errorf("token entry %d: unknown scope (want write, read or write+read)", n)
				}
			}
			var key [sha256.Size]byte
			copy(key[:], sum)
			if _, dup := v.byHash[key]; dup {
				return nil, fmt.Errorf("token entry %d: the same token is listed twice; give it one entry with write+read", n)
			}
			v.byHash[key] = t
		}
	}
	return v, nil
}

// Load combines inline entries with those in file, so a token can live in a
// mounted secret. A file that yields no tokens is an error: naming one means
// auth was meant to be on.
func Load(inline, file string) (*Verifier, error) {
	v, err := Parse(inline)
	if err != nil {
		return nil, fmt.Errorf("TOKENMETER_TOKENS: %w", err)
	}
	if file == "" {
		return v, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("TOKENMETER_TOKENS_FILE: %w", err)
	}
	fromFile, err := Parse(string(b))
	if err != nil {
		return nil, fmt.Errorf("TOKENMETER_TOKENS_FILE %s: %w", file, err)
	}
	if !fromFile.Enabled() {
		return nil, fmt.Errorf("TOKENMETER_TOKENS_FILE %s holds no tokens", file)
	}
	for key, t := range fromFile.byHash {
		if _, dup := v.byHash[key]; dup {
			return nil, fmt.Errorf("token %q is listed in both TOKENMETER_TOKENS and TOKENMETER_TOKENS_FILE", t.name)
		}
		v.byHash[key] = t
	}
	return v, nil
}

// Enabled reports whether any token is configured.
func (v *Verifier) Enabled() bool { return v != nil && len(v.byHash) > 0 }

// Check returns the token's name when bearer may act with scope.
func (v *Verifier) Check(bearer string, scope Scope) (string, error) {
	t, err := v.lookup(bearer)
	if err != nil || t == nil {
		return "", err
	}
	if !t.scopes[scope] {
		return t.name, ErrForbidden
	}
	return t.name, nil
}

// Known returns the token's name when bearer is any configured token.
func (v *Verifier) Known(bearer string) (string, error) {
	t, err := v.lookup(bearer)
	if err != nil || t == nil {
		return "", err
	}
	return t.name, nil
}

// lookup returns nil, nil when auth is off.
func (v *Verifier) lookup(bearer string) (*token, error) {
	if !v.Enabled() {
		return nil, nil
	}
	t, ok := v.byHash[sha256.Sum256([]byte(bearer))]
	if bearer == "" || !ok {
		return nil, ErrUnauthenticated
	}
	return &t, nil
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
		if !v.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		name, err := v.Check(bearer(r.Header.Get("Authorization")), scope)
		switch {
		case errors.Is(err, ErrUnauthenticated):
			logDenied(r.Method+" "+r.URL.Path, name, err)
			w.Header().Set("WWW-Authenticate", `Bearer realm="light-tokenmeter"`)
			onError(w, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required")
		case errors.Is(err, ErrForbidden):
			logDenied(r.Method+" "+r.URL.Path, name, err)
			onError(w, http.StatusForbidden, "forbidden", "the token lacks the "+string(scope)+" scope")
		default:
			log.Debug().Str("token", name).Str("path", r.URL.Path).Msg("authenticated")
			next.ServeHTTP(w, r)
		}
	})
}

// UnaryInterceptor checks the scope each gRPC method needs. A method missing
// from scopes is refused, so a new RPC cannot ship open by accident.
func UnaryInterceptor(v *Verifier, scopes map[string]Scope) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !v.Enabled() || isHealth(info.FullMethod) {
			return handler(ctx, req)
		}
		scope, ok := scopes[info.FullMethod]
		if !ok {
			logDenied(info.FullMethod, "", errors.New("no scope configured"))
			return nil, status.Errorf(codes.PermissionDenied, "no scope is configured for %s", info.FullMethod)
		}
		name, err := v.Check(bearer(incoming(ctx)), scope)
		switch {
		case errors.Is(err, ErrUnauthenticated):
			logDenied(info.FullMethod, name, err)
			return nil, status.Error(codes.Unauthenticated, "a valid bearer token is required")
		case errors.Is(err, ErrForbidden):
			logDenied(info.FullMethod, name, err)
			return nil, status.Error(codes.PermissionDenied, "the token lacks the "+string(scope)+" scope")
		}
		log.Debug().Str("token", name).Str("method", info.FullMethod).Msg("authenticated")
		return handler(ctx, req)
	}
}

// StreamInterceptor guards the only streams the server has, health watches
// and reflection: health is open for probes, reflection needs any token so the
// schema is not handed to anyone who reaches the port.
func StreamInterceptor(v *Verifier) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !v.Enabled() || isHealth(info.FullMethod) {
			return handler(srv, ss)
		}
		if !strings.HasPrefix(info.FullMethod, "/grpc.reflection.") {
			logDenied(info.FullMethod, "", errors.New("no scope configured"))
			return status.Errorf(codes.PermissionDenied, "no scope is configured for %s", info.FullMethod)
		}
		if _, err := v.Known(bearer(incoming(ss.Context()))); err != nil {
			logDenied(info.FullMethod, "", err)
			return status.Error(codes.Unauthenticated, "a valid bearer token is required")
		}
		return handler(srv, ss)
	}
}

func incoming(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("authorization"); len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

func isHealth(method string) bool { return strings.HasPrefix(method, "/grpc.health.") }

// logDenied stays at Warn: Error would send every probe of the port to Sentry.
func logDenied(target, token string, err error) {
	log.Warn().Str("target", target).Str("token", token).Err(err).Msg("request refused")
}
