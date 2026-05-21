package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"tokenusage/cmd/watcher/cli/svc"
)

// NewStartCmd returns the `start` cobra subcommand.
func NewStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the installed service",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doStart()
		},
	}
	return cmd
}

func doStart() error {
	self, _ := os.Executable()
	candidates := svc.DefaultBackends()
	started := 0
	for _, backend := range candidates {
		if !svc.IsInstalled(backend, self) {
			fmt.Printf("→ backend=%s: not installed; skipping\n", backend)
			continue
		}
		fmt.Printf("→ starting backend=%s\n", backend)
		if err := startOne(backend, self); err != nil {
			fmt.Printf("  failed: %v\n", err)
			continue
		}
		started++
	}
	if started == 0 {
		return errors.New("nothing to start — no candidate backend has a unit on disk")
	}
	return nil
}

func startOne(backend, self string) error {
	if backend == "supervisor" {
		return svc.StartSupervisor()
	}
	cfg, err := svc.Config(backend, self, "x", "x", nil)
	if err != nil {
		return err
	}
	s, err := service.New(svc.Runner{}, cfg)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	st, _ := s.Status()
	fmt.Printf("  started %s (backend=%s, status=%s)\n", svc.Name, backend, svc.StatusName(st))
	return nil
}
