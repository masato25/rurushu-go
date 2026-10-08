//go:build windows

package jobs

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func configureDetached(cmd *exec.Cmd) {
	const detachedProcess = 0x00000008
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess}
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "cmd.exe", "/C", command)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH", "/FO", "CSV").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), fmt.Sprintf("\"%d\"", pid))
}

func processIdentity(pid int) (string, error) {
	if !processAlive(pid) {
		return "", fmt.Errorf("process not found")
	}
	filter := fmt.Sprintf("ProcessId = %d", pid)
	script := fmt.Sprintf(`$p = Get-CimInstance Win32_Process -Filter %q; if ($null -eq $p) { exit 1 }; ($p.CreationDate.ToUniversalTime().ToString('o') + '|' + $p.ExecutablePath + '|' + $p.CommandLine)`, filter)
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return "", err
	}
	identity := strings.TrimSpace(string(out))
	if identity == "" {
		return "", fmt.Errorf("empty process identity for pid %s", strconv.Itoa(pid))
	}
	return identity, nil
}

func stopProcessGroup(pid int, _ string, _ time.Duration) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid")
	}
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	if output, err := cmd.CombinedOutput(); err != nil && processAlive(pid) {
		return fmt.Errorf("taskkill: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
