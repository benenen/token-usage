//go:build windows

package install

import (
	"errors"
	"fmt"
	"runtime"

	"golang.org/x/sys/windows"
)

// Windows uses the Service Control Manager. kardianos/service writes the
// service registration to HKLM\SYSTEM\CurrentControlSet\Services\<name>
// and starts it via the SCM API. There is no per-user service concept in
// Windows (user backend bails out in install.go's serviceConfig).
//
// Admin is required to write to SCM, so we probe the process token up
// front and surface a clean error instead of a cryptic SCM access-denied
// later.

func supervisorAvailable() bool { return false }

func platformPreInstall(backend string) error {
	if backend != "system" {
		return nil // serviceConfig already rejects --backend user on Windows
	}
	if !isElevated() {
		return errors.New("--backend system writes to the Service Control Manager and needs an elevated (\"Run as administrator\") shell")
	}
	return nil
}

func platformInstallHint(backend string) string {
	if backend == "system" {
		return "registered in the SCM; manage via `sc.exe query " + serviceName + "` or services.msc"
	}
	return ""
}

// isElevated checks whether the current process is running with the
// BUILTIN\Administrators group active. The classic admin-detection idiom
// on Windows: build the BUILTIN\Administrators SID, then ask whether
// it's a member of the current token.
func isElevated() bool {
	var sid *windows.SID
	err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY,
		2,
		windows.SECURITY_BUILTIN_DOMAIN_RID,
		windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0,
		&sid,
	)
	if err != nil {
		return false
	}
	defer windows.FreeSid(sid)
	token := windows.Token(0) // 0 = current process token
	member, err := token.IsMember(sid)
	return err == nil && member
}

// supervisord stubs — never available on Windows.
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
