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
	"github.com/masato25/rurushu-go/tui"
)

type duelUpdate struct {
	role       string
	round      int
	roundTime  time.Time
	turn       duel.Turn
	err        error
	done       bool
	result     duel.Result
	retry      bool
	retryAfter time.Duration
	active     bool
}
type duelUI struct {
	width, height   int
	work            viewport.Model
	input           textarea.Model
	workLines       []string
	instructions    chan string
	updates         chan duelUpdate
	run             func()
	cancel          context.CancelFunc
	stopped         bool
	status          string
	leftID, rightID string
	modal           string
	aux             *tui.AuxiliaryWorkspace
	newSessions     func() (string, string, error)
	setInitialGoal  func(goal string) error
	waitingForGoal  bool
}

type duelModalLayout struct {
	dialog     string
	x, y       int
	okX1, okX2 int
	okY        int
}

func newDuelUI(instructions chan string, updates chan duelUpdate, run func(), cancel context.CancelFunc, sessionIDs ...string) *duelUI {
	w := viewport.New(viewport.WithWidth(80), viewport.WithHeight(15))
	w.SoftWrap = true
	w.MouseWheelEnabled = true
	t := textarea.New()
	t.Placeholder = "主人輸入指令；思考中也可 Enter 預排，完成後自動接續"
	t.ShowLineNumbers = false
	t.SetHeight(2)
	t.SetWidth(76)
	t.Focus()
	km := textarea.DefaultKeyMap()
	km.InsertNewline.SetKeys("shift+enter", "ctrl+j")
	t.KeyMap = km
	m := &duelUI{work: w, input: t, instructions: instructions, updates: updates, run: run, cancel: cancel, status: "duel 進行中"}
	if len(sessionIDs) > 0 {
		m.leftID = sessionIDs[0]
	}
	if len(sessionIDs) > 1 {
		m.rightID = sessionIDs[1]
	}
	return m
}

