package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"
)

// install / uninstall / status — wire the watcher into a native service.
//
// Backends:
//
//	user        per-user service        (Linux systemd-user / macOS launchd-user)
//	system      machine-wide service    (Linux systemd / macOS launchd-daemon / Windows SCM)
//	supervisor  supervisord program     (Linux only; useful in containers)
//
// kardianos/service handles systemd / launchd / Windows; supervisord goes
// through our own writer because no cross-platform lib covers it.

const (
	serviceName        = "token-usage-watcher"
	serviceDisplayName = "Token usage watcher"
	serviceDescription = "Tail Claude Code / Codex transcripts and ship token usage."
	envEnvVar          = "TOKENUSAGE_ENV_FILE" // where to find env at runtime
)

type runner struct{} // service.Interface stub — never runs in-process

func (runner) Start(service.Service) error { return nil }
func (runner) Stop(service.Service) error  { return nil }

// ----- install -------------------------------------------------------------
func newInstallCmd(_, _ string) *cobra.Command {
	var (
		backend  string
		apiKey   string
		endpoint string
		extra    []string
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the watcher as a service",
		Long: "Pick a backend:\n" +
			"  --backend user        per-user service (systemd-user / launchd-user)\n" +
			"  --backend system      machine-wide service (needs root / admin)\n" +
			"  --backend supervisor  supervisord program (Linux containers; needs root)\n" +
			"\n" +
			"The binary self-copies into place (no separate download) and a 0600 env\n" +
			"file holds the API key.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doInstall(backend, apiKey, endpoint, extra)
		},
	}
	cmd.Flags().StringVar(&backend, "backend", "user", "user | system | supervisor")
	cmd.Flags().StringVar(&apiKey, "api-key", os.Getenv("TOKENUSAGE_API_KEY"),
		"API key (tuk_…) — required")
	cmd.Flags().StringVar(&endpoint, "endpoint", os.Getenv("TOKENUSAGE_ENDPOINT"),
		"server ingest endpoint — required (e.g. http://server:8080/ingest)")
	cmd.Flags().StringSliceVar(&extra, "extra-args", nil,
		"extra args appended to the service ExecStart, repeatable")
	return cmd
}

func doInstall(backend, apiKey, endpoint string, extra []string) error {
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
	// Uninstall first to make install idempotent: re-running with new
	// flags doesn't leave a stale unit behind.
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
	return nil
}

// serviceConfig builds a kardianos service.Config. We pass the api-key /
// endpoint via Environment (kardianos translates to systemd Environment=,
// launchd EnvironmentVariables, Windows registry env) so the secrets
// never appear on the command line.
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
			// Restart on crash, with a small back-off.
			"Restart":          "on-failure",
			"RestartSec":       10,
			// systemd-only: log to journal.
			"LogOutput":        true,
		},
	}

	switch backend {
	case "user":
		// kardianos uses UserService=true for systemd-user / launchd-user.
		// On Windows there is no concept of "user service" in SCM, so user
		// mode just isn't supported there; bail with a clear message.
		if runtime.GOOS == "windows" {
			return nil, errors.New("user backend isn't supported on Windows; use --backend system (needs admin)")
		}
		cfg.Option["UserService"] = true
	case "system":
		// default — machine-wide service. Needs root/admin to install.
	default:
		return nil, fmt.Errorf("invalid backend %q (use user | system | supervisor)", backend)
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
func newUninstallCmd(_ string) *cobra.Command {
	var backend string
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the installed service",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doUninstall(backend)
		},
	}
	cmd.Flags().StringVar(&backend, "backend", "user", "user | system | supervisor")
	return cmd
}

func doUninstall(backend string) error {
	if backend == "supervisor" {
		return uninstallSupervisor()
	}
	self, _ := os.Executable()
	cfg, err := serviceConfig(backend, self, "x", "x", nil) // env values irrelevant
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
func newStatusCmd(_ string) *cobra.Command {
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
		"user | system | supervisor | auto")
	return cmd
}

