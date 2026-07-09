package main

import (
	"os"
	"os/user"
	"path/filepath"

	"github.com/spf13/cobra"

	"tokenusage/cmd/watcher/cli"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	// os.UserHomeDir only reads $HOME, which is absent under init-started
	// supervisord — an empty home would collapse stateDir and defaultRoot
	// into cwd-relative paths. Fall back to the passwd entry.
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		if u, uerr := user.Current(); uerr == nil {
			home = u.HomeDir
		}
	}
	stateDir := filepath.Join(home, ".token-usage-watcher")
	defaultRoot := filepath.Join(home, ".claude", "projects")

	cfg := &cli.RunConfig{}
	runE := func(c *cobra.Command, _ []string) error {
		return cli.DoRun(c, cfg, home)
	}

	root := &cobra.Command{
		Use:   "token-usage-watcher",
		Short: "Tail Claude Code / Codex transcripts and push token usage to a server",
		Long: `Without a subcommand, runs the watcher (same as "run").

Subcommands:
  run        run the watcher (default)
  install    install as a service (systemd user / system / supervisord)
  uninstall  stop and remove the service
  start      start the installed service
  stop       stop the installed service (without removing it)
  restart    restart the installed service
  status     show service status
  logs       show service log output (-f to tail-follow)
  cleanup    stop, clear local spool/buffer, restart (with --with-checkpoint to force re-scan)`,
		SilenceUsage: true,
		RunE:         runE,
	}
	cli.BindRunFlags(root, cfg, defaultRoot, stateDir)

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run the watcher (default; same as no subcommand)",
		RunE:  runE,
	}
	cli.BindRunFlags(runCmd, cfg, defaultRoot, stateDir)

	root.AddCommand(
		runCmd,
		cli.NewInstallCmd(),
		cli.NewUninstallCmd(),
		cli.NewStartCmd(),
		cli.NewStopCmd(),
		cli.NewRestartCmd(),
		cli.NewStatusCmd(),
		cli.NewLogsCmd(),
		cli.NewCleanupCmd(),
	)
	return root
}
