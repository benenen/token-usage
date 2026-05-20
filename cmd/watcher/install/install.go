// Package install wires the watcher binary into a native process supervisor
// (systemd / launchd / Windows SCM / supervisord).
//
// Exported entry points are NewInstallCmd, NewUninstallCmd, NewStatusCmd —
// each returns a cobra.Command ready to be attached to the watcher's root.
// Platform-specific bits (the supervisord backend) live in linux.go behind
// a //go:build linux tag and degrade to a clear error on other OSes.
package install

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"
)

// install / uninstall / status — wire the watcher into a native service.
// kardianos/service handles the cross-platform stuff (systemd / launchd /
// Windows SCM); platform-specific extras (supervisord) live in
// install_linux.go and install_other.go behind build tags.

const (
	serviceName        = "token-usage-watcher"
	serviceDisplayName = "Token usage watcher"
	serviceDescription = "Tail Claude Code / Codex transcripts and ship token usage."
)

type runner struct{} // service.Interface stub — kardianos requires one even for non-resident installers

func (runner) Start(service.Service) error { return nil }
func (runner) Stop(service.Service) error  { return nil }

// ----- install -------------------------------------------------------------

// NewInstallCmd returns the `install` cobra subcommand.
func NewInstallCmd() *cobra.Command {
	var (
		backend  string
		apiKey   string
		endpoint string
		extra    []string
	)
	cmd := &cobra.Command{
		Use:          "install",
		Short:        "Install the watcher as a service",
		Long:         installLongHelp(),
		Args:         cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doInstall(backend, apiKey, endpoint, extra)
		},
	}
	cmd.Flags().StringVar(&backend, "backend", "user", "user | system"+supervisorFlagHelp())
	cmd.Flags().StringVar(&apiKey, "api-key", os.Getenv("TOKENUSAGE_API_KEY"),
		"API key (tuk_…) — required")
	cmd.Flags().StringVar(&endpoint, "endpoint", os.Getenv("TOKENUSAGE_ENDPOINT"),
		"server ingest endpoint — required (e.g. http://server:8080/ingest)")
	cmd.Flags().StringSliceVar(&extra, "extra-args", nil,
		"extra args appended to the service ExecStart, repeatable")
	return cmd
}

// installLongHelp adapts to the current OS so each platform's --help
// only mentions the backends that actually work there.
func installLongHelp() string {
	base := "The binary self-copies into place (no separate download) and a 0600\nenv file holds the API key.\n\nBackends:\n"
	switch runtime.GOOS {
	case "linux":
		return base +
			"  --backend user        systemd-user service (default; no root)\n" +
			"  --backend system      systemd-system service (needs root)\n" +
			"  --backend supervisor  supervisord program (containers; needs root)\n"
	case "darwin":
		return base +
			"  --backend user        launchd LaunchAgent (default; no root)\n" +
			"  --backend system      launchd LaunchDaemon (needs root)\n"
	case "windows":
		return base +
			"  --backend system      Windows Service Manager (default + only; needs admin)\n" +
			"                        Note: Windows has no per-user service concept;\n" +
			"                              use system mode with admin privileges.\n"
	default:
		return base + "  (this OS isn't supported)\n"
	}
}

func supervisorFlagHelp() string {
	if supervisorAvailable() {
		return " | supervisor"
	}
	return ""
}

func doInstall(backend, apiKey, endpoint string, extra []string) error {
	if apiKey == "" {
		return errors.New("--api-key (or $TOKENUSAGE_API_KEY) is required")
	}
	if endpoint == "" {
		return errors.New("--endpoint (or $TOKENUSAGE_ENDPOINT) is required")
	}
	// Platform-specific gate: launchctl on macOS, admin token on Windows,
	// systemctl on Linux user mode, etc. Each xxx.go owns its own checks.
	if err := platformPreInstall(backend); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}

	if backend == "supervisor" {
		return installSupervisor(self, apiKey, endpoint, extra)
	}

	cfg, err := serviceConfig(backend, self, apiKey, endpoint, extra)
	if err != nil {
		return err
	}
	s, err := service.New(runner{}, cfg)
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
		serviceName, backend, statusName(st))
	if hint := platformInstallHint(backend); hint != "" {
		fmt.Println("  " + hint)
	}
	return nil
}

