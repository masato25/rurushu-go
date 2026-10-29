package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestAuxiliaryWorkspaceFunctionKeyPanels(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewTerminalService(root)
	defer service.Close()
	aux := NewAuxiliaryWorkspace(root, service)
	aux.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	for _, test := range []struct {
		key      rune
		contains string
	}{
		{tea.KeyF2, "rurushu setup"},
		{tea.KeyF3, "Code Review"},
		{tea.KeyF5, "Files"},
	} {
		handled, _ := aux.Update(tea.KeyPressMsg(tea.Key{Code: test.key}))
		if !handled || !aux.Active() {
			t.Fatalf("%v did not activate auxiliary workspace", test.key)
		}
		view, ok := aux.View()
		if !ok || !strings.Contains(view.Content, test.contains) {
			t.Fatalf("%v view missing %q: %q", test.key, test.contains, view.Content)
		}
		aux.Update(tea.KeyPressMsg(tea.Key{Code: test.key}))
		if aux.Active() {
			t.Fatalf("%v did not close its panel", test.key)
		}
	}

	handled, cmd := aux.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyF4}))
	if !handled || !aux.Active() || cmd == nil {
		t.Fatal("F4 did not open shared terminal workspace")
	}
	view, ok := aux.View()
	if !ok || !strings.Contains(view.Content, "Sessions") {
		t.Fatalf("F4 terminal view missing: %q", view.Content)
	}
	aux.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyF4}))
	if aux.Active() {
		t.Fatal("F4 did not hide terminal workspace")
	}
}
