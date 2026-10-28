package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const browserMaxFileSize = 512 * 1024

type fileBrowser struct {
	root, dir, selectedFile             string
	entries                             []os.DirEntry
	selected, listScroll, previewScroll int
	focusFiles                          bool
	pageSize                            int
	content, errText                    string
}

func newFileBrowser(root string) *fileBrowser {
	b := &fileBrowser{root: root, dir: root, focusFiles: true}
	b.refresh()
	return b
}

func (b *fileBrowser) refresh() {
	items, err := os.ReadDir(b.dir)
	if err != nil {
		b.errText = err.Error()
		b.entries = nil
		return
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsDir() != items[j].IsDir() {
			return items[i].IsDir()
		}
		return strings.ToLower(items[i].Name()) < strings.ToLower(items[j].Name())
	})
	b.entries = items
	b.selected = min(b.selected, max(0, len(items)-1))
	b.errText = ""
	b.loadSelected()
}

func (b *fileBrowser) loadSelected() {
	b.previewScroll = 0
	b.selectedFile = ""
	b.content = ""
	if len(b.entries) == 0 {
		return
	}
	entry := b.entries[b.selected]
	path := filepath.Join(b.dir, entry.Name())
	if entry.IsDir() {
		b.content = "Directory: " + entry.Name() + "\nPress Enter to open."
		return
	}
	if entry.Type()&os.ModeSymlink != 0 {
		b.content = "Symbolic link preview disabled."
		return
	}
	info, err := entry.Info()
	if err != nil {
		b.content = err.Error()
		return
	}
	if !info.Mode().IsRegular() {
		b.content = "Not a regular file."
		return
	}
	if info.Size() > browserMaxFileSize {
		b.content = fmt.Sprintf("File too large to preview (%d bytes, limit %d).", info.Size(), browserMaxFileSize)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		b.content = err.Error()
		return
	}
	if strings.ContainsRune(string(data), '\x00') || !utf8.Valid(data) {
		b.content = "Binary/non-UTF-8 file preview unavailable."
		return
	}
	b.selectedFile = path
	b.content = string(data)
}

func (b *fileBrowser) goParent() {
	if b.dir == b.root {
		return
	}
	old := filepath.Base(b.dir)
	b.dir = filepath.Dir(b.dir)
	b.selected = 0
	b.listScroll = 0
	b.refresh()
	for i, e := range b.entries {
		if e.Name() == old {
			b.selected = i
			b.loadSelected()
			break
		}
	}
}

func (b *fileBrowser) key(k string) {
	page := max(1, b.pageSize)
	switch k {
	case "tab", "left", "right":
		b.focusFiles = !b.focusFiles
	case "backspace", "h":
		if b.focusFiles {
			b.goParent()
		} else {
			b.previewScroll = max(0, b.previewScroll-5)
		}
	case "enter", "l":
		if b.focusFiles && len(b.entries) > 0 && b.entries[b.selected].IsDir() {
			b.dir = filepath.Join(b.dir, b.entries[b.selected].Name())
			b.selected = 0
			b.listScroll = 0
			b.refresh()
		}
	case "up", "k":
		if b.focusFiles {
			b.selected = max(0, b.selected-1)
			b.loadSelected()
		} else {
			b.previewScroll = max(0, b.previewScroll-1)
		}
	case "down", "j":
		if b.focusFiles {
			b.selected = min(max(0, len(b.entries)-1), b.selected+1)
			b.loadSelected()
		} else {
			b.previewScroll++
		}
	case "pgup":
		if b.focusFiles {
			b.selected = max(0, b.selected-page)
			b.loadSelected()
		} else {
			b.previewScroll = max(0, b.previewScroll-page)
		}
	case "pgdown":
		if b.focusFiles {
			b.selected = min(max(0, len(b.entries)-1), b.selected+page)
			b.loadSelected()
		} else {
			b.previewScroll += page
		}
	case "r":
		b.refresh()
	}
}

func (b *fileBrowser) wheel(button tea.MouseButton) {
	if button == tea.MouseWheelUp {
		b.key("up")
	} else if button == tea.MouseWheelDown {
		b.key("down")
	}
}

func (b *fileBrowser) view(width, height int) tea.View {
	if width < 48 || height < 8 {
		v := tea.NewView("F5 Files: enlarge window · Esc closes")
		v.AltScreen = true
		v.MouseMode = tea.MouseModeCellMotion
		return v
	}
	rightWidth := max(23, min(42, width/3))
	leftWidth := width - rightWidth - 3
	rows := height - 2
	b.pageSize = max(1, rows-2)
	header := "Preview"
	if b.selectedFile != "" {
		header = filepath.Base(b.selectedFile)
	}
	var text string
	if strings.EqualFold(filepath.Ext(b.selectedFile), ".md") || strings.EqualFold(filepath.Ext(b.selectedFile), ".markdown") {
		renderer, err := glamour.NewTermRenderer(glamour.WithStyles(rurushuMarkdownStyle()), glamour.WithWordWrap(max(20, leftWidth-2)), glamour.WithTableWrap(true))
		if err == nil {
			text, err = renderer.Render(b.content)
		}
		if err != nil {
			text = b.content
		}
	} else {
		text = b.content
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	b.previewScroll = min(b.previewScroll, max(0, len(lines)-rows+1))
	preview := []string{ansi.Truncate(header, leftWidth, "…")}
	gutterWidth := len(fmt.Sprintf("%d", len(lines))) + 2
	contentWidth := max(1, leftWidth-gutterWidth)
	for i := b.previewScroll; i < len(lines) && len(preview) < rows; i++ {
		preview = append(preview, numberedPreviewLine(i+1, lines[i], gutterWidth, contentWidth))
	}
	for len(preview) < rows {
		preview = append(preview, "")
	}
	path, _ := filepath.Rel(b.root, b.dir)
	list := []string{ansi.Truncate("Files · "+path, rightWidth, "…")}
	if b.dir != b.root {
		list = append(list, "[..] parent")
	}
	visible := rows - len(list)
	if b.selected < b.listScroll {
		b.listScroll = b.selected
	}
	if b.selected >= b.listScroll+visible {
		b.listScroll = b.selected - visible + 1
	}
	for i := b.listScroll; i < len(b.entries) && len(list) < rows; i++ {
		e := b.entries[i]
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		mark := "  "
		if i == b.selected {
			mark = "> "
		}
		list = append(list, ansi.Truncate(mark+name, rightWidth, "…"))
	}
	if b.errText != "" && len(list) < rows {
		list = append(list, b.errText)
	}
	for len(list) < rows {
		list = append(list, "")
	}
	left := lipgloss.NewStyle().Width(leftWidth).Height(rows).Render(strings.Join(preview, "\n"))
	right := lipgloss.NewStyle().Width(rightWidth).Height(rows).Render(strings.Join(list, "\n"))
	footer := "F5/Esc close · Tab focus · ↑↓ navigate/scroll · Enter folder · Backspace parent · r refresh"
	v := tea.NewView(lipgloss.JoinHorizontal(lipgloss.Top, left, " │ ", right) + "\n" + ansi.Truncate(footer, width, "…"))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func numberedPreviewLine(number int, content string, gutterWidth, contentWidth int) string {
	gutter := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(fmt.Sprintf("%*d │", gutterWidth-2, number))
	return gutter + " " + ansi.Truncate(content, contentWidth, "…")
}