// serviceConfig builds a kardianos service.Config. EnvVars carry the
// secret (translated to systemd Environment= / launchd EnvironmentVariables
// / Windows registry env), so the API key never appears on a command line.
func serviceConfig(backend, self, apiKey, endpoint string, extra []string) (*service.Config, error) {
	args := []string{"run"}
	args = append(args, extra...)

	cfg := &service.Config{
		Name:        serviceName,
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		Executable:  self,
		Arguments:   args,
		EnvVars: map[string]string{
			"TOKENUSAGE_API_KEY":  apiKey,
			"TOKENUSAGE_ENDPOINT": endpoint,
		},
		Option: service.KeyValue{
			"Restart":    "on-failure",
			"RestartSec": 10,
			"LogOutput":  true, // systemd-only; ignored elsewhere
		},
	}

	switch backend {
	case "user":
		if runtime.GOOS == "windows" {
			return nil, errors.New("user backend isn't supported on Windows; use --backend system (needs admin)")
		}
		cfg.Option["UserService"] = true
	case "system":
		// machine-wide service — needs root/admin
	default:
		return nil, fmt.Errorf("invalid backend %q (use user | system%s)", backend, supervisorFlagHelp())
	}
	return cfg, nil
}

func statusName(s service.Status) string {
	switch s {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// ----- uninstall -----------------------------------------------------------

// NewUninstallCmd returns the `uninstall` cobra subcommand.
func NewUninstallCmd() *cobra.Command {
	var backend string
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the installed service",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doUninstall(backend)
		},
	}
	cmd.Flags().StringVar(&backend, "backend", "user", "user | system"+supervisorFlagHelp())
	return cmd
}

func doUninstall(backend string) error {
	if backend == "supervisor" {
		return uninstallSupervisor()
	}
	self, _ := os.Executable()
	cfg, err := serviceConfig(backend, self, "x", "x", nil) // env values irrelevant for uninstall
	if err != nil {
		return err
	}
	s, err := service.New(runner{}, cfg)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	_ = s.Stop()
	if err := s.Uninstall(); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	fmt.Printf("→ uninstalled %s (backend=%s)\n", serviceName, backend)
	return nil
}

// ----- status --------------------------------------------------------------

// NewStatusCmd returns the `status` cobra subcommand.
func NewStatusCmd() *cobra.Command {
	var backend string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show service status",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doStatus(backend)
		},
	}
	cmd.Flags().StringVar(&backend, "backend", "auto",
		"user | system"+supervisorFlagHelp()+" | auto")
	return cmd
}

func doStatus(backend string) error {
	if backend == "auto" {
		// Try every backend that's plausible on this OS.
		backends := []string{"user", "system"}
		if supervisorAvailable() {
			backends = append(backends, "supervisor")
		}
		for _, b := range backends {
			if b == "user" && runtime.GOOS == "windows" {
				continue // Windows has no user backend
			}
			fmt.Printf("─── backend=%s ───\n", b)
			_ = doStatus(b)
			fmt.Println()
		}
		return nil
	}
	if backend == "supervisor" {
		return statusSupervisor()
	}
	self, _ := os.Executable()
	cfg, err := serviceConfig(backend, self, "x", "x", nil)
	if err != nil {
		return err
	}
	s, err := service.New(runner{}, cfg)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	st, err := s.Status()
	if err != nil {
		fmt.Printf("  %s: %v\n", serviceName, err)
		return nil
	}
	fmt.Printf("  %s: %s\n", serviceName, statusName(st))
	return nil
}
