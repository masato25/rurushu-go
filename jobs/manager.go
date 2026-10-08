package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Status string

const (
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusExited   Status = "exited"
	StatusFailed   Status = "failed"
	StatusStopped  Status = "stopped"
	StatusStale    Status = "stale"
)

type Job struct {
	ID             int        `json:"id"`
	Command        string     `json:"command"`
	CWD            string     `json:"cwd"`
	Status         Status     `json:"status"`
	RunnerPID      int        `json:"runner_pid,omitempty"`
	RunnerIdentity string     `json:"runner_identity,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	ExitCode       *int       `json:"exit_code,omitempty"`
	LogPath        string     `json:"log_path"`
	Error          string     `json:"error,omitempty"`
}

type Manager struct {
	projectRoot string
	stateDir    string
	jobsDir     string
	executable  string
	mu          sync.Mutex
}

func Open(projectRoot, stateDir, executable string) (*Manager, error) {
	projectRoot, err := filepath.Abs(strings.TrimSpace(projectRoot))
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	stateDir, err = filepath.Abs(strings.TrimSpace(stateDir))
	if err != nil {
		return nil, fmt.Errorf("resolve state dir: %w", err)
	}
	jobsDir := filepath.Join(stateDir, "jobs")
	if info, lstatErr := os.Lstat(jobsDir); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("jobs directory must not be a symlink")
	} else if lstatErr != nil && !os.IsNotExist(lstatErr) {
		return nil, fmt.Errorf("inspect jobs dir: %w", lstatErr)
	}
	if err := os.MkdirAll(jobsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create jobs dir: %w", err)
	}
	_ = os.Chmod(jobsDir, 0o700)
	if strings.TrimSpace(executable) == "" {
		executable, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve rurushu executable: %w", err)
		}
	}
	return &Manager{projectRoot: projectRoot, stateDir: stateDir, jobsDir: jobsDir, executable: executable}, nil
}

func (m *Manager) Start(command string) (Job, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return Job{}, fmt.Errorf("command is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	id, err := m.nextIDLocked()
	if err != nil {
		return Job{}, err
	}
	job := Job{
		ID: id, Command: command, CWD: m.projectRoot, Status: StatusStarting,
		StartedAt: time.Now(), LogPath: filepath.Join("jobs", fmt.Sprintf("job_%06d.log", id)),
	}
	if err := m.writeLocked(job); err != nil {
		return Job{}, err
	}

	cmd := exec.Command(m.executable, "__job-runner", "--state-dir", m.stateDir, "--job", strconv.Itoa(id))
	configureDetached(cmd)
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return Job{}, err
	}
	defer devNull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	if err := cmd.Start(); err != nil {
		job.Status = StatusFailed
		job.Error = err.Error()
		now := time.Now()
		job.FinishedAt = &now
		_ = m.writeLocked(job)
		return Job{}, fmt.Errorf("start managed job: %w", err)
	}
	job.RunnerPID = cmd.Process.Pid
	_ = cmd.Process.Release()
	for attempt := 0; attempt < 25; attempt++ {
		current, readErr := m.readLocked(id)
		if readErr == nil && current.RunnerPID > 0 {
			return current, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return job, nil
}

func (m *Manager) List() ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	jobs, err := m.readAllLocked()
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		changed, reconcileErr := m.reconcileLocked(&jobs[i])
		if reconcileErr != nil {
			continue
		}
		if changed {
			_ = m.writeLocked(jobs[i])
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs, nil
}

func (m *Manager) Get(id int) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, err := m.readLocked(id)
	if err != nil {
		return Job{}, err
	}
	if changed, _ := m.reconcileLocked(&job); changed {
		_ = m.writeLocked(job)
	}
	return job, nil
}

func (m *Manager) RunningCount() int {
	jobs, err := m.List()
	if err != nil {
		return 0
	}
	count := 0
	for _, job := range jobs {
		if job.Status == StatusStarting || job.Status == StatusRunning {
			count++
		}
	}
	return count
}

func (m *Manager) Stop(id int) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, err := m.readLocked(id)
	if err != nil {
		return Job{}, err
	}
	if job.Status != StatusStarting && job.Status != StatusRunning {
		return job, fmt.Errorf("job #%d is %s", id, job.Status)
	}
	if !m.matchesRunner(job) {
		job.Status = StatusStale
		now := time.Now()
		job.FinishedAt = &now
		_ = m.writeLocked(job)
		return job, fmt.Errorf("job #%d runner is no longer the recorded process", id)
	}
	if err := stopProcessGroup(job.RunnerPID, 2*time.Second); err != nil {
		return job, fmt.Errorf("stop job #%d: %w", id, err)
	}
	job.Status = StatusStopped
	job.Error = ""
	now := time.Now()
	job.FinishedAt = &now
	if err := m.writeLocked(job); err != nil {
		return Job{}, err
	}
	return job, nil
}

func (m *Manager) Tail(id, maxBytes int) (string, error) {
	_, err := m.Get(id)
	if err != nil {
		return "", err
	}
	if maxBytes <= 0 {
		maxBytes = 32 * 1024
	}
	if maxBytes > 256*1024 {
		maxBytes = 256 * 1024
	}
	path := m.logPath(id)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := info.Size() - int64(maxBytes)
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)))
	if err != nil {
		return "", err
	}
	if start > 0 {
		if index := strings.IndexByte(string(data), '\n'); index >= 0 {
			data = data[index+1:]
		}
	}
	return strings.TrimSpace(string(data)), nil
}

func (m *Manager) RunJob(ctx context.Context, id int) error {
	m.mu.Lock()
	job, err := m.readLocked(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	identity, _ := processIdentity(os.Getpid())
	job.RunnerPID = os.Getpid()
	job.RunnerIdentity = identity
	job.Status = StatusRunning
	job.Error = ""
	if err := m.writeLocked(job); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	logPath := m.logPath(id)
	logFile, err := openRollingLog(logPath)
	if err != nil {
		return m.finishRunner(id, -1, err)
	}
	defer logFile.Close()
	_, _ = fmt.Fprintf(logFile, "[%s] rurushu job #%d started: %s\n", time.Now().Format(time.RFC3339), id, job.Command)

	cmd := shellCommand(ctx, job.Command)
	cmd.Dir = job.CWD
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	err = cmd.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	_, _ = fmt.Fprintf(logFile, "\n[%s] rurushu job #%d exited with code %d\n", time.Now().Format(time.RFC3339), id, exitCode)
	return m.finishRunner(id, exitCode, err)
}

func (m *Manager) finishRunner(id, exitCode int, runErr error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, err := m.readLocked(id)
	if err != nil {
		return err
	}
	if job.Status == StatusStopped {
		return nil
	}
	now := time.Now()
	job.FinishedAt = &now
	job.ExitCode = &exitCode
	if runErr == nil {
		job.Status = StatusExited
		job.Error = ""
	} else {
		job.Status = StatusFailed
		job.Error = runErr.Error()
	}
	return m.writeLocked(job)
}

func (m *Manager) reconcileLocked(job *Job) (bool, error) {
	if job.Status != StatusStarting && job.Status != StatusRunning {
		return false, nil
	}
	if job.Status == StatusStarting && job.RunnerPID <= 0 && time.Since(job.StartedAt) < 2*time.Second {
		return false, nil
	}
	if m.matchesRunner(*job) {
		if job.Status != StatusRunning {
			job.Status = StatusRunning
			return true, nil
		}
		return false, nil
	}
	job.Status = StatusStale
	now := time.Now()
	job.FinishedAt = &now
	if job.Error == "" {
		job.Error = "managed runner is no longer running"
	}
	return true, nil
}

func (m *Manager) matchesRunner(job Job) bool {
	if job.RunnerPID <= 0 || job.RunnerIdentity == "" || !processAlive(job.RunnerPID) {
		return false
	}
	identity, err := processIdentity(job.RunnerPID)
	return err == nil && identity == job.RunnerIdentity
}

func (m *Manager) nextIDLocked() (int, error) {
	jobs, err := m.readAllLocked()
	if err != nil {
		return 0, err
	}
	maxID := 0
	for _, job := range jobs {
		if job.ID > maxID {
			maxID = job.ID
		}
	}
	return maxID + 1, nil
}

func (m *Manager) readAllLocked() ([]Job, error) {
	entries, err := os.ReadDir(m.jobsDir)
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.jobsDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var job Job
		if err := json.Unmarshal(data, &job); err != nil {
			return nil, fmt.Errorf("decode job %s: %w", entry.Name(), err)
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (m *Manager) readLocked(id int) (Job, error) {
	data, err := os.ReadFile(m.jobPath(id))
	if os.IsNotExist(err) {
		return Job{}, fmt.Errorf("job #%d not found", id)
	}
	if err != nil {
		return Job{}, err
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return Job{}, fmt.Errorf("decode job #%d: %w", id, err)
	}
	return job, nil
}

func (m *Manager) writeLocked(job Job) error {
	job.LogPath = filepath.ToSlash(filepath.Join("jobs", fmt.Sprintf("job_%06d.log", job.ID)))
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(m.jobsDir, ".job-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, m.jobPath(job.ID))
}

func (m *Manager) jobPath(id int) string {
	return filepath.Join(m.jobsDir, fmt.Sprintf("job_%06d.json", id))
}

func (m *Manager) logPath(id int) string {
	return filepath.Join(m.jobsDir, fmt.Sprintf("job_%06d.log", id))
}
