package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"tokenusage/cmd/watcher/cli/svc"
)

// NewRestartCmd returns the `restart` cobra subcommand.
func NewRestartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the installed service",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doRestart()
		},
	}
	return cmd
}

func doRestart() error {
	self, _ := os.Executable()
	candidates := svc.DefaultBackends()
	restarted := 0
	for _, backend := range candidates {
		if !svc.IsInstalled(backend, self) {
			fmt.Printf("→ backend=%s: not installed; skipping\n", backend)
			continue
		}
		fmt.Printf("→ restarting backend=%s\n", backend)
		if err := restartOne(backend, self); err != nil {
			fmt.Printf("  failed: %v\n", err)
			continue
		}
		restarted++
	}
	if restarted == 0 {
		return errors.New("nothing to restart — no candidate backend has a unit on disk")
	}
	return nil
}

func restartOne(backend, self string) error {
	if backend == "supervisor" {
		return svc.RestartSupervisor()
	}
	cfg, err := svc.Config(backend, self, "x", "x", nil) // env values irrelevant for restart
	if err != nil {
		return err
	}
	s, err := service.New(svc.Runner{}, cfg)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	if err := s.Restart(); err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	st, _ := s.Status()
	fmt.Printf("  restarted %s (backend=%s, status=%s)\n", svc.Name, backend, svc.StatusName(st))
	return nil
}
