package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/joho/godotenv"
	"github.com/kalfian/paper-plane/internal/apikey"
	"github.com/kalfian/paper-plane/internal/config"
	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/store"
)

// tokenCmd manages access tokens directly in the instance's database, for
// headless setups (e.g. `docker compose exec paperplane /paperplane token
// create --name claude --scope mcp`). It returns a process exit code.
func tokenCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, "usage: paperplane token create|list|revoke ...\n")
		return 2
	}
	_ = godotenv.Load() // best-effort, same as the server
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, "config:", err)
		return 1
	}
	ctx := context.Background()
	st, err := store.NewSQLite(ctx, filepath.Join(cfg.DataDir, "paperplane.db"))
	if err != nil {
		fmt.Fprintln(stderr, "open database:", err)
		return 1
	}
	defer func() { _ = st.Close() }()
	return runTokenCmd(ctx, st, cfg, args, stdout, stderr)
}

// runTokenCmd is tokenCmd after the store is open (split out for tests).
func runTokenCmd(ctx context.Context, st store.Store, cfg config.Config, args []string, stdout, stderr io.Writer) int {
	switch args[0] {
	case "create":
		fset := flag.NewFlagSet("token create", flag.ContinueOnError)
		fset.SetOutput(stderr)
		name := fset.String("name", "", "label for the token (required)")
		scope := fset.String("scope", string(model.ScopeAll), "all | api | mcp")
		if err := fset.Parse(args[1:]); err != nil {
			return 2
		}
		n := strings.TrimSpace(*name)
		if n == "" || len(n) > 60 {
			fmt.Fprintln(stderr, "--name is required (max 60 characters)")
			return 2
		}
		sc := model.KeyScope(strings.TrimSpace(*scope))
		if !sc.Valid() {
			fmt.Fprintln(stderr, `--scope must be "all", "api", or "mcp"`)
			return 2
		}
		token, hash := apikey.Generate()
		k := &model.APIKey{ID: store.NewID(), Name: n, KeyHash: hash, Scope: sc}
		if err := st.CreateAPIKey(ctx, k); err != nil {
			fmt.Fprintln(stderr, "create token:", err)
			return 1
		}
		fmt.Fprintf(stderr, "Created token %q (id %s, scope: %s). It is shown only once:\n", n, k.ID, sc.Label())
		fmt.Fprintln(stdout, token)
		if sc.AllowsMCP() {
			base := cfg.AppURL
			if base == "" {
				base = "http://localhost:" + cfg.Port
			}
			fmt.Fprintf(stderr, "\nConnect Claude Code:\n  claude mcp add --transport http paperplane %s/api/mcp --header \"Authorization: Bearer %s\"\n", base, token)
		}
		return 0

	case "list":
		keys, err := st.ListAPIKeys(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "list tokens:", err)
			return 1
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tSCOPE\tCREATED\tLAST USED")
		for _, k := range keys {
			last := "never"
			if k.LastUsedAt != nil {
				last = k.LastUsedAt.Format("2006-01-02 15:04")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.ID, k.Name, k.Scope, k.CreatedAt.Format("2006-01-02 15:04"), last)
		}
		_ = tw.Flush()
		return 0

	case "revoke", "delete":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: paperplane token revoke <id>")
			return 2
		}
		if err := st.DeleteAPIKey(ctx, args[1]); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				fmt.Fprintln(stderr, "no token with id", args[1])
				return 1
			}
			fmt.Fprintln(stderr, "revoke token:", err)
			return 1
		}
		fmt.Fprintln(stderr, "Revoked", args[1])
		return 0
	}
	fmt.Fprintf(stderr, "unknown token subcommand %q (want create, list, revoke)\n", args[0])
	return 2
}
