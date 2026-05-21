// Package svc wires the watcher binary into a native process supervisor
// (systemd / launchd / Windows SCM / supervisord). It owns the shared
// kardianos service.Config builder, the platform-specific install /
// uninstall / restart / status entrypoints (declared per OS via build
// tags in linux.go / darwin.go / windows.go), and the supervisord
// backend (Linux-only).
//
// Consumed by the cobra commands in the parent `cli` package
// (install.go, uninstall.go, restart.go, status.go).
package svc

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/kardianos/service"
)

// Identifiers used for the systemd unit / launchd plist / SCM service.
const (
	Name        = "token-usage-watcher"
	DisplayName = "Token usage watcher"
	Description = "Tail Claude Code / Codex transcripts and ship token usage."
)

// Runner is a service.Interface stub — kardianos requires one even for
// non-resident installers (Start/Stop are never invoked by our CLI; the
// real watcher loop runs from `token-usage-watcher run`, not from here).
type Runner struct{}

func (Runner) Start(service.Service) error { return nil }
func (Runner) Stop(service.Service) error  { return nil }

// Config builds a kardianos service.Config. EnvVars carry the secret
// (translated to systemd Environment= / launchd EnvironmentVariables /
// Windows registry env), so the API key never appears on a command line.
func Config(backend, self, apiKey, endpoint string, extra []string) (*service.Config, error) {
	args := []string{"run"}
	args = append(args, extra...)

	cfg := &service.Config{
		Name:        Name,
		DisplayName: DisplayName,
		Description: Description,
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
		return nil, fmt.Errorf("invalid backend %q (use user | system%s)", backend, SupervisorFlagHelp())
	}
	return cfg, nil
}

// StatusName turns kardianos's enum into a CLI-friendly word.
func StatusName(s service.Status) string {
	switch s {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// SupervisorFlagHelp returns " | supervisor" on Linux (where the
// supervisord backend is available) and "" elsewhere, so the per-OS
// flag-help text doesn't promise something the binary can't deliver.
func SupervisorFlagHelp() string {
	if SupervisorAvailable() {
		return " | supervisor"
	}
	return ""
}

// IsInstalled reports whether the given backend currently has a unit
// (or supervisord program) on disk for our service. Used by uninstall
// and restart to walk DefaultBackends() and act on whichever entries
// are actually present, instead of asking the user to pick one.
func IsInstalled(backend, self string) bool {
	if backend == "supervisor" {
		return SupervisorInstalled()
	}
	cfg, err := Config(backend, self, "x", "x", nil)
	if err != nil {
		return false
	}
	s, err := service.New(Runner{}, cfg)
	if err != nil {
		return false
	}
	_, err = s.Status()
	// kardianos returns service.ErrNotInstalled (or platform-specific
	// equivalents like "service not found") when no unit/plist/SCM entry
	// exists — treat any error as "not installed" rather than guessing
	// at the precise sentinel per OS.
	return err == nil
}
