package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/trentkm/spanreed"
	"github.com/trentkm/spanreed/ptyadapter"
)

type terminalWorkspace struct {
	terminal spanreed.Model
	focused  bool
	name     string
}

// TerminalService owns exactly one PTY for the lifetime of the Rurushu process.
// It is shared by the agent tool and the F4 view; hiding F4 never closes it.
type TerminalService struct {
	mu        sync.Mutex
	workspace *terminalWorkspace
	started   bool
	cwd       string
}

func NewTerminalService(cwd string) *TerminalService { return &TerminalService{cwd: cwd} }

func (m *Model) SetTerminalService(service *TerminalService) { m.terminalService = service }
func (m *Model) terminalCWD() string {
	if m.terminalService != nil && m.terminalService.cwd != "" {
		return m.terminalService.cwd
	}
	cwd, _ := os.Getwd()
	return cwd
}

func (s *TerminalService) attach(width, height int) (*terminalWorkspace, tea.Cmd, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workspace == nil {
		w, _, err := spawnTerminalAt(s.cwd, width, height)
		if err != nil {
			return nil, nil, err
		}
		s.workspace = w
	}
	var cmd tea.Cmd
	if !s.started {
		cmd = s.workspace.terminal.Init()
		s.started = true
	}
	return s.workspace, cmd, nil
}

// SendCommand is an explicitly admitted shell submission, NOT execution proof.
// This service cannot reliably know when a command has finished or its exit code.
func (s *TerminalService) SendCommand(command string) error {
	command = strings.TrimSpace(command)
	if len(command) == 0 || len(command) > 8192 || strings.ContainsAny(command, "\r\n\x00\x1b") {
		return fmt.Errorf("terminal command must be one nonempty line (max 8192 bytes) without control characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workspace == nil {
		w, _, err := spawnTerminalAt(s.cwd, 100, 30)
		if err != nil {
			return err
		}
		s.workspace = w
	}
	return s.workspace.terminal.Write([]byte(command + "\n"))
}

func (s *TerminalService) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workspace != nil {
		s.workspace.close()
		s.workspace = nil
	}
}

func spawnTerminal(width, height int) (*terminalWorkspace, tea.Cmd, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	return spawnTerminalAt(cwd, width, height)
}

func spawnTerminalAt(cwd string, width, height int) (*terminalWorkspace, tea.Cmd, error) {
	cols, rows := terminalDimensions(width, height)
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	transport, err := ptyadapter.Spawn(context.Background(), ptyadapter.Spec{Command: shell, Dir: cwd}, cols, rows)
	if err != nil {
		return nil, nil, err
	}
	term := spanreed.New(transport, cols, rows, spanreed.WithReleaseKey("ctrl+]"))
	term.Blur()
	w := &terminalWorkspace{terminal: term, name: filepath.Base(cwd)}
	return w, term.Init(), nil
}

func terminalDimensions(width, height int) (int, int) {
	right := max(22, min(36, width/3))
	return max(1, width-right-3), max(1, height-3)
}

func (w *terminalWorkspace) update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc", "ctrl+]":
			if w.focused {
				w.terminal.Blur()
				w.focused = false
			}
			return nil
		case "tab":
			if !w.focused {
				w.focused = true
				w.terminal.Focus()
				return nil
			}
		case "enter":
			if !w.focused {
				w.focused = true
				w.terminal.Focus()
				return nil
			}
		}
	}
	var cmd tea.Cmd
	w.terminal, cmd = w.terminal.Update(msg)
	w.focused = w.terminal.Focused()
	return cmd
}

func (w *terminalWorkspace) resize(width, height int) tea.Cmd {
	cols, rows := terminalDimensions(width, height)
	var cmd tea.Cmd
	w.terminal, cmd = w.terminal.SetSize(cols, rows)
	return cmd
}

func (w *terminalWorkspace) close() { w.terminal.Close() }

func (w *terminalWorkspace) view(width, height int) tea.View {
	if width < 45 || height < 8 {
		v := tea.NewView("F4 Terminal: enlarge window · Esc to exit")
		v.AltScreen = true
		v.MouseMode = tea.MouseModeCellMotion
		return v
	}
	cols, rows := terminalDimensions(width, height)
	left := lipgloss.NewStyle().Width(cols).Height(rows + 1).Render("Terminal · " + w.name + "\n" + w.terminal.View())
	focus := "[Enter] Focus shell"
	if w.focused {
		focus = "[Ctrl+] Release keyboard"
	}
	right := lipgloss.NewStyle().Width(width - cols - 3).Height(rows + 1).Render("Sessions\n\n› 1 · " + w.name + "\n  PTY shell\n\n" + focus)
	footer := "F4/Esc close (unfocused) · Enter focus · Ctrl+] release · Ctrl+C interrupts shell"
	v := tea.NewView(lipgloss.JoinHorizontal(lipgloss.Top, left, " │ ", right) + "\n" + ansi.Truncate(strings.TrimSpace(footer), max(1, width), "…"))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "Rurushu Terminal"
	return v
}
