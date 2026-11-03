package tui

import (
	"fmt"

	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestF5MarkdownFolderAndExit(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "docs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "readme.md"), []byte("# Markdown heading\n\n- First entry\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b := newFileBrowser(root)
	if len(b.entries) != 1 || !b.entries[0].IsDir() {
		t.Fatal("missing directory")
	}
	b.key("enter")
	if len(b.entries) != 1 || b.entries[0].Name() != "readme.md" {
		t.Fatalf("navigation failed: %+v", b.entries)
	}
	if !strings.Contains(b.content, "Markdown heading") || !strings.Contains(b.view(110, 30).Content, "Files") {
		t.Fatal("markdown preview missing")
	}
	b.key("backspace")
	if b.dir != root {
		t.Fatal("parent navigation failed")
	}
	m := NewWithStreamer("test", "cos", &fakeStreamer{})
	m.SetTerminalService(NewTerminalService(root))
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyF5}))
	if m.browser == nil || m.browser.root != root {
		t.Fatal("F5 did not open project browser")
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if m.browser != nil {
		t.Fatal("F5 exit failed")
	}
}

func TestF5BinaryAndOversizePreview(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), []byte{0, 1, 2}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), make([]byte, browserMaxFileSize+1), 0600); err != nil {
		t.Fatal(err)
	}
	b := newFileBrowser(root)
	if !strings.Contains(b.content, "Binary") {
		t.Fatalf("binary preview: %q", b.content)
	}
	b.key("down")
	if !strings.Contains(b.content, "too large") {
		t.Fatalf("oversize preview: %q", b.content)
	}
}

func TestF5PageKeysUseVisibleHeight(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 55; i++ {
		name := fmt.Sprintf("%03d.txt", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte(strings.Repeat("line\n", 100)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b := newFileBrowser(root)
	b.view(100, 22)
	page := b.pageSize
	if page != 18 {
		t.Fatalf("unexpected page size %d", page)
	}
	b.key("pgdown")
	if b.selected != page {
		t.Fatalf("file page down: %d", b.selected)
	}
	b.key("pgup")
	if b.selected != 0 {
		t.Fatalf("file page up: %d", b.selected)
	}
	b.key("tab")
	b.key("pgdown")
	if b.previewScroll != page {
		t.Fatalf("preview page down: %d", b.previewScroll)
	}
	b.key("pgup")
	if b.previewScroll != 0 {
		t.Fatalf("preview page up: %d", b.previewScroll)
	}
}

func TestF5PreviewLineNumbersAfterPaging(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte(strings.Repeat("entry\n", 50)), 0600); err != nil {
		t.Fatal(err)
	}
	b := newFileBrowser(root)
	first := ansi.Strip(b.view(100, 20).Content)
	if !strings.Contains(first, "1 │ entry") {
		t.Fatalf("missing first line number: %q", first)
	}
	b.key("tab")
	b.key("pgdown")
	next := ansi.Strip(b.view(100, 20).Content)
	if !strings.Contains(next, "17 │ entry") {
		t.Fatalf("page should start at line 17: %q", next)
	}
}

func TestF5VimCharacterAndBlockSelection(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("abcdef\n123456\nABCDEF\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b := newFileBrowser(root)
	b.view(100, 20)
	b.key("enter")
	if b.focusFiles {
		t.Fatal("Enter must focus selected file")
	}
	b.key("l")
	b.key("v")
	b.key("l")
	b.key("l")
	if got := b.selectedText(); got != "bcd" {
		t.Fatalf("character selection = %q", got)
	}
	b.key("esc")
	b.key("ctrl+v")
	b.key("down")
	b.key("l")
	if got := b.selectedText(); got != "de\n45" {
		t.Fatalf("block selection = %q", got)
	}
	b.key("tab")
	if b.selectMode != "" || !b.focusFiles {
		t.Fatal("Tab must leave selection and focus files")
	}
}

func TestF5MarkdownFocusedPreviewDoesNotExposeANSISequences(t *testing.T) {
	root := t.TempDir()
	content := "# Title\n\nSome **strong** text\n\n```go\nfmt.Println(1)\n```\n"
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	b := newFileBrowser(root)
	b.view(100, 25)
	b.key("enter")
	if b.focusFiles {
		t.Fatal("Enter did not focus markdown")
	}
	for _, line := range b.lines(52) {
		if strings.Contains(line, "\x1b") || strings.Contains(line, "[38;2;") || strings.Contains(line, "[0m") {
			t.Fatalf("rendered markdown contains raw ANSI: %q", line)
		}
	}
	b.view(100, 25)
	b.key("ctrl+v")
	b.key("down")
	if strings.Contains(b.selectedText(), "\x1b") {
		t.Fatal("selected markdown copied escape bytes")
	}
}
