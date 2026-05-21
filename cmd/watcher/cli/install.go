package cli

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"tokenusage/cmd/watcher/cli/svc"
)

// NewInstallCmd returns the `install` cobra subcommand.
func NewInstallCmd() *cobra.Command {
	var (
		apiKey   string
		endpoint string
		extra    []string
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the watcher as a service",
		Long:  installLongHelp(),
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doInstall(apiKey, endpoint, extra)
		},
	}
	cmd.Flags().StringVar(&apiKey, "api-key", os.Getenv("TOKENUSAGE_API_KEY"),
		"API key (tuk_…) — required")
	cmd.Flags().StringVar(&endpoint, "endpoint", os.Getenv("TOKENUSAGE_ENDPOINT"),
		"server ingest endpoint — required (e.g. http://server:8080/ingest)")
	cmd.Flags().StringSliceVar(&extra, "extra-args", nil,
		"extra args appended to the service ExecStart, repeatable")
	return cmd
}

// installLongHelp describes the per-OS backend preference order so
// users know what auto-selection will pick (and what root vs non-root
// changes). There is no --backend flag — the binary tries each
// candidate in order and stops at the first that works.
func installLongHelp() string {
	base := "The binary self-copies into place (no separate download) and a 0600\nenv file holds the API key.\n\nBackend is auto-selected from the per-OS preference list — the first\ncandidate whose pre-flight check passes gets installed.\n\n"
	switch runtime.GOOS {
	case "linux":
		return base +
			"Linux preference order:\n" +
			"  root      systemd-system → supervisord → systemd-user\n" +
			"  non-root  systemd-user\n"
	case "darwin":
		return base +
			"macOS preference order:\n" +
			"  root      launchd LaunchDaemon → launchd LaunchAgent\n" +
			"  non-root  launchd LaunchAgent\n"
	case "windows":
		return base +
			"Windows: Service Control Manager (requires an elevated shell).\n" +
			"Note: Windows has no per-user service concept.\n"
	default:
		return base + "(this OS isn't supported)\n"
	}
}

func doInstall(apiKey, endpoint string, extra []string) error {
	if apiKey == "" {
		return errors.New("--api-key (or $TOKENUSAGE_API_KEY) is required")
	}
	if endpoint == "" {
		return errors.New("--endpoint (or $TOKENUSAGE_ENDPOINT) is required")
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}

	candidates := svc.DefaultBackends()
	var skipped []string
	for _, backend := range candidates {
		fmt.Printf("→ trying backend=%s\n", backend)
		if err := svc.PlatformPreInstall(backend); err != nil {
			fmt.Printf("  not viable: %v\n", err)
			skipped = append(skipped, fmt.Sprintf("%s (%v)", backend, err))
			continue
		}
		if err := installOne(backend, self, apiKey, endpoint, extra); err != nil {
			fmt.Printf("  install failed: %v\n", err)
			skipped = append(skipped, fmt.Sprintf("%s (%v)", backend, err))
			continue
		}
		return nil
	}
	return fmt.Errorf("no backend usable on %s — tried: %s",
		runtime.GOOS, strings.Join(skipped, "; "))
}

// installOne runs the install for a single backend. Returns nil and
// prints the success line on success, or the relevant error otherwise.
func installOne(backend, self, apiKey, endpoint string, extra []string) error {
	if backend == "supervisor" {
		return svc.InstallSupervisor(self, apiKey, endpoint, extra)
	}
	cfg, err := svc.Config(backend, self, apiKey, endpoint, extra)
	if err != nil {
		return err
	}
	s, err := service.New(svc.Runner{}, cfg)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	// Uninstall first so re-running with new flags doesn't leave a stale unit.
	_ = s.Uninstall()
	if err := s.Install(); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	st, _ := s.Status()
	fmt.Printf("→ installed and started %s (backend=%s, status=%s)\n",
		svc.Name, backend, svc.StatusName(st))
	if hint := svc.PlatformInstallHint(backend); hint != "" {
		fmt.Println("  " + hint)
	}
	return nil
}
