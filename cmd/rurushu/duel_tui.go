package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/masato25/rurushu-go/duel"
)

type duelUpdate struct {
	role      string
	round     int
	roundTime time.Time
	turn      duel.Turn
	err       error
	done      bool
	result    duel.Result
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
	modal                 string
}

type duelModalLayout struct {
	dialog     string
	x, y       int
	okX1, okX2 int
	okY        int
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
				m.status = "等待外部條件 · 可輸入主人指令繼續"
				m.modal = v.result.Summary
			} else if v.result.Status == "propose_review" {
				m.status = "RIGHT 建議驗收 · 自動輪詢已停止，可輸入主人指令繼續"
			} else if v.result.Status == "deadline_reached" {
				m.status = "輪詢期限已到 · 可輸入主人指令繼續"
			} else {
				m.status = "雙方同意，等待主人驗收：" + v.result.Summary
			}
			if alert {
				return m, tea.Batch(m.input.Focus(), tea.Raw("\a"))
			}
			return m, m.input.Focus()
		}
		entry := renderDuelTurn(v, max(16, (m.width-3)/2))
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
	case tea.MouseWheelMsg:
		if m.modal != "" {
			return m, nil
		}
		if v.Y >= 1 && v.Y < m.height-5 {
			if v.X < m.width/2 {
				m.left, _ = m.left.Update(v)
			} else {
				m.right, _ = m.right.Update(v)
			}
		}
		return m, nil
	case tea.MouseClickMsg:
		if m.modal != "" && v.Button == tea.MouseLeft {
			layout := m.modalLayout()
			if v.X >= layout.okX1 && v.X <= layout.okX2 && v.Y == layout.okY {
				m.modal = ""
				return m, m.input.Focus()
			}
		}
		return m, nil
	case tea.PasteMsg:
		if m.modal != "" {
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(v)
		return m, cmd
	case tea.KeyPressMsg:
		if m.modal != "" {
			switch v.String() {
			case "enter", "esc", "o":
				m.modal = ""
				return m, m.input.Focus()
			case "ctrl+c":
				m.cancel()
				return m, tea.Quit
			}
			return m, nil
		}
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
		case "pgup", "pgdown", "home", "end":
			m.left, _ = m.left.Update(message)
			m.right, _ = m.right.Update(message)
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(message)
		return m, cmd
	}
	return m, nil
}

func renderDuelTurn(update duelUpdate, width int) string {
	color := duelActionColor(update.turn.Action)
	roundLabel := fmt.Sprintf("● ROUND %02d", update.round)
	if !update.roundTime.IsZero() {
		roundLabel += " · " + update.roundTime.In(time.Local).Format("2006-01-02 15:04")
	}
	label := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(color)).Render(roundLabel + "  ·  " + strings.ToUpper(update.turn.Action))
	divider := lipgloss.NewStyle().Foreground(lipgloss.Color("#515968")).Render(strings.Repeat("─", max(1, min(width-3, 42))))
	markdown := strings.TrimSpace(update.turn.Summary)
	if evidence := strings.TrimSpace(update.turn.Evidence); evidence != "" {
		markdown += "\n\n**Evidence**\n\n" + evidence
	}
	renderer, err := glamour.NewTermRenderer(glamour.WithStyles(styles.DarkStyleConfig), glamour.WithWordWrap(max(1, width-5)))
	if err == nil {
		if rendered, renderErr := renderer.Render(markdown); renderErr == nil {
			markdown = strings.TrimSpace(rendered)
		}
	}
	return label + "\n" + divider + "\n" + markdown
}

func duelActionColor(action string) string {
	switch action {
	case "plan":
		return "#75AFFF"
	case "research":
		return "#C39BFF"
	case "continue":
		return "#6ED6AA"
	case "blocked":
		return "#E6A34B"
	case "propose_review", "agree_review":
		return "#E3C86F"
	}
	return "#7DAEA3"
}

func (m *duelUI) modalLayout() duelModalLayout {
	boxWidth := max(24, min(m.width-8, 82))
	innerWidth := max(1, boxWidth-6)
	ok := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#8DA9E8")).Render("[ OK ]")
	actionLine := lipgloss.PlaceHorizontal(innerWidth, lipgloss.Center, ok+"  Enter / Esc 關閉")
	dialog := lipgloss.NewStyle().Width(boxWidth).Padding(1, 2).Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#E6A34B")).Background(lipgloss.Color("#222831")).Render(
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E6A34B")).Render("⚠ 等待外部條件") + "\n\n" +
			lipgloss.NewStyle().Width(innerWidth).Render(m.modal) + "\n\n" + actionLine)
	dialogWidth := lipgloss.Width(dialog)
	dialogHeight := lipgloss.Height(dialog)
	x := max(0, (m.width-dialogWidth)/2)
	y := max(0, (m.height-dialogHeight)/2)
	// The action line is the last content row before bottom padding and border.
	okY := y + dialogHeight - 3
	okStartInLine := max(0, (innerWidth-lipgloss.Width(ok+"  Enter / Esc 關閉"))/2)
	// Account for the left border and two columns of horizontal padding.
	okX1 := x + 3 + okStartInLine
	return duelModalLayout{dialog: dialog, x: x, y: y, okX1: okX1, okX2: okX1 + lipgloss.Width(ok) - 1, okY: okY}
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
	result.AltScreen = true
	result.MouseMode = tea.MouseModeCellMotion
	if m.modal != "" {
		layout := m.modalLayout()
		canvas := lipgloss.NewCanvas(max(m.width, lipgloss.Width(layout.dialog)+2), max(m.height, 12))
		canvas.Compose(lipgloss.NewLayer(result.Content))
		canvas.Compose(lipgloss.NewLayer(layout.dialog).X(layout.x).Y(layout.y))
		result.Content = canvas.Render()
	}
	return result
}
