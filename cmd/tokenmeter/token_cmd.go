package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// entry is a TOKENMETER_TOKENS line: name:scope[+scope]:sha256hex.
func entry(name string, scopes []string, token string) string {
	sum := sha256.Sum256([]byte(token))
	return name + ":" + strings.Join(scopes, "+") + ":" + hex.EncodeToString(sum[:])
}

func checkScopes(name string, scopes []string) error {
	if name == "" || strings.ContainsAny(name, ":,\n#") {
		return usageError{fmt.Errorf("--name %q: want a name without : , # or newlines", name)}
	}
	if len(scopes) == 0 {
		return usageError{fmt.Errorf("--scope is required: read, write or both")}
	}
	for _, s := range scopes {
		if s != "read" && s != "write" {
			return usageError{fmt.Errorf("scope %q: want read or write", s)}
		}
	}
	return nil
}

func (a *app) tokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Tokens for TOKENMETER_TOKENS"}

	var name, format string
	var scopes []string
	mint := &cobra.Command{
		Use:   "new --name NAME --scope read|write[,...]",
		Short: "Make a token; print it once, with its TOKENMETER_TOKENS entry",
		Long: `Make a random token for a client and print it once, with the entry to add to
TOKENMETER_TOKENS (or TOKENMETER_TOKENS_FILE): name:scope[+scope]:sha256. The
service keeps only the hash. Nothing is sent anywhere.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := checkScopes(name, scopes); err != nil {
				return err
			}
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return err
			}
			tok := hex.EncodeToString(b)
			line := entry(name, scopes, tok)
			if format == "json" {
				p := &printer{w: a.stdout, format: "json"}
				return p.data(map[string]any{"name": name, "scopes": scopes, "token": tok, "entry": line})
			}
			if format != "table" {
				return usageError{fmt.Errorf("-o %q: want table or json", format)}
			}
			fmt.Fprintln(a.stderr, "Token for the client. It is shown only now; store it in your secret manager:")
			fmt.Fprintln(a.stdout, tok)
			fmt.Fprintln(a.stderr, "Entry for TOKENMETER_TOKENS:")
			fmt.Fprintln(a.stdout, line)
			return nil
		},
	}
	mint.Flags().StringVar(&name, "name", "", "who the token is for, logged with each call")
	mint.Flags().StringSliceVar(&scopes, "scope", nil, "read, write or read,write")
	mint.Flags().StringVarP(&format, "output", "o", "table", "table or json")
	_ = mint.MarkFlagRequired("name")
	_ = mint.RegisterFlagCompletionFunc("scope", cobra.FixedCompletions([]string{"read", "write"}, cobra.ShellCompDirectiveNoFileComp))

	var hName string
	var hScopes []string
	hash := &cobra.Command{
		Use:   "hash [--name NAME --scope SCOPES]",
		Short: "Hash a token read from stdin; with --name and --scope print its entry",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			tok, err := a.readSecret("Token: ")
			if err != nil {
				return err
			}
			if hName == "" && len(hScopes) == 0 {
				sum := sha256.Sum256([]byte(tok))
				fmt.Fprintln(a.stdout, hex.EncodeToString(sum[:]))
				return nil
			}
			if err := checkScopes(hName, hScopes); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, entry(hName, hScopes, tok))
			return nil
		},
	}
	hash.Flags().StringVar(&hName, "name", "", "the token's name")
	hash.Flags().StringSliceVar(&hScopes, "scope", nil, "read, write or read,write")

	cmd.AddCommand(mint, hash)
	return cmd
}
