//go:build darwin

package install

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

// supervisorAvailable is false on macOS (no supervisord port).
func supervisorAvailable() bool { return false }

// platformPreInstall runs before kardianos service.Install(). Bails out
// early with a clear message when the host environment can't host the
// chosen backend.
func platformPreInstall(backend string) error {
	if _, err := exec.LookPath("launchctl"); err != nil {
		return errors.New("launchctl not found in $PATH; install Xcode Command Line Tools")
	}
	if backend == "system" && os.Geteuid() != 0 {
		return errors.New("--backend system writes /Library/LaunchDaemons/, which needs root; re-run with sudo")
	}
	return nil
}

// platformInstallHint returns a one-liner shown after a successful install,
// pointing the user at the plist that was written.
func platformInstallHint(backend string) string {
	home, _ := os.UserHomeDir()
	switch backend {
	case "user":
		return "plist: " + filepath.Join(home, "Library", "LaunchAgents", serviceName+".plist")
	case "system":
		return "plist: /Library/LaunchDaemons/" + serviceName + ".plist"
	}
	return ""
}

// supervisord stubs — Linux-only feature; surface a clear error here.
func installSupervisor(_, _, _ string, _ []string) error {
	return fmt.Errorf("--backend supervisor is only available on Linux (current OS: %s)", runtime.GOOS)
}
func uninstallSupervisor() error {
	return fmt.Errorf("--backend supervisor is only available on Linux (current OS: %s)", runtime.GOOS)
}
func statusSupervisor() error {
	fmt.Printf("  %s: (--backend supervisor not available on %s)\n", serviceName, runtime.GOOS)
	return nil
}
