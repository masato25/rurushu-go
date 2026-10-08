//go:build !windows

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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-lc", command)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func processIdentity(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=", "-o", "command=").Output()
	if err != nil {
		return "", err
	}
	identity := strings.TrimSpace(string(out))
	if identity == "" {
		return "", fmt.Errorf("empty process identity")
	}
	return identity, nil
}

func stopProcessGroup(pid int, expectedIdentity string, grace time.Duration) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid")
	}
	stillRecordedProcess := func() bool {
		identity, err := processIdentity(pid)
		return err == nil && identity == expectedIdentity
	}
	target := pid
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid > 0 && pgid != syscall.Getpgrp() {
		target = -pgid
	}
	if err := syscall.Kill(target, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		if !stillRecordedProcess() {
			return nil
		}
		return fmt.Errorf("send SIGTERM: %w", err)
	}
	deadline := time.Now().Add(grace)
	for stillRecordedProcess() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if stillRecordedProcess() {
		if err := syscall.Kill(target, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			if !stillRecordedProcess() {
				return nil
			}
			return fmt.Errorf("send SIGKILL: %w", err)
		}
	}
	return nil
}
