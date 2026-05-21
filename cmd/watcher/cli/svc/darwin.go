//go:build darwin

package svc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// macOS uses launchd. kardianos/service writes the plist:
//
//	user backend   -> ~/Library/LaunchAgents/token-usage-watcher.plist
//	system backend -> /Library/LaunchDaemons/token-usage-watcher.plist  (needs root)
//
// `launchctl` is part of macOS — checked at install time for early failure.

// SupervisorAvailable is false on macOS (no supervisord port).
func SupervisorAvailable() bool { return false }

// DefaultBackends is the ordered preference list of backends to try on
// the current process. macOS has no supervisord port; only the two
// launchd modes are candidates.
//
// macOS root     -> [system, user]   (machine-wide first)
// macOS non-root -> [user]
func DefaultBackends() []string {
	if os.Geteuid() == 0 {
		return []string{"system", "user"}
	}
	return []string{"user"}
}

// SupervisorInstalled is always false on macOS — never available here.
func SupervisorInstalled() bool { return false }

// ShowLogs uses macOS's unified logging (`log show` / `log stream`)
// filtered to our process name. The kardianos plist doesn't configure
// StandardOutPath/StandardErrorPath, so stdout/stderr never hit disk
// directly — the unified log is the only built-in source.
func ShowLogs(backend string, follow bool) error {
	pred := fmt.Sprintf("process == %q", Name)
	if follow {
		return runStdio("log", "stream", "--predicate", pred)
	}
	return runStdio("log", "show", "--predicate", pred, "--last", "1h", "--style", "compact")
}

// PlatformPreInstall runs before kardianos service.Install(). Bails out
// early with a clear message when the host environment can't host the
// chosen backend.
func PlatformPreInstall(backend string) error {
	if _, err := exec.LookPath("launchctl"); err != nil {
		return errors.New("launchctl not found in $PATH; install Xcode Command Line Tools")
	}
	if backend == "system" && os.Geteuid() != 0 {
		return errors.New("--backend system writes /Library/LaunchDaemons/, which needs root; re-run with sudo")
	}
	return nil
}

// PlatformInstallHint returns a one-liner shown after a successful install,
// pointing the user at the plist that was written.
func PlatformInstallHint(backend string) string {
	home, _ := os.UserHomeDir()
	switch backend {
	case "user":
		return "plist: " + filepath.Join(home, "Library", "LaunchAgents", Name+".plist")
	case "system":
		return "plist: /Library/LaunchDaemons/" + Name + ".plist"
	}
	return ""
}

// supervisord stubs — Linux-only feature; surface a clear error here.
func InstallSupervisor(_, _, _ string, _ []string) error {
	return fmt.Errorf("--backend supervisor is only available on Linux (current OS: %s)", runtime.GOOS)
}
func UninstallSupervisor() error {
	return fmt.Errorf("--backend supervisor is only available on Linux (current OS: %s)", runtime.GOOS)
}
func StatusSupervisor() error {
	fmt.Printf("  %s: (--backend supervisor not available on %s)\n", Name, runtime.GOOS)
	return nil
}
func StopSupervisor() error {
	return fmt.Errorf("--backend supervisor is only available on Linux (current OS: %s)", runtime.GOOS)
}
func StartSupervisor() error {
	return fmt.Errorf("--backend supervisor is only available on Linux (current OS: %s)", runtime.GOOS)
}
func RestartSupervisor() error {
	return fmt.Errorf("--backend supervisor is only available on Linux (current OS: %s)", runtime.GOOS)
}
