package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"tokenusage/internal/server"
)

// All admin commands inherit --dsn from root via PersistentFlags. They
// can also accept --dsn after the subcommand name (cobra handles flag
// interspersion). Positional args come last.

func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "User and API-key management (local CLI; reads/writes the same DB the server uses)",
	}
	cmd.AddCommand(
		newUserAddCmd(),
		newUserListCmd(),
		newKeyCreateCmd(),
		newKeyListCmd(),
		newKeyRevokeCmd(),
	)
	return cmd
}

func mustOpenStore() (*server.Store, error) {
	if dsn == "" {
		return nil, errors.New("--dsn (or $TOKENUSAGE_DSN) is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return server.NewStore(ctx, dsn)
}

// ----- user-add ------------------------------------------------------------
func newUserAddCmd() *cobra.Command {
	var email string
	cmd := &cobra.Command{
		Use:   "user-add <user_id>",
		Short: "Create a new user",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			st, err := mustOpenStore()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.CreateUser(context.Background(), args[0], email); err != nil {
				return fmt.Errorf("create user: %w", err)
			}
			fmt.Printf("created user %q\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "optional email")
	return cmd
}

// ----- user-list -----------------------------------------------------------
func newUserListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "user-list",
		Short: "List all users",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			st, err := mustOpenStore()
			if err != nil {
				return err
			}
			defer st.Close()
			users, err := st.ListUsers(context.Background())
			if err != nil {
				return err
			}
			if len(users) == 0 {
				fmt.Println("(no users)")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "USER_ID\tEMAIL\tCREATED\tDISABLED")
			for _, u := range users {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%v\n",
					u.UserID, dash(u.Email), fmtTime(u.CreatedAt), u.Disabled)
			}
			return tw.Flush()
		},
	}
}

// ----- key-create ----------------------------------------------------------
func newKeyCreateCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "key-create <user_id>",
		Short: "Mint a new API key (the raw key is printed once)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			st, err := mustOpenStore()
			if err != nil {
				return err
			}
			defer st.Close()
			raw, prefix, err := st.CreateAPIKey(context.Background(), args[0], name)
			if err != nil {
				return fmt.Errorf("create key: %w", err)
			}
			fmt.Printf("api key for %q (prefix %s):\n\n   %s\n\nsave it now — the full key won't be shown again.\n",
				args[0], prefix, raw)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "optional label, e.g. 'macbook'")
	return cmd
}

// ----- key-list ------------------------------------------------------------
func newKeyListCmd() *cobra.Command {
	var userFilter string
	cmd := &cobra.Command{
		Use:   "key-list",
		Short: "List API keys (prefix only, never the raw key)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			st, err := mustOpenStore()
			if err != nil {
				return err
			}
			defer st.Close()
			keys, err := st.ListAPIKeys(context.Background(), userFilter)
			if err != nil {
				return err
			}
			if len(keys) == 0 {
				fmt.Println("(no keys)")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "PREFIX\tUSER\tNAME\tCREATED\tLAST_USED\tSTATUS")
			for _, k := range keys {
				status := "active"
				if k.Revoked {
					status = "revoked"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					k.Prefix, k.UserID, dash(k.Name),
					fmtTime(k.CreatedAt), fmtPtrTime(k.LastUsedAt), status)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&userFilter, "user", "", "filter by user_id (omit for all)")
	return cmd
}

// ----- key-revoke ----------------------------------------------------------
func newKeyRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "key-revoke <prefix>",
		Short: "Revoke an API key by its 12-char prefix",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			st, err := mustOpenStore()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.RevokeAPIKey(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Printf("revoked key with prefix %s\n", args[0])
			return nil
		},
	}
}

// ----- formatting helpers --------------------------------------------------
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
