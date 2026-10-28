package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestF4TerminalLifecycle(t *testing.T) {
	m := NewWithStreamer("test", "cos", &fakeStreamer{})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyF4}))
	if m.terminal == nil || cmd == nil {
		t.Fatal("F4 should open a terminal and begin the frame pump")
	}
	defer func() {
		if m.terminal != nil {
			m.terminal.close()
		}
	}()
	view := m.View()
	if !view.AltScreen || view.MouseMode == tea.MouseModeNone || !strings.Contains(view.Content, "Sessions") {
		t.Fatalf("F4 view missing terminal layout: %+v", view)
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if !m.terminal.focused {
		t.Fatal("Enter should focus the PTY")
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if m.terminal == nil || m.terminal.focused {
		t.Fatal("Esc must release shell focus without closing F4")
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if !m.terminal.focused {
		t.Fatal("Enter must refocus shell")
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: ']', Mod: tea.ModCtrl}))
	if m.terminal.focused {
		t.Fatal("Ctrl+] should release PTY keyboard focus")
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyF4}))
	if m.terminalVisible || m.terminal == nil {
		t.Fatal("F4 should hide the workspace, keeping its PTY alive")
	}
	previous := m.terminal
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyF4}))
	if !m.terminalVisible || m.terminal != previous {
		t.Fatal("F4 reopen must reuse the existing PTY")
	}
}

func TestTerminalDimensionsStayPositive(t *testing.T) {
	for _, size := range [][2]int{{0, 0}, {1, 1}, {45, 8}, {100, 30}} {
		cols, rows := terminalDimensions(size[0], size[1])
		if cols < 1 || rows < 1 {
			t.Fatalf("size %v resulted in %d x %d", size, cols, rows)
		}
	}
}

func TestTerminalServiceSharesShellAcrossHiddenView(t *testing.T) {
	s := NewTerminalService(t.TempDir())
	defer s.Close()
	if err := s.SendCommand("echo shared_session"); err != nil {
		t.Fatal(err)
	}
	first, cmd, err := s.attach(100, 30)
	if err != nil || first == nil || cmd == nil {
		t.Fatalf("first attach: %v", err)
	}
	second, cmd, err := s.attach(100, 30)
	if err != nil || second != first || cmd != nil {
		t.Fatalf("reattach created a different PTY: %v", err)
	}
}
