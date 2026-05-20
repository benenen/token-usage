package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"tokenusage/internal/server"
)

// runAdmin dispatches `token-usage-server admin [--dsn …] <cmd> [flags] [args]`.
//
// --dsn (or $TOKENUSAGE_DSN) may appear either before or after the
// subcommand. User management is local-CLI only by design — no HTTP
// admin surface.
//
// Convention inside subcommands: flags BEFORE positional args, e.g.
//   admin --dsn ... user-add --email x@y.com alice
//   admin user-add --email x@y.com alice          # same, dsn via env
//   admin key-create --name laptop alice
func runAdmin(args []string) {
	// Layer 1: pull a global --dsn that may sit before the subcommand.
	// flag.Parse stops at the first non-flag token (the subcommand name),
	// so anything after the subcommand stays in cmdArgs untouched.
	globalFS := flag.NewFlagSet("admin", flag.ContinueOnError)
	globalFS.SetOutput(io.Discard)
	globalDSN := globalFS.String("dsn", os.Getenv("TOKENUSAGE_DSN"), "")
	if err := globalFS.Parse(args); err != nil {
		printAdminHelp()
		os.Exit(2)
	}
	rest := globalFS.Args()
	if len(rest) == 0 {
		printAdminHelp()
		os.Exit(2)
	}
	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "user-add":
		cmdUserAdd(cmdArgs, *globalDSN)
	case "user-list":
		cmdUserList(cmdArgs, *globalDSN)
	case "key-create":
		cmdKeyCreate(cmdArgs, *globalDSN)
	case "key-list":
		cmdKeyList(cmdArgs, *globalDSN)
	case "key-revoke":
		cmdKeyRevoke(cmdArgs, *globalDSN)
	case "-h", "--help", "help":
		printAdminHelp()
	default:
		fmt.Fprintf(os.Stderr, "unknown admin command: %s\n\n", cmd)
		printAdminHelp()
		os.Exit(2)
	}
}

func printAdminHelp() {
	fmt.Fprint(os.Stderr, `usage: token-usage-server admin [--dsn <DSN>] <command> [flags] [args]

global:
  --dsn       PostgreSQL DSN (env: TOKENUSAGE_DSN). May appear before or
              after the subcommand.

commands:
  user-add    [--email <e>] <user_id>            create a user
  user-list                                      list users
  key-create  [--name <label>] <user_id>         mint a new api key (printed once)
  key-list    [--user <user_id>]                 list api keys (prefix only)
  key-revoke  <prefix>                           revoke an api key by prefix

subcommand flags come BEFORE positionals.
`)
}

// ----- helpers ----------------------------------------------------------

func mustOpenStore(dsn string) *server.Store {
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "--dsn (or $TOKENUSAGE_DSN) is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := server.NewStore(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store: %v\n", err)
		os.Exit(1)
	}
	return st
}

func positional(fs *flag.FlagSet, name string) string {
	if fs.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "missing positional argument: %s\n", name)
		os.Exit(2)
	}
	return fs.Arg(0)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

func fmtPtrTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return fmtTime(*t)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ----- commands ---------------------------------------------------------

func cmdUserAdd(args []string, defaultDSN string) {
	fs := flag.NewFlagSet("user-add", flag.ExitOnError)
	email := fs.String("email", "", "optional email")
	dsn := fs.String("dsn", defaultDSN, "PostgreSQL DSN")
	_ = fs.Parse(args)
	userID := positional(fs, "user_id")

	st := mustOpenStore(*dsn)
	defer st.Close()
	if err := st.CreateUser(context.Background(), userID, *email); err != nil {
		fmt.Fprintf(os.Stderr, "create user: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("created user %q\n", userID)
}

func cmdUserList(args []string, defaultDSN string) {
	fs := flag.NewFlagSet("user-list", flag.ExitOnError)
	dsn := fs.String("dsn", defaultDSN, "PostgreSQL DSN")
	_ = fs.Parse(args)

	st := mustOpenStore(*dsn)
	defer st.Close()
	users, err := st.ListUsers(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "list users: %v\n", err)
		os.Exit(1)
	}
	if len(users) == 0 {
		fmt.Println("(no users)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "USER_ID\tEMAIL\tCREATED\tDISABLED")
	for _, u := range users {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\n",
			u.UserID, dash(u.Email), fmtTime(u.CreatedAt), u.Disabled)
	}
	_ = tw.Flush()
}

func cmdKeyCreate(args []string, defaultDSN string) {
	fs := flag.NewFlagSet("key-create", flag.ExitOnError)
	name := fs.String("name", "", "optional label, e.g. 'macbook'")
	dsn := fs.String("dsn", defaultDSN, "PostgreSQL DSN")
	_ = fs.Parse(args)
	userID := positional(fs, "user_id")

	st := mustOpenStore(*dsn)
	defer st.Close()
	raw, prefix, err := st.CreateAPIKey(context.Background(), userID, *name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create key: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("api key for %q (prefix %s):\n\n   %s\n\n", userID, prefix, raw)
	fmt.Println("save it now — the full key won't be shown again.")
}

func cmdKeyList(args []string, defaultDSN string) {
	fs := flag.NewFlagSet("key-list", flag.ExitOnError)
	userFilter := fs.String("user", "", "filter by user_id (omit for all)")
	dsn := fs.String("dsn", defaultDSN, "PostgreSQL DSN")
	_ = fs.Parse(args)

	st := mustOpenStore(*dsn)
	defer st.Close()
	keys, err := st.ListAPIKeys(context.Background(), *userFilter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list keys: %v\n", err)
		os.Exit(1)
	}
	if len(keys) == 0 {
		fmt.Println("(no keys)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PREFIX\tUSER\tNAME\tCREATED\tLAST_USED\tSTATUS")
	for _, k := range keys {
		status := "active"
		if k.Revoked {
			status = "revoked"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			k.Prefix, k.UserID, dash(k.Name), fmtTime(k.CreatedAt), fmtPtrTime(k.LastUsedAt), status)
	}
	_ = tw.Flush()
}

func cmdKeyRevoke(args []string, defaultDSN string) {
	fs := flag.NewFlagSet("key-revoke", flag.ExitOnError)
	dsn := fs.String("dsn", defaultDSN, "PostgreSQL DSN")
	_ = fs.Parse(args)
	prefix := positional(fs, "prefix")

	st := mustOpenStore(*dsn)
	defer st.Close()
	if err := st.RevokeAPIKey(context.Background(), prefix); err != nil {
		fmt.Fprintf(os.Stderr, "revoke: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("revoked key with prefix %s\n", prefix)
}
