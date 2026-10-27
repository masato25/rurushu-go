package main

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/masato25/rurushu-go/duel"
)

type duelUpdate struct {
	role   string
	round  int
	turn   duel.Turn
	err    error
	done   bool
	result duel.Result
}
type duelUI struct {
	width, height         int
	left, right           viewport.Model
	input                 textarea.Model
	leftLines, rightLines []string
	instructions          chan string
	updates               chan duelUpdate
	run                   func()
	cancel                context.CancelFunc
	stopped               bool
	status                string
	leftID, rightID       string
}

func newDuelUI(instructions chan string, updates chan duelUpdate, run func(), cancel context.CancelFunc, sessionIDs ...string) *duelUI {
	l := viewport.New(viewport.WithWidth(40), viewport.WithHeight(15))
	r := viewport.New(viewport.WithWidth(40), viewport.WithHeight(15))
	l.SoftWrap = true
	r.SoftWrap = true
	l.MouseWheelEnabled = true
	r.MouseWheelEnabled = true
	t := textarea.New()
	t.Placeholder = "主人輸入指令，Enter 送出；Ctrl+C 離開"
	t.ShowLineNumbers = false
	t.SetHeight(2)
	t.SetWidth(76)
	t.Focus()
	km := textarea.DefaultKeyMap()
	km.InsertNewline.SetKeys("shift+enter", "ctrl+j")
	t.KeyMap = km
	m := &duelUI{left: l, right: r, input: t, instructions: instructions, updates: updates, run: run, cancel: cancel, status: "左右互搏進行中"}
	if len(sessionIDs) > 0 {
		m.leftID = sessionIDs[0]
	}
	if len(sessionIDs) > 1 {
		m.rightID = sessionIDs[1]
	}
	return m
}
func awaitDuelUpdate(ch <-chan duelUpdate) tea.Cmd { return func() tea.Msg { return <-ch } }
func (m *duelUI) Init() tea.Cmd {
	go m.run()
	return tea.Batch(m.input.Focus(), awaitDuelUpdate(m.updates))
}
func (m *duelUI) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch v := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
		w := max(16, (m.width-3)/2)
		m.input.SetWidth(max(1, m.width-4))
		h := max(3, m.height-7)
		m.left.SetWidth(w)
		m.right.SetWidth(w)
		m.left.SetHeight(h)
		m.right.SetHeight(h)
		m.left.SetContent(strings.Join(m.leftLines, "\n\n"))
		m.right.SetContent(strings.Join(m.rightLines, "\n\n"))
	case duelUpdate:
		if v.done {
			// Notify the owner once when both agents request review.
			// Use Bubble Tea's raw terminal output to avoid corrupting the TUI.
			alert := v.err == nil && v.result.Status == "owner_review" && !m.stopped
			m.stopped = true
			if v.err != nil {
				m.status = "停止：" + v.err.Error()
			} else if v.result.Status == "blocked" {
				m.status = "等待外部條件／主人指令（Enter 送出後繼續）：" + v.result.Summary
			} else {
				m.status = "雙方同意，等待主人驗收：" + v.result.Summary
			}
			if alert {
				return m, tea.Batch(m.input.Focus(), tea.Raw("\a"))
			}
			return m, m.input.Focus()
		}
		entry := fmt.Sprintf("[%d · %s]\n%s\n%s", v.round, v.turn.Action, v.turn.Summary, v.turn.Evidence)
		if v.role == "left" {
			m.leftLines = append(m.leftLines, entry)
			m.left.SetContent(strings.Join(m.leftLines, "\n\n"))
			m.left.GotoBottom()
		} else {
			m.rightLines = append(m.rightLines, entry)
			m.right.SetContent(strings.Join(m.rightLines, "\n\n"))
			m.right.GotoBottom()
		}
		return m, awaitDuelUpdate(m.updates)
	case tea.KeyPressMsg:
		switch v.String() {
		case "ctrl+c":
			m.cancel()
			return m, tea.Quit
		case "enter":
			note := strings.TrimSpace(m.input.Value())
			if note != "" {
				select {
				case m.instructions <- note:
					m.status = "主人指令已排入下一個決策節點"
				default:
					m.status = "指令佇列已滿，請稍後再試"
				}
				m.input.Reset()
				if m.stopped {
					m.stopped = false
					m.status = "重試左右互搏中"
					go m.run()
					return m, awaitDuelUpdate(m.updates)
				}
			}
			return m, m.input.Focus()
		case "ctrl+r":
			if m.stopped {
				m.stopped = false
				m.status = "重試左右互搏中"
				go m.run()
				return m, awaitDuelUpdate(m.updates)
			}
		case "tab":
			m.left.GotoBottom()
			m.right.GotoBottom()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(message)
		return m, cmd
	}
	return m, nil
}
func (m *duelUI) View() tea.View {
	w := max(16, (m.width-3)/2)
	headline := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E6B450"))
	leftTitle := " LEFT · 執行者 "
	rightTitle := " RIGHT · 決策者 "
	if m.leftID != "" {
		leftTitle += "· " + m.leftID + " "
	}
	if m.rightID != "" {
		rightTitle += "· " + m.rightID + " "
	}
	left := headline.Render(leftTitle) + "\n" + m.left.View()
	right := headline.Render(rightTitle) + "\n" + m.right.View()
	view := lipgloss.JoinHorizontal(lipgloss.Top, lipgloss.NewStyle().Width(w).Render(left), " │ ", lipgloss.NewStyle().Width(w).Render(right))
	footer := "\n" + m.status + "\n" + m.input.View() + "\nCtrl+C 離開 · Ctrl+R 重試 · Shift+Enter 換行"
	result := tea.NewView(view + footer)
	return result
}
