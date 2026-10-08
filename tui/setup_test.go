package tui

import (
	"os"
	"testing"

	tea "charm.land/bubbletea/v2"
	rurushuconfig "github.com/arborlogic/rurushu-go/config"
)

func TestSetupSave(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	m := NewSetupModel(rurushuconfig.Defaults())
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
