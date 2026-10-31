package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const watchVersion = 1

type watchSchedule struct {
	Version    int       `json:"version"`
	Active     bool      `json:"active"`
	Interval   string    `json:"interval"`
	Prompt     string    `json:"prompt"`
	NextRunAt  time.Time `json:"next_run_at"`
	LastRunAt  time.Time `json:"last_run_at,omitempty"`
	generation uint64
}

func (m *Model) registerWatchSlashCommand() {
	_ = m.RegisterSlashCommand(SlashCommand{
		Name:        "watch",
		Usage:       "/watch [<interval> <instruction>|stop]",
		Description: "run a recurring instruction in this single chat session",
		Run: func(args string) (SlashCommandResult, error) {
			args = strings.TrimSpace(args)
			if args == "" {
				return SlashCommandResult{Output: m.watchSummary()}, nil
			}
			if strings.EqualFold(args, "stop") {
				m.watch.generation++
				m.watch.Active = false
				if err := m.persistWatchSchedule(); err != nil {
					return SlashCommandResult{}, err
				}
				return SlashCommandResult{Output: "recurring watch stopped"}, nil
			}

			parts := strings.Fields(args)
			if len(parts) < 2 {
				return SlashCommandResult{}, fmt.Errorf("usage: /watch <interval> <instruction> (example: /watch 10m inspect the monitor, summarize changes, and fix safe actionable issues)")
			}
			interval, err := time.ParseDuration(parts[0])
			if err != nil || interval <= 0 {
				return SlashCommandResult{}, fmt.Errorf("invalid watch interval %q", parts[0])
			}
			prompt := strings.TrimSpace(strings.TrimPrefix(args, parts[0]))
			if prompt == "" {
				return SlashCommandResult{}, fmt.Errorf("watch instruction is required")
			}

			m.watch.generation++
			m.watch.Version = watchVersion
			m.watch.Active = true
			m.watch.Interval = interval.String()
			m.watch.Prompt = prompt
			m.watch.NextRunAt = time.Now().Add(interval)
			m.watch.LastRunAt = time.Time{}
			if err := m.persistWatchSchedule(); err != nil {
				return SlashCommandResult{}, err
			}
			return SlashCommandResult{
				Output: fmt.Sprintf("recurring watch enabled every %s in session %s\nnext run: %s\n%s", interval, m.sessionID, m.watch.NextRunAt.Format(time.RFC3339), prompt),
				cmd:    m.watchTimerCmd(),
			}, nil
		},
	})
}

func (m *Model) watchSummary() string {
	if !m.watch.Active {
		return "recurring watch is not active\n\nStart one with: /watch 10m <instruction>"
	}
	return fmt.Sprintf("recurring watch: every %s\nnext run: %s\ninstruction: %s", m.watch.Interval, m.watch.NextRunAt.Format(time.RFC3339), m.watch.Prompt)
}

func (m *Model) updateWatchFromSetup(active bool, interval, prompt string) error {
	if active {
		duration, err := time.ParseDuration(interval)
		if err != nil || duration < time.Second || duration > 24*time.Hour {
			return fmt.Errorf("invalid watch interval")
		}
		if strings.TrimSpace(prompt) == "" {
			return fmt.Errorf("watch instruction is required")
		}
		m.watch = watchSchedule{Version: watchVersion, Active: true, Interval: duration.String(), Prompt: strings.TrimSpace(prompt), NextRunAt: time.Now().Add(duration), generation: m.watch.generation + 1}
	} else {
		m.watch.generation++
		m.watch.Active = false
	}
	return m.persistWatchSchedule()
}

func (m *Model) watchTimerCmd() tea.Cmd {
	if !m.watch.Active {
		return nil
	}
	delay := time.Until(m.watch.NextRunAt)
	if delay < 0 {
		delay = 0
	}
	generation := m.watch.generation
	return tea.Tick(delay, func(time.Time) tea.Msg { return watchTickMsg{generation: generation} })
}

func watchSyncCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return watchSyncMsg{} })
}

// External watch_* tools write the schedule while an agent is streaming.
// Synchronize it into the TUI's timer without mutating the current composer.
func (m *Model) syncWatchSchedule() tea.Cmd {
	path := m.watchPath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var saved watchSchedule
	if err := json.Unmarshal(data, &saved); err != nil || saved.Version != watchVersion {
		return nil
	}
	if saved.Active == m.watch.Active && saved.Interval == m.watch.Interval && saved.Prompt == m.watch.Prompt && saved.NextRunAt.Equal(m.watch.NextRunAt) && saved.LastRunAt.Equal(m.watch.LastRunAt) {
		return nil
	}
	saved.generation = m.watch.generation + 1
	m.watch = saved
	return m.watchTimerCmd()
}

func (m *Model) handleWatchTick(msg watchTickMsg) (tea.Model, tea.Cmd) {
	if !m.watch.Active || msg.generation != m.watch.generation {
		return m, nil
	}
	if time.Now().Before(m.watch.NextRunAt) {
		return m, m.watchTimerCmd()
	}
	interval, err := time.ParseDuration(m.watch.Interval)
	if err != nil || interval <= 0 {
		m.watch.Active = false
		m.sessionSaveError = "invalid recurring watch interval; schedule disabled"
		_ = m.persistWatchSchedule()
		return m, nil
	}

	now := time.Now()
	m.watch.NextRunAt = now.Add(interval)
	var submit tea.Cmd
	// A recurring task must never clobber a draft or pile up behind a long
	// model/tool run. Skip this tick and try again at the next interval.
	if !m.streaming && strings.TrimSpace(m.composer.Value()) == "" && m.pendingPermission == nil && m.selectionPicker == nil && m.setup == nil && m.review == nil && !m.terminalVisible {
		m.watch.LastRunAt = now
		m.queuedInputs = append(m.queuedInputs, m.watch.Prompt)
		submit = m.nextQueuedSubmitCmd()
	}
	if err := m.persistWatchSchedule(); err != nil {
		m.sessionSaveError = err.Error()
	}
	return m, tea.Batch(submit, m.watchTimerCmd())
}

func (m *Model) watchPath() string {
	if m.sessionStore == nil || strings.TrimSpace(m.sessionID) == "" {
		return ""
	}
	return filepath.Join(m.sessionStore.Dir, "watch-"+m.sessionID+".json")
}

func (m *Model) loadWatchSchedule() {
	m.watch.generation++
	path := m.watchPath()
	if path == "" {
		m.watch.Active = false
		return
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		m.watch.Active = false
		return
	}
	if err != nil {
		m.watch.Active = false
		m.sessionSaveError = err.Error()
		return
	}
	var saved watchSchedule
	if err := json.Unmarshal(data, &saved); err != nil || saved.Version != watchVersion {
		m.watch.Active = false
		m.sessionSaveError = "could not restore recurring watch"
		return
	}
	saved.generation = m.watch.generation
	if saved.Active && saved.NextRunAt.IsZero() {
		if interval, err := time.ParseDuration(saved.Interval); err == nil && interval > 0 {
			saved.NextRunAt = time.Now().Add(interval)
		}
	}
	m.watch = saved
}

func (m *Model) persistWatchSchedule() error {
	path := m.watchPath()
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(m.watch, "", "  ")
	if err != nil {
		return fmt.Errorf("encode recurring watch: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".watch-*.tmp")
	if err != nil {
		return fmt.Errorf("create recurring watch temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("save recurring watch: %w", err)
	}
	return nil
}
