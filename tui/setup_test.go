package tui

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	rurushuconfig "github.com/masato25/rurushu-go/config"
)

func TestSetupScreenClipsToTerminalAndEnablesMouse(t *testing.T) {
	m := NewSetupModel(rurushuconfig.Defaults())
	m.Update(tea.WindowSizeMsg{Width: 72, Height: 9})
	v := m.View()
	if v.MouseMode == tea.MouseModeNone {
		t.Fatal("setup must capture wheel events")
	}
	if lines := strings.Count(v.Content, "\n") + 1; lines > 9 {
		t.Fatalf("setup overflows terminal: %d lines", lines)
	}
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.scroll == 0 {
		t.Fatal("mouse wheel did not scroll setup")
	}
}

func TestSetupSave(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	cfg := rurushuconfig.Defaults()
	cfg.AutoApprove = true
	m := NewSetupModel(cfg)
	m.inputs[setupModel].SetValue("test-model")
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	if m.SavedPath() == "" {
		t.Fatal("missing saved path")
	}
	if _, err := os.Stat(m.SavedPath()); err != nil {
		t.Fatal(err)
	}
	saved, err := rurushuconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.AutoApprove {
		t.Fatal("setup must preserve the auto approve setting")
	}
}

func TestSetupEnterAdvancesThenSaves(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := NewSetupModel(rurushuconfig.Defaults())
	m.inputs[setupModel].SetValue("test-model")
	for i := 0; i < setupFieldCount-1; i++ {
		_, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	}
	_, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if !m.saved {
		t.Fatal("expected setup to save on final enter")
	}
}

func TestSetupGoalEditorDisplaysAndSavesGoal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := NewSetupModel(rurushuconfig.Defaults())
	m.inputs[setupModel].SetValue("test-model")
	var saved string
	m.EnableGoalEditor("current duel goal", func(goal string) error {
		saved = goal
		return nil
	})
	if view := m.View().Content; !strings.Contains(view, "Duel goal") || !strings.Contains(view, "current duel goal") {
		t.Fatalf("goal editor missing from setup: %q", view)
	}
	m.inputs[len(m.inputs)-1].SetValue("updated duel goal")
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	if saved != "updated duel goal" {
		t.Fatalf("saved goal=%q", saved)
	}
}
