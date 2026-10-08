//go:build !windows

package jobs

import (
	"os/exec"
	"testing"
	"time"
)

func TestStopProcessGroupTreatsExitedRunnerAsStopped(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	configureDetached(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()

	identity, err := processIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("process identity: %v", err)
	}
	if err := stopProcessGroup(cmd.Process.Pid, identity, 500*time.Millisecond); err != nil {
		t.Fatalf("stop process group: %v", err)
	}

	if current, err := processIdentity(cmd.Process.Pid); err == nil && current == identity {
		t.Fatalf("recorded process is still running after stop")
	}
}
