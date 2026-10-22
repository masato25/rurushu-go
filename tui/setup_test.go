package tui

import (
	"os"
	"testing"

	tea "charm.land/bubbletea/v2"
	rurushuconfig "github.com/masato25/rurushu-go/config"
)

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
