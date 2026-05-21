package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"tokenusage/cmd/watcher/cli/svc"
)

// NewStopCmd returns the `stop` cobra subcommand.
func NewStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the installed service (without removing it)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doStop()
		},
	}
	return cmd
}

func doStop() error {
	self, _ := os.Executable()
	candidates := svc.DefaultBackends()
	stopped := 0
	for _, backend := range candidates {
		if !svc.IsInstalled(backend, self) {
			fmt.Printf("→ backend=%s: not installed; skipping\n", backend)
			continue
		}
		fmt.Printf("→ stopping backend=%s\n", backend)
		if err := stopOne(backend, self); err != nil {
			fmt.Printf("  failed: %v\n", err)
			continue
		}
		stopped++
	}
	if stopped == 0 {
		return errors.New("nothing to stop — no candidate backend has a unit on disk")
	}
	return nil
}

func stopOne(backend, self string) error {
	if backend == "supervisor" {
		return svc.StopSupervisor()
	}
	cfg, err := svc.Config(backend, self, "x", "x", nil)
	if err != nil {
		return err
	}
	s, err := service.New(svc.Runner{}, cfg)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	if err := s.Stop(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	fmt.Printf("  stopped %s (backend=%s)\n", svc.Name, backend)
	return nil
}
