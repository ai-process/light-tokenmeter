// Command tokenmeter reads and writes light-tokenmeter: usage summaries,
// top spenders, usage by your own metadata and exports, test events, the
// service's health, and new tokens for TOKENMETER_TOKENS.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// app is one invocation: the streams and the connection flags.
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	t              target
	timeout        time.Duration
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{stdin: stdin, stdout: stdout, stderr: stderr}
	a.t.warn = stderr
	root := a.root()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.Execute()
	if err == nil {
		return exitOK
	}
	var c codeError
	if errors.As(err, &c) {
		return c.code
	}
	fmt.Fprintln(stderr, "tokenmeter:", errText(err))
	if isUsage(err) {
		return exitUsage
	}
	return exitCode(err)
}

// codeError ends the command with a code and nothing more to print.
type codeError struct{ code int }

func (e codeError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// isUsage tells cobra's own errors from the API's. Argument and flag errors
// are wrapped as usageError by strict; these two come from cobra's dispatch.
func isUsage(err error) bool {
	var u usageError
	if errors.As(err, &u) {
		return true
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "unknown command") || strings.HasPrefix(msg, "required flag")
}

// strict makes argument errors usage errors, and a command group called
// without a known subcommand one too instead of help with exit 0.
func strict(cmd *cobra.Command) {
	if cmd.HasSubCommands() && cmd.Run == nil && cmd.RunE == nil {
		cmd.Args = cobra.NoArgs
		cmd.RunE = func(c *cobra.Command, _ []string) error {
			return usageError{fmt.Errorf("%s needs a subcommand (%s --help)", c.CommandPath(), c.CommandPath())}
		}
	}
	if v := cmd.Args; v != nil {
		cmd.Args = func(c *cobra.Command, args []string) error {
			if err := v(c, args); err != nil {
				return usageError{err}
			}
			return nil
		}
	}
	for _, c := range cmd.Commands() {
		strict(c)
	}
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "tokenmeter",
		Short: "Read and write light-tokenmeter",
		Long: `tokenmeter reads usage from light-tokenmeter and sends it test events.

Connection: --context, or --addr (TOKENMETER_ADDR, default localhost:9090, the
gRPC API) and --tls (TOKENMETER_TLS=true); the HTTP API at --http
(TOKENMETER_HTTP, default port 8080 of the same host). The token comes from
--token-file, TOKENMETER_TOKEN or the context: read for reports, write for
events. Without one the call goes out unauthenticated.

` + exitCodesHelp,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			a.t.tlsSet = cmd.Flags().Changed("tls")
		},
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	pf := root.PersistentFlags()
	pf.StringVar(&a.t.context, "context", "", "context from ~/.config/tokenmeter/config.yaml (TOKENMETER_CONTEXT)")
	pf.StringVar(&a.t.addr, "addr", "", "gRPC API address host:port (TOKENMETER_ADDR)")
	pf.BoolVar(&a.t.tls, "tls", false, "use TLS (TOKENMETER_TLS)")
	pf.StringVar(&a.t.http, "http", "", "HTTP API base URL (TOKENMETER_HTTP)")
	pf.StringVar(&a.t.tokenFile, "token-file", "", "read the token from this file instead of TOKENMETER_TOKEN")
	pf.DurationVar(&a.timeout, "timeout", time.Minute, "time limit for each command")

	root.AddCommand(a.usageCmd(), a.tokenCmd(), a.eventCmd(), a.healthCmd(), a.mcpCmd(), a.contextCmd(), &cobra.Command{
		Use: "version", Short: "Print the version", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintln(a.stdout, version)
			return nil
		},
	})
	for _, c := range root.Commands() {
		strict(c)
	}
	return root
}

// ctx bounds one command by --timeout.
func (a *app) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), a.timeout)
}

// outputFlag adds -o to a command that prints data.
func outputFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVarP(dst, "output", "o", "table", "output format: table, json or csv")
	_ = cmd.RegisterFlagCompletionFunc("output", cobra.FixedCompletions([]string{"table", "json", "csv"}, cobra.ShellCompDirectiveNoFileComp))
}
