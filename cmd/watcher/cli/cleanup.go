package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"tokenusage/cmd/watcher/cli/svc"
)

// NewCleanupCmd returns the `cleanup` cobra subcommand.
func NewCleanupCmd() *cobra.Command {
	var withCheckpoint bool
	var stateDirOverride string
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Stop service, clear local spool buffer, start service",
		Long: `Three-step recovery flow:

  1. stop every installed candidate backend
  2. delete ~/.token-usage-watcher/buffer/* (pending un-sent batches)
  3. start every installed candidate backend

With --with-checkpoint, ALSO deletes ~/.token-usage-watcher/checkpoint.json
so the next scan re-reads every source file from the start. Server dedups
via (message_id, request_id), so previously-ingested records become
duplicates while genuinely missing ones become accepted=… in the per-batch
log. Use this to recover from spool data-loss (e.g. older builds that
dropped 401-rejected batches off the buffer).

NOTE: the buffer and checkpoint paths are relative to the running
process's HOME — if the service was installed under a different user
(supervisord runs as root by default), invoke cleanup as that user
(typically sudo).`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doCleanup(withCheckpoint, stateDirOverride)
		},
	}
	cmd.Flags().BoolVar(&withCheckpoint, "with-checkpoint", false,
		"also delete checkpoint.json (forces a full re-scan of all source files)")
	cmd.Flags().StringVar(&stateDirOverride, "state-dir", "",
		"state directory (default: $HOME/.token-usage-watcher)")
	return cmd
}

func doCleanup(withCheckpoint bool, stateDirOverride string) error {
	self, _ := os.Executable()
	candidates := svc.DefaultBackends()
	installed := []string{}
	for _, backend := range candidates {
		if svc.IsInstalled(backend, self) {
			installed = append(installed, backend)
		}
	}
	if len(installed) == 0 {
		fmt.Println("→ no service installed; cleaning state files only")
	}

	// Step 1: stop everything we can find.
	for _, backend := range installed {
		fmt.Printf("→ stopping backend=%s\n", backend)
		if err := stopOne(backend, self); err != nil {
			fmt.Printf("  failed: %v (continuing — cleanup still useful)\n", err)
		}
	}

	// Step 2: clear state files.
	stateDir := stateDirOverride
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".token-usage-watcher")
	}
	bufferDir := filepath.Join(stateDir, "buffer")
	if n, err := cleanDirContents(bufferDir); err != nil {
		fmt.Printf("→ buffer cleanup: %v\n", err)
	} else {
		fmt.Printf("→ removed %d file(s) from %s\n", n, bufferDir)
	}
	if withCheckpoint {
		ckpt := filepath.Join(stateDir, "checkpoint.json")
		if err := os.Remove(ckpt); err == nil {
			fmt.Printf("→ removed %s — next scan will re-read every source file from the start\n", ckpt)
		} else if os.IsNotExist(err) {
			fmt.Printf("→ %s already absent\n", ckpt)
		} else {
			fmt.Printf("→ checkpoint cleanup: %v\n", err)
		}
	}

	// Step 3: start everything we stopped.
	for _, backend := range installed {
		fmt.Printf("→ starting backend=%s\n", backend)
		if err := startOne(backend, self); err != nil {
			fmt.Printf("  failed: %v\n", err)
		}
	}
	return nil
}

// cleanDirContents removes every regular file directly under dir (does
// not recurse into subdirs; does not touch the dir itself). Missing dir
// is treated as "nothing to clean" rather than an error — the buffer
// dir is created lazily by the watcher's first spool, so it may simply
// not exist yet.
func cleanDirContents(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			n++
		}
	}
	return n, nil
}
