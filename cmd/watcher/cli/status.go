package cli

import (
	"fmt"
	"os"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"tokenusage/cmd/watcher/cli/svc"
)

// NewStatusCmd returns the `status` cobra subcommand.
func NewStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show service status across every per-OS default backend",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doStatus()
		},
	}
	return cmd
}

func doStatus() error {
	self, _ := os.Executable()
	for _, backend := range svc.DefaultBackends() {
		fmt.Printf("─── backend=%s ───\n", backend)
		statusOne(backend, self)
		fmt.Println()
	}
	return nil
}

func statusOne(backend, self string) {
	if backend == "supervisor" {
		_ = svc.StatusSupervisor()
		return
	}
	cfg, err := svc.Config(backend, self, "x", "x", nil)
	if err != nil {
		fmt.Printf("  %s: %v\n", svc.Name, err)
		return
	}
	s, err := service.New(svc.Runner{}, cfg)
	if err != nil {
		fmt.Printf("  %s: %v\n", svc.Name, err)
		return
	}
	st, err := s.Status()
	if err != nil {
		fmt.Printf("  %s: %v\n", svc.Name, err)
		return
	}
	fmt.Printf("  %s: %s\n", svc.Name, svc.StatusName(st))
}