func doStatus(backend string) error {
	if backend == "auto" {
		// Try each backend that's plausible on this OS, report whatever
		// responds. supervisord only on Linux.
		for _, b := range []string{"user", "system", "supervisor"} {
			if b == "supervisor" && runtime.GOOS != "linux" {
				continue
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

// ----- supervisor (Linux only, manual config write) ------------------------
func installSupervisor(self, apiKey, endpoint string, extra []string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("--backend supervisor is Linux-only (current: %s)", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		return errors.New("--backend supervisor needs root; re-run with sudo")
	}
	if _, err := exec.LookPath("supervisorctl"); err != nil {
		return errors.New("supervisorctl not found in $PATH (apt-get install supervisor)")
	}

	confDir := "/etc/supervisor/conf.d"
	if _, err := os.Stat("/etc/supervisord.d"); err == nil {
		confDir = "/etc/supervisord.d"
	}
	binDir := "/usr/local/bin"
	envDir := "/etc/token-usage-watcher"
	binPath := filepath.Join(binDir, serviceName)
	envPath := filepath.Join(envDir, "env")
	confPath := filepath.Join(confDir, serviceName+".conf")

	// 1. copy self to /usr/local/bin
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	if err := copySelf(self, binPath); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}

	// 2. env file (0600)
	if err := os.MkdirAll(envDir, 0o700); err != nil {
		return err
	}
	envContent := "TOKENUSAGE_API_KEY=" + apiKey + "\nTOKENUSAGE_ENDPOINT=" + endpoint + "\n"
	if err := os.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		return err
	}

	// 3. supervisord program file
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}
	execLine := binPath + " run"
	for _, a := range extra {
		execLine += " " + a
	}
	conf := "[program:" + serviceName + "]\n" +
		"command=/bin/bash -c 'set -a; . " + envPath + "; set +a; exec " + execLine + "'\n" +
		"autostart=true\nautorestart=true\nstartsecs=1\nstartretries=10\n" +
		"stopsignal=TERM\nstopwaitsecs=10\nuser=root\n" +
		"stdout_logfile=/var/log/" + serviceName + ".out.log\n" +
		"stderr_logfile=/var/log/" + serviceName + ".err.log\n" +
		"stdout_logfile_maxbytes=10MB\nstderr_logfile_maxbytes=10MB\n" +
		"stdout_logfile_backups=3\nstderr_logfile_backups=3\n"
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		return err
	}

	// 4. reload + start
	if err := runCmd("supervisorctl", "reread"); err != nil {
		return err
	}
	if err := runCmd("supervisorctl", "update"); err != nil {
		return err
	}
	fmt.Printf("→ installed supervisor program %s\n", confPath)
	return statusSupervisor()
}

func uninstallSupervisor() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("--backend supervisor is Linux-only")
	}
	confDir := "/etc/supervisor/conf.d"
	if _, err := os.Stat("/etc/supervisord.d"); err == nil {
		confDir = "/etc/supervisord.d"
	}
	confPath := filepath.Join(confDir, serviceName+".conf")
	_ = exec.Command("supervisorctl", "stop", serviceName).Run()
	_ = exec.Command("supervisorctl", "remove", serviceName).Run()
	_ = os.Remove(confPath)
	_ = exec.Command("supervisorctl", "reread").Run()
	_ = exec.Command("supervisorctl", "update").Run()
	_ = os.Remove("/usr/local/bin/" + serviceName)
	_ = os.RemoveAll("/etc/token-usage-watcher")
	fmt.Printf("→ uninstalled supervisor program %s\n", confPath)
	return nil
}

func statusSupervisor() error {
	if runtime.GOOS != "linux" {
		fmt.Printf("  %s: (supervisor not available on %s)\n", serviceName, runtime.GOOS)
		return nil
	}
	out, err := exec.Command("supervisorctl", "status", serviceName).CombinedOutput()
	fmt.Printf("  %s", string(out))
	_ = err
	return nil
}

// ----- helpers -------------------------------------------------------------
func runCmd(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func copySelf(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, in, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
