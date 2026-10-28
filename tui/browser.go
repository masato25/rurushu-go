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
	"github.com/atotto/clipboard"
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
	previewLines                        []string
	cursorRow, cursorCol                int
	anchorRow, anchorCol                int
	selectMode                          string
	copyStatus                          string
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
	b.cursorRow, b.cursorCol = 0, 0
	b.selectMode = ""
	b.copyStatus = ""
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

func (b *fileBrowser) lines(width int) []string {
	text := b.content
	if strings.EqualFold(filepath.Ext(b.selectedFile), ".md") || strings.EqualFold(filepath.Ext(b.selectedFile), ".markdown") {
		renderer, err := glamour.NewTermRenderer(glamour.WithStyles(rurushuMarkdownStyle()), glamour.WithWordWrap(max(20, width)), glamour.WithTableWrap(true))
		if err == nil {
			if rendered, err := renderer.Render(text); err == nil {
				text = rendered
			}
		}
	}
	// Glamour emits ANSI SGR sequences. Passing those through the file
	// selection/highlighting and Lip Gloss layout pipeline can leak fragments
	// such as "[38;2;...m" onto the screen. F5 owns its highlights, so use
	// clean display text before line slicing and selection.
	text = ansi.Strip(text)
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

func (b *fileBrowser) selectedText() string {
	if b.selectMode == "" || len(b.previewLines) == 0 {
		return ""
	}
	aRow, cRow := b.anchorRow, b.cursorRow
	aCol, cCol := b.anchorCol, b.cursorCol
	if aRow > cRow {
		aRow, cRow = cRow, aRow
	}
	if b.selectMode == "block" {
		if aCol > cCol {
			aCol, cCol = cCol, aCol
		}
		var parts []string
		for i := aRow; i <= cRow; i++ {
			r := []rune(ansi.Strip(b.previewLines[i]))
			start := min(aCol, len(r))
			end := min(cCol+1, len(r))
			parts = append(parts, string(r[start:max(start, end)]))
		}
		return strings.Join(parts, "\n")
	}
	startRow, startCol, endRow, endCol := b.anchorRow, b.anchorCol, b.cursorRow, b.cursorCol
	if startRow > endRow || (startRow == endRow && startCol > endCol) {
		startRow, endRow = endRow, startRow
		startCol, endCol = endCol, startCol
	}
	var parts []string
	for i := startRow; i <= endRow; i++ {
		r := []rune(ansi.Strip(b.previewLines[i]))
		start, end := 0, len(r)
		if i == startRow {
			start = min(startCol, len(r))
		}
		if i == endRow {
			end = min(endCol+1, len(r))
		}
		parts = append(parts, string(r[start:max(start, end)]))
	}
	return strings.Join(parts, "\n")
}

func (b *fileBrowser) highlightLine(index int, line string) string {
	if b.selectMode == "" {
		return line
	}
	r := []rune(ansi.Strip(line))
	start, end := -1, -1
	if b.selectMode == "block" {
		lo, hi := min(b.anchorRow, b.cursorRow), max(b.anchorRow, b.cursorRow)
		if index >= lo && index <= hi {
			start = min(b.anchorCol, b.cursorCol)
			end = max(b.anchorCol, b.cursorCol) + 1
		}
	} else {
		ar, ac, cr, cc := b.anchorRow, b.anchorCol, b.cursorRow, b.cursorCol
		if ar > cr || (ar == cr && ac > cc) {
			ar, cr = cr, ar
			ac, cc = cc, ac
		}
		if index >= ar && index <= cr {
			start = 0
			end = len(r)
			if index == ar {
				start = ac
			}
			if index == cr {
				end = cc + 1
			}
		}
	}
	if start < 0 {
		return line
	}
	start = min(start, len(r))
	end = min(max(start, end), len(r))
	return string(r[:start]) + lipgloss.NewStyle().Reverse(true).Render(string(r[start:end])) + string(r[end:])
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
	if !b.focusFiles {
		if k == "esc" {
			b.selectMode = ""
			b.copyStatus = ""
			return
		}
		if k == "v" || k == "ctrl+v" {
			mode := "char"
			if k == "ctrl+v" {
				mode = "block"
			}
			if b.selectMode == mode {
				b.selectMode = ""
			} else {
				b.selectMode = mode
				b.anchorRow, b.anchorCol = b.cursorRow, b.cursorCol
			}
			return
		}
		if k == "y" && b.selectMode != "" {
			if err := clipboard.WriteAll(b.selectedText()); err != nil {
				b.copyStatus = "Copy failed: " + err.Error()
			} else {
				b.copyStatus = "Copied selection"
			}
			b.selectMode = ""
			return
		}
		if len(b.previewLines) > 0 {
			step := 0
			switch k {
			case "up", "k":
				step = -1
			case "down", "j":
				step = 1
			case "pgup":
				step = -page
			case "pgdown":
				step = page
			}
			if step != 0 {
				b.cursorRow = max(0, min(len(b.previewLines)-1, b.cursorRow+step))
				if k == "pgup" || k == "pgdown" {
					b.previewScroll = b.cursorRow
				} else {
					b.previewScroll = max(0, min(b.previewScroll, b.cursorRow))
					if b.cursorRow >= b.previewScroll+page {
						b.previewScroll = b.cursorRow - page + 1
					}
				}
				b.cursorCol = min(b.cursorCol, len([]rune(ansi.Strip(b.previewLines[b.cursorRow]))))
				return
			}
			if k == "h" || k == "left" {
				b.cursorCol = max(0, b.cursorCol-1)
				return
			}
			if k == "l" || k == "right" {
				b.cursorCol = min(len([]rune(ansi.Strip(b.previewLines[b.cursorRow]))), b.cursorCol+1)
				return
			}
		}
	}
	switch k {
	case "tab", "left", "right":
		b.selectMode = ""
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
		} else if b.focusFiles && b.selectedFile != "" {
			b.focusFiles = false
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
	lines := b.lines(leftWidth - 8)
	b.previewLines = lines
	b.cursorRow = min(b.cursorRow, max(0, len(lines)-1))
	b.previewScroll = min(b.previewScroll, max(0, len(lines)-rows+1))
	preview := []string{ansi.Truncate(header, leftWidth, "…")}
	gutterWidth := len(fmt.Sprintf("%d", len(lines))) + 2
	contentWidth := max(1, leftWidth-gutterWidth)
	for i := b.previewScroll; i < len(lines) && len(preview) < rows; i++ {
		content := b.highlightLine(i, lines[i])
		if !b.focusFiles && i == b.cursorRow && b.selectMode == "" {
			content = lipgloss.NewStyle().Underline(true).Render(content)
		}
		preview = append(preview, numberedPreviewLine(i+1, content, gutterWidth, contentWidth))
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
	footer := "F5 close · Enter focus · Tab switch · hjkl move · v select · Ctrl+V block · y copy · Esc cancel"
	if b.copyStatus != "" {
		footer = b.copyStatus + " · " + footer
	}
	v := tea.NewView(lipgloss.JoinHorizontal(lipgloss.Top, left, " │ ", right) + "\n" + ansi.Truncate(footer, width, "…"))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func numberedPreviewLine(number int, content string, gutterWidth, contentWidth int) string {
	gutter := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(fmt.Sprintf("%*d │", gutterWidth-2, number))
	return gutter + " " + ansi.Truncate(content, contentWidth, "…")
}
