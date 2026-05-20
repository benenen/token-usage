package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// install / uninstall / status — wire the watcher into a process supervisor.
//
// Backends:
//   user        systemd user service           ($HOME/.config/systemd/user/)
//   system      systemd system service         (/etc/systemd/system/)        — needs root
//   supervisor  supervisord program            (/etc/supervisor/conf.d/)     — needs root
//
// install copies the running binary into place (no separate download), writes
// a 0600 env file with TOKENUSAGE_API_KEY / TOKENUSAGE_ENDPOINT, drops the
// service unit, and asks the supervisor to pick it up.

const (
	serviceName = "token-usage-watcher"
)

type installPaths struct {
	binDir       string
	binPath      string
	envDir       string
	envPath      string
	unitDir      string
	unitPath     string
	supConfDir   string
	supConfPath  string
	needsSudo    bool
	wantedBy     string // systemd target
}

func pathsFor(backend, home string) installPaths {
	p := installPaths{}
	switch backend {
	case "user":
		p.binDir = filepath.Join(home, ".local", "bin")
		p.envDir = filepath.Join(home, ".config", "token-usage-watcher")
		p.unitDir = filepath.Join(home, ".config", "systemd", "user")
		p.wantedBy = "default.target"
	default: // system | supervisor
		p.binDir = "/usr/local/bin"
		p.envDir = "/etc/token-usage-watcher"
		p.unitDir = "/etc/systemd/system"
		p.supConfDir = "/etc/supervisor/conf.d"
		if _, err := os.Stat("/etc/supervisord.d"); err == nil && backend == "supervisor" {
			p.supConfDir = "/etc/supervisord.d"
		}
		p.needsSudo = os.Geteuid() != 0
		p.wantedBy = "multi-user.target"
	}
	p.binPath = filepath.Join(p.binDir, serviceName)
	p.envPath = filepath.Join(p.envDir, "env")
	p.unitPath = filepath.Join(p.unitDir, serviceName+".service")
	p.supConfPath = filepath.Join(p.supConfDir, serviceName+".conf")
	return p
}

// ----- install -------------------------------------------------------------
func newInstallCmd(home, _ string) *cobra.Command {
	var (
		backend  string
		apiKey   string
		endpoint string
		extra    string
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the watcher as a service",
		Long: "Copies the running binary into place, writes an env file and a service\n" +
			"unit, and tells the supervisor to start it. Pick a backend:\n" +
			"  --backend user       systemd user service (default; no root)\n" +
			"  --backend system     systemd system service (needs root)\n" +
			"  --backend supervisor supervisord program (containers; needs root)",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doInstall(backend, apiKey, endpoint, extra, home)
		},
	}
	cmd.Flags().StringVar(&backend, "backend", "user", "user | system | supervisor")
	cmd.Flags().StringVar(&apiKey, "api-key", os.Getenv("TOKENUSAGE_API_KEY"),
		"API key (tuk_…) — required")
	cmd.Flags().StringVar(&endpoint, "endpoint",
		os.Getenv("TOKENUSAGE_ENDPOINT"),
		"server ingest endpoint — required (e.g. http://server:8080/ingest)")
	cmd.Flags().StringVar(&extra, "extra-args", "",
		"extra flags appended to ExecStart / command (quoted)")
	return cmd
}

func doInstall(backend, apiKey, endpoint, extra, home string) error {
	if err := validateBackend(backend); err != nil {
		return err
	}
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
	p := pathsFor(backend, home)
	if p.needsSudo {
		return fmt.Errorf("backend %q needs root; re-run with sudo", backend)
	}

	// 1. binary
	fmt.Printf("→ installing binary to %s\n", p.binPath)
	if err := mkdirAll(p.binDir, 0o755); err != nil {
		return err
	}
	if err := copyFile(self, p.binPath, 0o755); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}

	// 2. env file
	fmt.Printf("→ writing env file %s (mode 0600)\n", p.envPath)
	if err := mkdirAll(p.envDir, 0o700); err != nil {
		return err
	}
	envContent := "# Managed by `token-usage-watcher install`. Restart the service after edits.\n" +
		"TOKENUSAGE_API_KEY=" + apiKey + "\n" +
		"TOKENUSAGE_ENDPOINT=" + endpoint + "\n"
	if err := os.WriteFile(p.envPath, []byte(envContent), 0o600); err != nil {
		return err
	}

	// 3. service unit / supervisor conf
	execLine := p.binPath
	if extra != "" {
		execLine = p.binPath + " " + extra
	}
	if backend == "supervisor" {
		fmt.Printf("→ writing supervisord program %s\n", p.supConfPath)
		if err := mkdirAll(p.supConfDir, 0o755); err != nil {
			return err
		}
		conf := "[program:" + serviceName + "]\n" +
			"command=/bin/bash -c 'set -a; . " + p.envPath + "; set +a; exec " + execLine + "'\n" +
			"autostart=true\n" +
			"autorestart=true\n" +
			"startsecs=1\n" +
			"startretries=10\n" +
			"stopsignal=TERM\n" +
			"stopwaitsecs=10\n" +
			"user=root\n" +
			"stdout_logfile=/var/log/" + serviceName + ".out.log\n" +
			"stderr_logfile=/var/log/" + serviceName + ".err.log\n" +
			"stdout_logfile_maxbytes=10MB\n" +
			"stderr_logfile_maxbytes=10MB\n" +
			"stdout_logfile_backups=3\n" +
			"stderr_logfile_backups=3\n"
		if err := os.WriteFile(p.supConfPath, []byte(conf), 0o644); err != nil {
			return err
		}
		fmt.Println("→ supervisorctl reread && update")
		if err := runCmd("supervisorctl", "reread"); err != nil {
			return err
		}
		if err := runCmd("supervisorctl", "update"); err != nil {
			return err
		}
		return showStatus(backend)
	}

	// systemd path (user or system)
	fmt.Printf("→ writing systemd unit %s\n", p.unitPath)
	if err := mkdirAll(p.unitDir, 0o755); err != nil {
		return err
	}
	unit := "[Unit]\n" +
		"Description=Token usage watcher (Claude Code / Codex)\n" +
		"After=network-online.target\n" +
		"Wants=network-online.target\n" +
		"\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"EnvironmentFile=" + p.envPath + "\n" +
		"ExecStart=" + execLine + "\n" +
		"Restart=on-failure\n" +
		"RestartSec=10s\n" +
		"NoNewPrivileges=true\n" +
		"ProtectSystem=full\n" +
		"PrivateTmp=true\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=" + p.wantedBy + "\n"
	if err := os.WriteFile(p.unitPath, []byte(unit), 0o644); err != nil {
		return err
	}

	systemctl := []string{"systemctl"}
	if backend == "user" {
		systemctl = append(systemctl, "--user")
	}
	fmt.Println("→ systemctl daemon-reload && enable --now")
	if err := runCmd(systemctl[0], append(systemctl[1:], "daemon-reload")...); err != nil {
		return err
	}
	if err := runCmd(systemctl[0], append(systemctl[1:], "enable", "--now", serviceName+".service")...); err != nil {
		return err
	}
	return showStatus(backend)
}

