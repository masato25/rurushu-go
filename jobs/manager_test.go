package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagerPersistsAndListsJobs(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".rurushu")
	if err := os.MkdirAll(filepath.Join(stateDir, "jobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := Open(root, stateDir, os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	job := Job{ID: 1, Command: "echo hi", CWD: root, Status: StatusExited, StartedAt: nowForTest(), LogPath: "jobs/job_000001.log"}
	if err := m.writeLocked(job); err != nil {
		m.mu.Unlock()
		t.Fatal(err)
	}
	m.mu.Unlock()
	jobs, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Command != "echo hi" {
		t.Fatalf("jobs=%+v", jobs)
	}
}

func TestTailIsBoundedAndReturnsRecentOutput(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".rurushu")
	if err := os.MkdirAll(filepath.Join(stateDir, "jobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := Open(root, stateDir, os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	job := Job{ID: 1, Command: "server", CWD: root, Status: StatusExited, StartedAt: nowForTest(), LogPath: "jobs/job_000001.log"}
	if err := m.writeLocked(job); err != nil {
		m.mu.Unlock()
		t.Fatal(err)
	}
	m.mu.Unlock()
	logPath := filepath.Join(stateDir, "jobs", "job_000001.log")
	if err := os.WriteFile(logPath, []byte(strings.Repeat("old line\n", 100)+"latest marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := m.Tail(1, 128)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "latest marker") || len(out) > 128 {
		t.Fatalf("tail=%q", out)
	}
}

func TestRollingLogStaysBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	log, err := openRollingLog(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := []byte(strings.Repeat("x", 128*1024))
	for i := 0; i < 24; i++ {
		if _, err := log.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxJobLogBytes {
		t.Fatalf("log grew to %d bytes", info.Size())
	}
}

func TestListMarksMismatchedProcessIdentityStale(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".rurushu")
	m, err := Open(root, stateDir, os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	job := Job{
		ID: 1, Command: "do-not-kill", CWD: root, Status: StatusRunning,
		RunnerPID: os.Getpid(), RunnerIdentity: "definitely-not-this-process",
		StartedAt: time.Now().Add(-time.Minute), LogPath: "../../outside.log",
	}
	if err := m.writeLocked(job); err != nil {
		m.mu.Unlock()
		t.Fatal(err)
	}
	m.mu.Unlock()
	all, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Status != StatusStale {
		t.Fatalf("jobs=%+v", all)
	}
	if all[0].LogPath != "jobs/job_000001.log" {
		t.Fatalf("unsafe log path persisted: %q", all[0].LogPath)
	}
}

func nowForTest() time.Time { return time.Unix(1700000000, 0) }
