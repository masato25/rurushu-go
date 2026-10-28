package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type reviewFile struct {
	path, status string
	untracked    bool
}
type reviewModel struct {
	cwd                          string
	files                        []reviewFile
	selected, scroll, generation int
	focusFiles, loading          bool
	diff                         []string
	err                          string
	commits                      []string
	commitError                  string
}
type reviewFilesMsg struct {
	generation  int
	files       []reviewFile
	commits     []string
	commitError string
	err         error
}
type reviewDiffMsg struct {
	generation int
	diff       string
	err        error
}

func newReviewModel() *reviewModel {
	cwd, _ := os.Getwd()
	return &reviewModel{cwd: cwd, focusFiles: true}
}

func reviewGit(ctx context.Context, cwd string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "git", append([]string{"-C", cwd, "--no-pager"}, args...)...).Output()
}

func (r *reviewModel) refresh() tea.Cmd {
	r.generation++
	id, cwd := r.generation, r.cwd
	r.loading = true
	r.err = ""
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		data, err := reviewGit(ctx, cwd, "status", "--porcelain=v1", "-z", "--untracked-files=all")
		if err != nil {
			return reviewFilesMsg{generation: id, err: fmt.Errorf("git status: %w", err)}
		}
		parts := strings.Split(string(data), "\x00")
		files := make([]reviewFile, 0, len(parts))
		for i := 0; i < len(parts); i++ {
			entry := parts[i]
			if len(entry) < 4 {
				continue
			}
			status := entry[:2]
			path := entry[3:]
			if status[0] == 'R' || status[0] == 'C' || status[1] == 'R' || status[1] == 'C' {
				i++
			}
			files = append(files, reviewFile{path: path, status: status, untracked: status == "??"})
		}
		commits, commitErr := reviewGit(ctx, cwd, "log", "-7", "--date=format-local:%m-%d %H:%M", "--pretty=format:%h%x09%ad%x09%s")
		var commitLines []string
		if commitErr == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(commits)), "\n") {
				if line != "" {
					commitLines = append(commitLines, strings.ReplaceAll(line, "\t", "  "))
				}
			}
		}
		var commitError string
		if commitErr != nil {
			commitError = "Commit history unavailable"
		}
		return reviewFilesMsg{generation: id, files: files, commits: commitLines, commitError: commitError}
	}
}

func (r *reviewModel) loadDiff() tea.Cmd {
	r.generation++
	id, cwd := r.generation, r.cwd
	r.loading = true
	r.err = ""
	r.scroll = 0
	if len(r.files) == 0 {
		r.loading = false
		r.diff = nil
		return nil
	}
	f := r.files[r.selected]
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if f.untracked {
			bytes, err := os.ReadFile(filepath.Join(cwd, f.path))
			if err != nil {
				return reviewDiffMsg{generation: id, err: err}
			}
			if len(bytes) > 256*1024 {
				return reviewDiffMsg{generation: id, diff: "Untracked file exceeds 256 KiB preview limit"}
			}
			if strings.IndexByte(string(bytes), 0) >= 0 {
				return reviewDiffMsg{generation: id, diff: "Binary untracked file (preview unavailable)"}
			}
			lines := []string{"--- (untracked)", "+++ " + f.path, "@@ new file @@"}
			for _, line := range strings.Split(strings.TrimSuffix(string(bytes), "\n"), "\n") {
				lines = append(lines, "+"+line)
			}
			return reviewDiffMsg{generation: id, diff: strings.Join(lines, "\n")}
		}
		data, err := reviewGit(ctx, cwd, "diff", "--no-ext-diff", "--no-color", "HEAD", "--", f.path)
		return reviewDiffMsg{generation: id, diff: string(data), err: err}
	}
}

