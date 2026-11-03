package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestF3ReviewShowsModifiedAndUntrackedFiles(t *testing.T) {
	cwd := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	run("init", "-q")
	run("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "initial")
	if err := os.WriteFile(filepath.Join(cwd, "new.txt"), []byte("hello\nworld\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &reviewModel{cwd: cwd, focusFiles: true}
	files := r.refresh()().(reviewFilesMsg)
	if files.err != nil || len(files.files) != 1 || files.files[0].path != "new.txt" {
		t.Fatalf("files: %+v", files)
	}
	if len(files.commits) != 1 || !strings.Contains(files.commits[0], "initial") {
		t.Fatalf("commit history missing: %+v", files.commits)
	}
	r.files = files.files
	diff := r.loadDiff()().(reviewDiffMsg)
	if diff.err != nil || !strings.Contains(diff.diff, "+hello") {
		t.Fatalf("diff: %+v", diff)
	}

	m := NewWithStreamer("test", "cos", &fakeStreamer{})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyF3}))
	if m.review == nil {
		t.Fatal("F3 did not open review")
	}
	m.review.files = r.files
	m.review.commits = files.commits
	m.review.diff = strings.Split(diff.diff, "\n")
	view := m.View().Content
	if !strings.Contains(view, "Changed files") || !strings.Contains(view, "new.txt") || !strings.Contains(view, "+hello") || !strings.Contains(view, "Recent commits") || !strings.Contains(view, "initial") {
		t.Fatalf("review not rendered: %s", view)
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if m.review != nil {
		t.Fatal("Esc failed to close review")
	}
}
