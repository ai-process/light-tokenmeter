package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ai-process/light-tokenmeter/pkg/client"
)

// target is where and how to reach light-tokenmeter. Flags win over
// environment variables, which win over the selected context.
type target struct {
	context   string
	addr      string
	tls       bool
	tlsSet    bool
	http      string
	tokenFile string

	ctx      *Context // the selected context, nil without one
	ctxName  string
	envToken string // TOKENMETER_TOKEN, unless a context was asked for by name
	warn     io.Writer
	resolved bool
	httpOnce sync.Once
	hc       *http.Client
}

// resolve fills addr, tls and http from the environment and the context, on
// first use, so commands that need no service never read the contexts file.
func (t *target) resolve() error {
	if t.resolved {
		return nil
	}
	t.resolved = true
	asked := firstNonEmpty(t.context, os.Getenv("TOKENMETER_CONTEXT"))
	cs, err := loadContexts()
	var noHome contextsUnreachable
	if errors.As(err, &noHome) && asked == "" {
		// No home directory (cron, systemd): flags and environment still work.
		cs, err = &Contexts{}, nil
	}
	if err != nil {
		return err
	}
	name := firstNonEmpty(asked, cs.Current)
	if name != "" {
		c, ok := cs.Contexts[name]
		if !ok {
			return fmt.Errorf("no context %q (tokenmeter context list)", name)
		}
		t.ctx, t.ctxName = c, name
	}
	c := t.ctx
	if c == nil {
		c = &Context{}
	}
	// A context asked for by name wins over the environment, so its token never
	// goes to an address left in the shell, nor the shell's token to its address.
	env := os.Getenv
	if asked != "" {
		env = func(string) string { return "" }
	}
	t.envToken = env("TOKENMETER_TOKEN")
	t.addr = firstNonEmpty(t.addr, env("TOKENMETER_ADDR"), c.Addr, "localhost:9090")
	if !t.tlsSet {
		if v := env("TOKENMETER_TLS"); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return usageError{fmt.Errorf("TOKENMETER_TLS=%q: want true or false", v)}
			}
			t.tls = b
		} else {
			t.tls = c.TLS
		}
	}
	t.http = firstNonEmpty(t.http, env("TOKENMETER_HTTP"), c.HTTP)
	if t.http == "" {
		host, _, err := net.SplitHostPort(t.addr)
		if err != nil {
			host = t.addr
		}
		scheme := "http"
		if t.tls {
			scheme = "https"
		}
		t.http = scheme + "://" + net.JoinHostPort(host, "8080")
	}
	t.http = strings.TrimRight(t.http, "/")
	return nil
}

// token reads the bearer token from --token-file, TOKENMETER_TOKEN or the
// context, never from a flag value. Empty means none: the service may run
// without auth, and then it refuses nothing.
func (t *target) token() (string, error) {
	if err := t.resolve(); err != nil {
		return "", err
	}
	if t.tokenFile != "" {
		return readKeyFile(t.tokenFile)
	}
	if t.envToken != "" {
		return t.envToken, nil
	}
	if t.ctx != nil && !t.ctx.Token.empty() {
		k, err := t.ctx.Token.read(t.ctxName)
		if err != nil {
			return "", fmt.Errorf("token of context %s: %w", t.ctxName, err)
		}
		return k, nil
	}
	return "", nil
}

func (t *target) client() (*client.Client, error) {
	tok, err := t.token()
	if err != nil {
		return nil, err
	}
	if tok != "" && !t.tls && !loopback(t.addr) {
		fmt.Fprintf(t.warn, "tokenmeter: warning: sending the token to %s without TLS; pass --tls unless this network is private\n", t.addr)
	}
	return client.New(client.Options{Addr: t.addr, Token: tok, TLS: t.tls})
}

// httpBase is the HTTP API's base URL.
func (t *target) httpBase() (string, error) {
	if err := t.resolve(); err != nil {
		return "", err
	}
	return t.http, nil
}

// httpClient is shared: tokenmeter mcp probes health for as long as it runs.
func (t *target) httpClient() *http.Client {
	t.httpOnce.Do(func() {
		t.hc = &http.Client{Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			IdleConnTimeout: 90 * time.Second,
		}}
	})
	return t.hc
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