func (r *reviewModel) key(key string) tea.Cmd {
	switch key {
	case "r":
		return r.refresh()
	case "tab", "left", "right":
		r.focusFiles = !r.focusFiles
	case "up", "k":
		if r.focusFiles {
			if r.selected > 0 {
				r.selected--
				return r.loadDiff()
			}
		} else {
			r.scroll = max(0, r.scroll-1)
		}
	case "down", "j":
		if r.focusFiles {
			if r.selected+1 < len(r.files) {
				r.selected++
				return r.loadDiff()
			}
		} else {
			r.scroll++
		}
	case "pgup":
		r.scroll = max(0, r.scroll-12)
	case "pgdown":
		r.scroll += 12
	}
	return nil
}

func (r *reviewModel) wheel(button tea.MouseButton) tea.Cmd {
	key := "down"
	if button == tea.MouseWheelUp {
		key = "up"
	}
	for i := 0; i < 3; i++ {
		if cmd := r.key(key); cmd != nil {
			return cmd
		}
	}
	return nil
}

func (r *reviewModel) view(width, height int) tea.View {
	if width < 40 || height < 8 {
		v := tea.NewView("F3 Code Review: enlarge terminal · Esc to return")
		v.AltScreen = true
		return v
	}
	rightWidth := max(22, min(44, width/3))
	leftWidth := max(10, width-rightWidth-3)
	trim := func(s string, n int) string { return ansi.Truncate(s, max(1, n), "…") }
	title := "Code Review  ·  Diff"
	if r.loading {
		title += "  (loading)"
	}
	if r.err != "" {
		title += "  ERROR: " + r.err
	}
	leftLines := []string{trim(title, leftWidth)}
	if len(r.files) == 0 && !r.loading {
		leftLines = append(leftLines, "No Git changes")
	}
	visible := max(1, height-3)
	r.scroll = min(r.scroll, max(0, len(r.diff)-visible))
	for _, line := range r.diff[r.scroll:min(len(r.diff), r.scroll+visible)] {
		shown := trim(line, leftWidth)
		switch {
		case strings.HasPrefix(line, "+"):
			shown = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render(shown)
		case strings.HasPrefix(line, "-"):
			shown = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render(shown)
		case strings.HasPrefix(line, "@@"):
			shown = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(shown)
		}
		leftLines = append(leftLines, shown)
	}
	commitRows := min(len(r.commits), max(0, (height-5)/3))
	if commitRows == 0 && height >= 10 {
		commitRows = 1
	}
	fileRows := max(1, height-4-commitRows)
	rightLines := []string{fmt.Sprintf("Changed files (%d)", len(r.files))}
	start := max(0, r.selected-fileRows+1)
	for i := start; i < len(r.files) && i < start+fileRows; i++ {
		f := r.files[i]
		prefix := "  "
		if i == r.selected {
			prefix = "› "
		}
		rightLines = append(rightLines, trim(prefix+f.status+" "+f.path, rightWidth))
	}
	for len(rightLines) < height-2-commitRows {
		rightLines = append(rightLines, "")
	}
	rightLines = append(rightLines, "Recent commits")
	if r.commitError != "" {
		rightLines = append(rightLines, trim(r.commitError, rightWidth))
	} else if len(r.commits) == 0 {
		rightLines = append(rightLines, "No commits yet")
	}
	for i := 0; i < commitRows && i < len(r.commits); i++ {
		rightLines = append(rightLines, trim(r.commits[i], rightWidth))
	}
	left := lipgloss.NewStyle().Width(leftWidth).Height(height - 1).Render(strings.Join(leftLines, "\n"))
	right := lipgloss.NewStyle().Width(rightWidth).Height(height - 1).Render(strings.Join(rightLines, "\n"))
	footer := "F3/Esc return  ·  Tab/←→ focus  ·  ↑↓ select/scroll  ·  PgUp/PgDn  ·  r refresh"
	v := tea.NewView(lipgloss.JoinHorizontal(lipgloss.Top, left, " │ ", right) + "\n" + trim(footer, width))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "Rurushu Code Review"
	return v
}
