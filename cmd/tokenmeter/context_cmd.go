package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func (a *app) contextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Name services and where their tokens live (~/.config/tokenmeter/config.yaml)",
	}
	complete := func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		cs, err := loadContexts()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		return cs.names(), cobra.ShellCompDirectiveNoFileComp
	}

	var format string
	list := &cobra.Command{
		Use: "list", Short: "List contexts; * marks the current one", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			cs, err := loadContexts()
			if err != nil {
				return err
			}
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			if p.json() {
				return p.data(cs)
			}
			var rows [][]string
			for _, n := range cs.names() {
				c, cur := cs.Contexts[n], " "
				if n == cs.Current {
					cur = "*"
				}
				rows = append(rows, []string{cur, n, c.Addr, yesNo(c.TLS), c.HTTP, c.Token.String()})
			}
			return p.rows([]string{"", "NAME", "ADDR", "TLS", "HTTP", "TOKEN"}, rows)
		},
	}
	outputFlag(list, &format)

	use := &cobra.Command{
		Use: "use NAME", Short: "Make a context the current one", Args: cobra.ExactArgs(1), ValidArgsFunction: complete,
		RunE: func(_ *cobra.Command, args []string) error {
			cs, err := loadContexts()
			if err != nil {
				return err
			}
			if cs.Contexts[args[0]] == nil {
				return fmt.Errorf("no context %q", args[0])
			}
			cs.Current = args[0]
			return cs.save()
		},
	}

	var addr, httpURL, token string
	var useTLS bool
	set := &cobra.Command{
		Use:   "set NAME [--addr HOST:PORT] [--tls] [--http URL] [--token SOURCE]",
		Short: "Create or change a context",
		Long: `Create or change a context. A token SOURCE is env:VAR, file:PATH or keychain
(macOS Keychain or the Linux secret service; fill it with tokenmeter context
store-token). The first context created becomes the current one.`,
		Args: cobra.ExactArgs(1), ValidArgsFunction: complete,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, err := loadContexts()
			if err != nil {
				return err
			}
			c := cs.Contexts[args[0]]
			if c == nil {
				c = &Context{}
				cs.Contexts[args[0]] = c
			}
			f := cmd.Flags()
			if f.Changed("addr") {
				c.Addr = addr
			}
			if f.Changed("tls") {
				c.TLS = useTLS
			}
			if f.Changed("http") {
				c.HTTP = httpURL
			}
			if f.Changed("token") {
				if c.Token, err = parseKeySource(token); err != nil {
					return usageError{err}
				}
			}
			if cs.Current == "" {
				cs.Current = args[0]
			}
			return cs.save()
		},
	}
	// Local flags shadow the global connection flags: here they describe the context.
	set.Flags().StringVar(&addr, "addr", "", "gRPC API address host:port")
	set.Flags().BoolVar(&useTLS, "tls", false, "use TLS")
	set.Flags().StringVar(&httpURL, "http", "", "HTTP API base URL")
	set.Flags().StringVar(&token, "token", "", "token source: env:VAR, file:PATH or keychain")

	del := &cobra.Command{
		Use: "delete NAME", Short: "Remove a context (keychain entries stay)", Args: cobra.ExactArgs(1), ValidArgsFunction: complete,
		RunE: func(_ *cobra.Command, args []string) error {
			cs, err := loadContexts()
			if err != nil {
				return err
			}
			if cs.Contexts[args[0]] == nil {
				return fmt.Errorf("no context %q", args[0])
			}
			delete(cs.Contexts, args[0])
			if cs.Current == args[0] {
				cs.Current = ""
			}
			return cs.save()
		},
	}

	var toFile bool
	store := &cobra.Command{
		Use:   "store-token NAME [--file]",
		Short: "Read a token from stdin and keep it in the keychain (or a 0600 file)",
		Long: `Read a token from stdin, without echo on a terminal, and keep it in the system
keychain; --file keeps it in ~/.config/tokenmeter/NAME.token (mode 0600)
instead. The context's token source is updated to match.`,
		Args: cobra.ExactArgs(1), ValidArgsFunction: complete,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			cs, err := loadContexts()
			if err != nil {
				return err
			}
			c := cs.Contexts[name]
			if c == nil {
				return fmt.Errorf("no context %q (tokenmeter context set %s first)", name, name)
			}
			plain, err := a.readSecret("Token: ")
			if err != nil {
				return err
			}
			var src KeySource
			if toFile {
				dir, err := configDir()
				if err != nil {
					return err
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				path := filepath.Join(dir, safeName(name)+".token")
				if err := writePrivate(path, []byte(plain+"\n")); err != nil {
					return err
				}
				src = KeySource{File: path}
			} else {
				if err := keychainSet(name, plain); err != nil {
					return fmt.Errorf("%w (pass --file to keep the token in a file instead)", err)
				}
				src = KeySource{Keychain: true}
			}
			c.Token = src
			if err := cs.save(); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Stored the token of %s in %s.\n", name, src)
			return nil
		},
	}
	store.Flags().BoolVar(&toFile, "file", false, "keep the token in a 0600 file instead of the keychain")

	cmd.AddCommand(list, use, set, del, store)
	return cmd
}

// readSecret reads one line from stdin, without echo on a terminal.
func (a *app) readSecret(prompt string) (string, error) {
	var line string
	if f, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(a.stderr, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.stderr)
		if err != nil {
			return "", err
		}
		line = string(b)
	} else {
		s, err := bufio.NewReader(a.stdin).ReadString('\n')
		if err != nil && s == "" {
			return "", errors.New("nothing on stdin")
		}
		line = s
	}
	if line = strings.TrimSpace(line); line == "" {
		return "", errors.New("empty input")
	}
	return line, nil
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == os.PathSeparator {
			return '_'
		}
		return r
	}, s)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
