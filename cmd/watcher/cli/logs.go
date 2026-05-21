package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"tokenusage/cmd/watcher/cli/svc"
)

// NewLogsCmd returns the `logs` cobra subcommand.
func NewLogsCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show log output for the installed service",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doLogs(follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false,
		"follow new log lines (journalctl -f / tail -f / log stream); only the first installed backend is followed")
	return cmd
}

func doLogs(follow bool) error {
	self, _ := os.Executable()
	candidates := svc.DefaultBackends()
	shown := 0
	for _, backend := range candidates {
		if !svc.IsInstalled(backend, self) {
			continue
		}
		fmt.Printf("─── backend=%s ───\n", backend)
		if err := svc.ShowLogs(backend, follow); err != nil {
			fmt.Printf("  failed: %v\n", err)
			continue
		}
		shown++
		if follow {
			// -f never returns until interrupted; bail out of the loop
			// instead of trying to follow every installed backend at
			// once (interleaving is messy and rarely what users want).
			return nil
		}
	}
	if shown == 0 {
		return errors.New("nothing to show — no candidate backend has a unit on disk")
	}
	return nil
}
