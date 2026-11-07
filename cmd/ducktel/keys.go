package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/keys"
)

// keysReloadInterval is how often a keys file is checked for changes, so a
// key added or revoked in the ConfigMap takes effect without a restart.
const keysReloadInterval = 30 * time.Second

// keysFileHelp is shared by serve and mcp.
const keysFileHelp = "JSON list of per-client keys (name, sha256, scopes, tenants); reloaded on change (env: DUCKTEL_KEYS_FILE)"

// openKeys loads the keys file named by the flag or DUCKTEL_KEYS_FILE and
// watches it for changes. It returns nil when no file is configured.
func openKeys(cmd *cobra.Command, path string) (*keys.Store, error) {
	if !cmd.Flags().Changed("keys-file") {
		if v := os.Getenv("DUCKTEL_KEYS_FILE"); v != "" {
			path = v
		}
	}
	if path == "" {
		return nil, nil
	}
	store, err := keys.Open(path)
	if err != nil {
		return nil, err
	}
	go store.Watch(keysReloadInterval, make(chan struct{}))
	log.Printf("keys: loaded %s (%d keys)", path, store.Len())
	return store, nil
}

// principalFromHeader resolves an already-authenticated request's
// Authorization header to its principal: the shared token is legacy,
// anything else is looked up as a key.
func principalFromHeader(token string, store *keys.Store) func(http.Header) *keys.Principal {
	return func(h http.Header) *keys.Principal {
		_, cred, _ := strings.Cut(h.Get("Authorization"), " ")
		if token != "" && subtle.ConstantTimeCompare([]byte(cred), []byte(token)) == 1 {
			return keys.LegacyPrincipal
		}
		if p := store.Lookup(cred); p != nil {
			return p
		}
		// Unreachable behind the HTTP auth check; fail closed regardless.
		return &keys.Principal{Name: "unknown"}
	}
}

func keyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Create per-client keys without the token ever being printed",
		Long: `Create per-client keys.

ducktel stores only the SHA-256 of a key. "key new" writes a fresh token to a
file (mode 0600, never overwriting) and prints only its name and hash; put the
hash into the keys file (or Terraform's clients map) and point the client at
the token file. The token itself is never written to stdout, so it does not end
up in a terminal scrollback, a CI log or an AI assistant's context.`,
	}

	var name, out string
	newCmd := &cobra.Command{
		Use:          "new",
		SilenceUsage: true,
		Short:        "Write a new token to --out and print its hash",
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				return errors.New("--out is required: the token is only ever written to a file")
			}
			hash, err := keys.WriteNew(out)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{
				"name": name, "sha256": hash, "token_file": out,
			})
		},
	}
	newCmd.Flags().StringVar(&name, "name", "", "Key name, as it will appear in the keys file and in logs")
	newCmd.Flags().StringVar(&out, "out", "", "File to write the token to (created with mode 0600; must not exist)")

	hashCmd := &cobra.Command{
		Use:          "hash FILE",
		SilenceUsage: true,
		Short:        "Print the SHA-256 of the token stored in FILE",
		Args:         cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hash, err := keys.HashFile(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), hash)
			return nil
		},
	}

	cmd.AddCommand(newCmd, hashCmd)
	return cmd
}
