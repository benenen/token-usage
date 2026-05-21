package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"tokenusage/cmd/watcher/cli/svc"
)

// NewUninstallCmd returns the `uninstall` cobra subcommand.
func NewUninstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the installed service",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doUninstall()
		},
	}
	return cmd
}

func doUninstall() error {
	self, _ := os.Executable()
	candidates := svc.DefaultBackends()
	uninstalled := 0
	for _, backend := range candidates {
		if !svc.IsInstalled(backend, self) {
			fmt.Printf("→ backend=%s: not installed; skipping\n", backend)
			continue
		}
		fmt.Printf("→ uninstalling backend=%s\n", backend)
		if err := uninstallOne(backend, self); err != nil {
			fmt.Printf("  failed: %v\n", err)
			continue
		}
		uninstalled++
	}
	if uninstalled == 0 {
		return errors.New("nothing to uninstall — no candidate backend has a unit on disk")
	}
	return nil
}

func uninstallOne(backend, self string) error {
	if backend == "supervisor" {
		return svc.UninstallSupervisor()
	}
	cfg, err := svc.Config(backend, self, "x", "x", nil) // env values irrelevant for uninstall
	if err != nil {
		return err
	}
	s, err := service.New(svc.Runner{}, cfg)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	_ = s.Stop()
	if err := s.Uninstall(); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	fmt.Printf("  uninstalled %s (backend=%s)\n", svc.Name, backend)
	return nil
}
