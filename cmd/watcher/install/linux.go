//go:build linux

package install

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// supervisorAvailable reports whether the supervisord backend is buildable
// on this OS. Linux-only: there's no cross-platform supervisord equivalent.
func supervisorAvailable() bool { return true }

// Linux uses systemd. kardianos/service writes the unit:
//
//	user backend   -> ~/.config/systemd/user/token-usage-watcher.service
//	system backend -> /etc/systemd/system/token-usage-watcher.service  (needs root)
//
// supervisord is an alternative backend handled below.

func platformPreInstall(backend string) error {
	if backend == "system" && os.Geteuid() != 0 {
		return errors.New("--backend system writes to /etc/systemd/system/, which needs root; re-run with sudo")
	}
	if backend == "user" {
		// Mirrors the check installer.sh used to do — user-mode services
		// fail mysteriously without a user systemd session (containers,
		// non-graphical logins).
		if _, err := exec.LookPath("systemctl"); err != nil {
			return errors.New("systemctl not found in $PATH")
		}
	}
	return nil
}

func platformInstallHint(backend string) string {
	home, _ := os.UserHomeDir()
	switch backend {
	case "user":
		return "unit: " + home + "/.config/systemd/user/" + serviceName + ".service"
	case "system":
		return "unit: /etc/systemd/system/" + serviceName + ".service"
	case "supervisor":
		return "program: " + pickSupervisorConfDir() + "/" + serviceName + ".conf"
	}
	return ""
}

// installSupervisor writes a /etc/supervisor/conf.d/<svc>.conf file plus
// a 0600 env file under /etc, then runs `supervisorctl reread && update`.
// Drops the API key in the env file (not the program config, which is
// world-readable) so we don't leak the secret.
func installSupervisor(self, apiKey, endpoint string, extra []string) error {
	if os.Geteuid() != 0 {
		return errors.New("--backend supervisor needs root; re-run with sudo")
	}
	if _, err := exec.LookPath("supervisorctl"); err != nil {
		return errors.New("supervisorctl not found in $PATH (apt-get install supervisor)")
	}

	confDir := pickSupervisorConfDir()
	binDir := "/usr/local/bin"
	envDir := "/etc/token-usage-watcher"
	binPath := filepath.Join(binDir, serviceName)
	envPath := filepath.Join(envDir, "env")
	confPath := filepath.Join(confDir, serviceName+".conf")

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	if err := copySelf(self, binPath); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}

	if err := os.MkdirAll(envDir, 0o700); err != nil {
		return err
	}
	envContent := "TOKENUSAGE_API_KEY=" + apiKey + "\nTOKENUSAGE_ENDPOINT=" + endpoint + "\n"
	if err := os.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		return err
	}

	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}
	execLine := binPath + " run"
	if len(extra) > 0 {
		execLine += " " + strings.Join(extra, " ")
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
	confDir := pickSupervisorConfDir()
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
	out, err := exec.Command("supervisorctl", "status", serviceName).CombinedOutput()
	fmt.Printf("  %s", string(out))
	_ = err
	return nil
}

func pickSupervisorConfDir() string {
	if _, err := os.Stat("/etc/supervisord.d"); err == nil {
		return "/etc/supervisord.d"
	}
	return "/etc/supervisor/conf.d"
}

// runCmd / copySelf live here so the !linux file can omit them without
// running into "declared but not used" issues. They're trivial and only
// invoked by supervisorAvailable=true platforms anyway.
func runCmd(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func copySelf(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