// ----- uninstall -----------------------------------------------------------
func newUninstallCmd(home string) *cobra.Command {
	var backend string
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the installed service",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return doUninstall(backend, home)
		},
	}
	cmd.Flags().StringVar(&backend, "backend", "user", "user | system | supervisor")
	return cmd
}

func doUninstall(backend, home string) error {
	if err := validateBackend(backend); err != nil {
		return err
	}
	p := pathsFor(backend, home)
	if p.needsSudo {
		return fmt.Errorf("backend %q needs root; re-run with sudo", backend)
	}

	fmt.Printf("→ stopping %s (backend=%s)\n", serviceName, backend)
	switch backend {
	case "user":
		_ = runQuiet("systemctl", "--user", "stop", serviceName+".service")
		_ = runQuiet("systemctl", "--user", "disable", serviceName+".service")
		_ = runQuiet("systemctl", "--user", "daemon-reload")
	case "system":
		_ = runQuiet("systemctl", "stop", serviceName+".service")
		_ = runQuiet("systemctl", "disable", serviceName+".service")
		_ = runQuiet("systemctl", "daemon-reload")
	case "supervisor":
		_ = runQuiet("supervisorctl", "stop", serviceName)
		_ = runQuiet("supervisorctl", "remove", serviceName)
		_ = os.Remove(p.supConfPath)
		_ = runQuiet("supervisorctl", "reread")
		_ = runQuiet("supervisorctl", "update")
	}
	if backend == "user" || backend == "system" {
		_ = os.Remove(p.unitPath)
	}
	_ = os.Remove(p.binPath)
	_ = os.RemoveAll(p.envDir)
	fmt.Println("→ uninstalled. (Checkpoint at $HOME/.token-usage-watcher/ left untouched.)")
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
			return showStatus(backend)
		},
	}
	cmd.Flags().StringVar(&backend, "backend", "auto",
		"user | system | supervisor | auto (try all three and report)")
	return cmd
}

func showStatus(backend string) error {
	if backend == "auto" {
		// Try all three; print whatever responds.
		printHeader := func(name string) { fmt.Println("─── " + name + " ───") }
		printHeader("systemd --user")
		_ = runRelay("systemctl", "--user", "status", serviceName+".service", "--no-pager", "--lines=10")
		fmt.Println()
		printHeader("systemd (system)")
		_ = runRelay("systemctl", "status", serviceName+".service", "--no-pager", "--lines=10")
		fmt.Println()
		printHeader("supervisord")
		_ = runRelay("supervisorctl", "status", serviceName)
		return nil
	}
	switch backend {
	case "user":
		return runRelay("systemctl", "--user", "status", serviceName+".service", "--no-pager", "--lines=20")
	case "system":
		return runRelay("systemctl", "status", serviceName+".service", "--no-pager", "--lines=20")
	case "supervisor":
		return runRelay("supervisorctl", "status", serviceName)
	}
	return validateBackend(backend)
}

// ----- helpers -------------------------------------------------------------
func validateBackend(b string) error {
	switch b {
	case "user", "system", "supervisor":
		return nil
	}
	return fmt.Errorf("invalid backend %q (want: user | system | supervisor)", b)
}

func runCmd(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// runRelay is like runCmd but does NOT propagate exit code as a Go error —
// status output (even of an exited service) is the user's signal.
func runRelay(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	_ = c.Run()
	return nil
}

func runQuiet(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

func mkdirAll(p string, mode os.FileMode) error {
	if err := os.MkdirAll(p, mode); err != nil {
		return fmt.Errorf("mkdir %s: %w", p, err)
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Use a temp file in the same dir + rename so an interrupted copy
	// doesn't leave a half-written binary at dst.
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