func (m *duelUI) setAuxiliaryWorkspace(aux *tui.AuxiliaryWorkspace)      { m.aux = aux }
func (m *duelUI) setNewSessionHandler(fn func() (string, string, error)) { m.newSessions = fn }
func (m *duelUI) setInitialGoalHandler(fn func(goal string) error) {
	m.setInitialGoal = fn
	m.waitingForGoal = true
	m.stopped = true
	m.status = "尚未設定 goal · 請直接輸入第一則目標後按 Enter"
	m.input.Placeholder = "輸入 duel goal，Enter 後建立 LEFT/RIGHT session 並開始"
}
func awaitDuelUpdate(ch <-chan duelUpdate) tea.Cmd { return func() tea.Msg { return <-ch } }
func (m *duelUI) Init() tea.Cmd {
	if m.waitingForGoal {
		return m.input.Focus()
	}
	go m.run()
	return tea.Batch(m.input.Focus(), awaitDuelUpdate(m.updates))
}
func (m *duelUI) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if m.aux != nil {
		if handled, cmd := m.aux.Update(message); handled {
			return m, cmd
		}
	}
	switch v := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
		m.input.SetWidth(max(1, m.width-4))
		h := max(3, m.height-7)
		m.work.SetWidth(max(16, m.width-2))
		m.work.SetHeight(h)
		m.work.SetContent(strings.Join(m.workLines, "\n\n"))
	case duelUpdate:
		if v.active {
			m.status = fmt.Sprintf("%s 執行中 · ROUND %02d", strings.ToUpper(v.role), v.round)
			return m, awaitDuelUpdate(m.updates)
		}
		if v.retry {
			m.status = fmt.Sprintf("%s session 發生錯誤：%v · %s 後自動重試一次", strings.ToUpper(v.role), v.err, v.retryAfter)
			return m, awaitDuelUpdate(m.updates)
		}
		if v.done {
			// Notify the owner once when both agents request review.
			// Use Bubble Tea's raw terminal output to avoid corrupting the TUI.
			alert := v.err == nil && v.result.Status == "owner_review" && !m.stopped
			m.stopped = true
			if v.err != nil {
				m.status = "停止：" + v.err.Error() + " · 可直接輸入指令或按 Ctrl+R 重試"
			} else if v.result.Status == "blocked" {
				m.status = "等待外部條件 · 可輸入主人指令繼續"
				m.modal = v.result.Summary
			} else if v.result.Status == "propose_review" {
				m.status = "RIGHT 建議驗收 · 自動輪詢已停止，可輸入主人指令繼續"
			} else if v.result.Status == "deadline_reached" {
				m.status = "輪詢期限已到 · 可輸入主人指令繼續"
			} else {
				m.status = "等待主人驗收 · 請直接在下方輸入指令後 Enter 繼續"
			}
			if alert {
				return m, tea.Batch(m.input.Focus(), tea.Raw("\a"))
			}
			return m, m.input.Focus()
		}
		entry := renderDuelTurn(v, max(16, m.width-2))
		m.workLines = append(m.workLines, entry)
		m.work.SetContent(strings.Join(m.workLines, "\n\n"))
		m.work.GotoBottom()
		return m, awaitDuelUpdate(m.updates)
	case tea.MouseWheelMsg:
		if m.modal != "" {
			return m, nil
		}
		if v.Y >= 1 && v.Y < m.height-5 {
			m.work, _ = m.work.Update(v)
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
		case "esc":
			if m.input.Value() == "" {
				return m, nil
			}
			m.input.Reset()
			m.status = "已清空目前輸入內容"
			return m, m.input.Focus()
		case "enter":
			note := strings.TrimSpace(m.input.Value())
			if note != "" {
				if m.waitingForGoal {
					if m.setInitialGoal == nil {
						m.status = "無法初始化 duel goal"
						return m, m.input.Focus()
					}
					if err := m.setInitialGoal(note); err != nil {
						m.status = "建立 duel 失敗：" + err.Error()
						return m, m.input.Focus()
					}
					m.input.Reset()
					m.waitingForGoal = false
					m.stopped = false
					m.input.Placeholder = "主人輸入指令；思考中也可 Enter 預排，完成後自動接續"
					m.status = "已設定 goal，開始左右互搏"
					go m.run()
					return m, awaitDuelUpdate(m.updates)
				}
				if isDuelNewSessionCommand(note) {
					m.input.Reset()
					if !m.stopped {
						m.status = "/new 只能在 duel 停止後使用，避免正在執行的 sessions 被替換"
						return m, m.input.Focus()
					}
					if m.newSessions == nil {
						m.status = "目前無法建立新的 duel sessions"
						return m, m.input.Focus()
					}
					leftID, rightID, err := m.newSessions()
					if err != nil {
						m.status = fmt.Sprintf("建立新 duel sessions 失敗：%v", err)
						return m, m.input.Focus()
					}
					m.leftID = leftID
					m.rightID = rightID
					m.stopped = false
					m.status = "已建立新的 LEFT/RIGHT sessions，接續目前 duel"
					go m.run()
					return m, awaitDuelUpdate(m.updates)
				}
				select {
				case m.instructions <- note:
					m.appendOwnerInstruction(note)
					m.status = fmt.Sprintf("主人指令已預排 · 待送 %d 則", len(m.instructions))
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
			m.work, _ = m.work.Update(message)
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(message)
		return m, cmd
	}
	return m, nil
}

func (m *duelUI) appendOwnerInstruction(note string) {
	note = strings.TrimSpace(note)
	if note == "" {
		return
	}
	label := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E3C86F")).Render("● OWNER · INSTRUCTION")
	divider := lipgloss.NewStyle().Foreground(lipgloss.Color("#515968")).Render(strings.Repeat("─", max(1, min(max(16, m.width-2)-3, 42))))
	entry := label + "\n" + divider + "\n" + note
	m.workLines = append(m.workLines, entry)
	m.work.SetContent(strings.Join(m.workLines, "\n\n"))
	m.work.GotoBottom()
}

func isDuelNewSessionCommand(input string) bool {
	return strings.EqualFold(strings.TrimSpace(input), "/new")
}

func renderDuelTurn(update duelUpdate, width int) string {
	color := duelActionColor(update.turn.Action)
	role := strings.ToUpper(strings.TrimSpace(update.role))
	if role == "" {
		role = "DUEL"
	}
	roundLabel := fmt.Sprintf("● %s · ROUND %02d", role, update.round)
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
	case "work":
		return "#6ED6AA"
	case "feedback":
		return "#75AFFF"
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
	if m.aux != nil {
		if view, ok := m.aux.View(); ok {
			return view
		}
	}
	headline := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E6B450"))
	title := " DUEL · 單一工作區 "
	if m.leftID != "" {
		title += "· LEFT " + m.leftID + " "
	}
	if m.rightID != "" {
		title += "· RIGHT " + m.rightID + " "
	}
	view := headline.Render(title) + "\n" + m.work.View()
	status := m.status
	if pending := len(m.instructions); pending > 0 && !strings.Contains(status, "待送") {
		status += fmt.Sprintf(" · 待送指令 %d 則", pending)
	}
	footer := "\n" + status + "\n" + m.input.View() + "\nF2 Setup · F3 Review · F4 Terminal · F5 Files · /new 換新 sessions · Enter 送出/預排 · Esc 清空輸入 · Ctrl+C 離開 · Ctrl+R 重試 · Shift+Enter 換行"
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
