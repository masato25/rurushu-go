package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/masato25/rurushu-go/duel"
	"github.com/masato25/rurushu-go/provider"
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
	width, height    int
	work             viewport.Model
	input            textarea.Model
	workLines        []string
	instructions     chan string
	updates          chan duelUpdate
	run              func()
	cancel           context.CancelFunc
	stopLoop         func()
	stopped          bool
	running          bool
	retrying         bool
	restartAfterDone bool
	status           string
	leftID, rightID  string
	modal            string
	aux              *tui.AuxiliaryWorkspace
	newSessions      func() (string, string, error)
	queueOwner       func(string) error
	setInitialGoal   func(goal string) error
	waitingForGoal   bool
	slashMatches     []tui.SlashCommand
	slashMatchIndex  int
	switchMode       string
	initialTail      bool
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
	m.updateSlashAutocomplete()
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
func (m *duelUI) setStopLoopHandler(fn func())                           { m.stopLoop = fn }
func (m *duelUI) setOwnerQueueHandler(fn func(string) error)             { m.queueOwner = fn }
func (m *duelUI) setInitialLeftHistory(history []provider.Message) {
	if len(history) == 0 {
		return
	}
	m.workLines = append(m.workLines, lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#6ED6AA")).Render("● CONTINUED FROM SINGLE · LEFT SESSION"))
	m.initialTail = true
	for _, message := range history {
		content := strings.TrimSpace(message.Content)
		if content == "" {
			continue
		}
		label := ""
		switch message.Role {
		case provider.RoleUser:
			label = "SINGLE · USER"
		case provider.RoleAssistant:
			label = "SINGLE · ASSISTANT"
		default:
			continue
		}
		m.appendLocalMarkdownOutput(label, content)
	}
}

func (m *duelUI) setInitialRightHistory(history []duelPersistedTurn) {
	for _, item := range history {
		if strings.TrimSpace(item.Summary) == "" {
			continue
		}
		entry := renderDuelTurn(duelUpdate{
			role:      "right",
			round:     item.Round,
			roundTime: item.Time,
			turn:      duel.Turn{Action: item.Action, Summary: item.Summary, Content: item.Summary},
		}, max(16, m.work.Width()))
		m.workLines = append(m.workLines, entry)
	}
	if len(history) > 0 {
		m.initialTail = true
		m.work.SetContent(strings.Join(m.workLines, "\n\n"))
		m.work.GotoBottom()
	}
}
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
	m.run()
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
		if m.initialTail {
			m.work.GotoBottom()
			m.initialTail = false
		}
	case duelUpdate:
		if v.active {
			m.running = true
			m.retrying = false
			if strings.EqualFold(v.role, "left") {
				// A queued owner instruction gets drained immediately after LEFT
				// starts. Reaching the next LEFT therefore means no forced
				// restart is needed if the current run later completes.
				m.restartAfterDone = false
			}
			m.status = fmt.Sprintf("%s 執行中 · ROUND %02d", strings.ToUpper(v.role), v.round)
			return m, awaitDuelUpdate(m.updates)
		}
		if v.retry {
			m.running = true
			m.retrying = true
			m.status = fmt.Sprintf("%s session 發生錯誤：%v · %s 後自動重試一次", strings.ToUpper(v.role), v.err, v.retryAfter)
			return m, awaitDuelUpdate(m.updates)
		}
		if v.done {
			m.running = false
			m.retrying = false
			if m.restartAfterDone {
				m.restartAfterDone = false
				m.stopped = false
				m.status = "已收到主人指令，重新啟動 LEFT"
				m.run()
				return m, awaitDuelUpdate(m.updates)
			}
			// Notify the owner once when both agents request review.
			// Use Bubble Tea's raw terminal output to avoid corrupting the TUI.
			alert := v.err == nil && v.result.Status == "owner_review" && !m.stopped
			m.stopped = true
			if errors.Is(v.err, context.Canceled) {
				m.status = "LEFT/RIGHT loop 已停止 · 可用 /retry 或 Ctrl+R 重新啟動"
			} else if v.err != nil {
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
		m.updateSlashAutocomplete()
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
		if len(m.slashMatches) > 0 {
			switch v.String() {
			case "up":
				m.slashMatchIndex = (m.slashMatchIndex - 1 + len(m.slashMatches)) % len(m.slashMatches)
				return m, nil
			case "down":
				m.slashMatchIndex = (m.slashMatchIndex + 1) % len(m.slashMatches)
				return m, nil
			case "tab":
				m.completeSlashAutocomplete()
				return m, nil
			case "enter":
				if !m.currentSlashCommandIsExact() {
					m.completeSlashAutocomplete()
					return m, nil
				}
			}
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
			m.updateSlashAutocomplete()
			m.status = "已清空目前輸入內容"
			return m, m.input.Focus()
		case "enter":
			note := strings.TrimSpace(m.input.Value())
			if note != "" {
				if handled, cmd := m.runSlashCommand(note); handled {
					m.input.Reset()
					m.updateSlashAutocomplete()
					return m, cmd
				}
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
					m.updateSlashAutocomplete()
					m.waitingForGoal = false
					m.stopped = false
					m.input.Placeholder = "主人輸入指令；思考中也可 Enter 預排，完成後自動接續"
					m.status = "已設定 goal，開始左右互搏"
					m.run()
					return m, awaitDuelUpdate(m.updates)
				}
				if isDuelNewSessionCommand(note) {
					m.input.Reset()
					m.updateSlashAutocomplete()
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
					m.run()
					return m, awaitDuelUpdate(m.updates)
				}
				if m.queueOwner != nil {
					if err := m.queueOwner(note); err != nil {
						m.status = "保存主人指令失敗：" + err.Error()
						return m, m.input.Focus()
					}
				}
				select {
				case m.instructions <- note:
					m.appendOwnerInstruction(note)
					m.status = fmt.Sprintf("主人指令已保存並預排 · 待 LEFT 消費 %d 則", len(m.instructions))
					if m.running {
						m.restartAfterDone = true
					}
				default:
					m.status = "指令佇列已滿，請稍後再試"
				}
				m.input.Reset()
				m.updateSlashAutocomplete()
				if m.retrying && m.stopLoop != nil {
					m.restartAfterDone = true
					m.stopLoop()
					m.status = "已收到主人指令，停止舊 retry 後立即重啟 LEFT"
					m.input.Reset()
					m.updateSlashAutocomplete()
					return m, awaitDuelUpdate(m.updates)
				}
				if !m.running {
					m.stopped = false
					m.status = "已收到主人指令，立即啟動 LEFT"
					m.run()
					return m, awaitDuelUpdate(m.updates)
				}
			}
			return m, m.input.Focus()
		case "ctrl+r":
			if m.stopped {
				m.stopped = false
				m.status = "重試左右互搏中"
				m.run()
				return m, awaitDuelUpdate(m.updates)
			}
		case "pgup", "pgdown", "home", "end":
			m.work, _ = m.work.Update(message)
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(message)
		m.updateSlashAutocomplete()
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

func duelSlashCommands() []tui.SlashCommand {
	return []tui.SlashCommand{
		{Name: "exit", Usage: "/exit", Description: "exit the duel TUI"},
		{Name: "f2", Usage: "/f2", Description: "open setup (same as F2)"},
		{Name: "f3", Usage: "/f3", Description: "open review (same as F3)"},
		{Name: "f4", Usage: "/f4", Description: "open terminal (same as F4)"},
		{Name: "f5", Usage: "/f5", Description: "open project browser (same as F5)"},
		{Name: "help", Usage: "/help", Description: "show available duel commands"},
		{Name: "new", Usage: "/new", Description: "replace both LEFT/RIGHT sessions after duel stops"},
		{Name: "retry", Usage: "/retry", Description: "retry a stopped duel (same as Ctrl+R)"},
		{Name: "single", Usage: "/single", Description: "stop duel and switch to single chat mode"},
		{Name: "stop", Usage: "/stop", Description: "stop the LEFT/RIGHT loop but keep the duel TUI open"},
	}
}

func duelSlashQuery(input string) (string, bool) {
	input = strings.TrimLeft(input, " \t")
	if !strings.HasPrefix(input, "/") || strings.HasPrefix(input, "//") {
		return "", false
	}
	body := strings.TrimPrefix(input, "/")
	if strings.ContainsAny(body, " \t\r\n") {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(body)), true
}

func (m *duelUI) updateSlashAutocomplete() {
	query, ok := duelSlashQuery(m.input.Value())
	if !ok {
		m.slashMatches = nil
		m.slashMatchIndex = 0
		return
	}
	matches := make([]tui.SlashCommand, 0)
	for _, command := range duelSlashCommands() {
		if strings.HasPrefix(command.Name, query) {
			matches = append(matches, command)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })
	m.slashMatches = matches
	if len(matches) == 0 {
		m.slashMatchIndex = 0
	} else if m.slashMatchIndex >= len(matches) {
		m.slashMatchIndex = len(matches) - 1
	}
}

func (m *duelUI) currentSlashCommandIsExact() bool {
	query, ok := duelSlashQuery(m.input.Value())
	if !ok || query == "" {
		return false
	}
	for _, command := range duelSlashCommands() {
		if command.Name == query {
			return true
		}
	}
	return false
}

func (m *duelUI) completeSlashAutocomplete() {
	if len(m.slashMatches) == 0 {
		return
	}
	idx := min(max(0, m.slashMatchIndex), len(m.slashMatches)-1)
	m.input.SetValue("/" + m.slashMatches[idx].Name)
	m.input.CursorEnd()
	m.updateSlashAutocomplete()
}

func (m *duelUI) renderSlashAutocomplete(width int) string {
	if len(m.slashMatches) == 0 || width <= 0 {
		return ""
	}
	const maxVisible = 5
	limit := min(maxVisible, len(m.slashMatches))
	start := 0
	if m.slashMatchIndex >= limit {
		start = m.slashMatchIndex - limit + 1
	}
	end := min(len(m.slashMatches), start+limit)
	var b strings.Builder
	for i := start; i < end; i++ {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		command := m.slashMatches[i]
		marker := "  "
		style := lipgloss.NewStyle().Foreground(lipgloss.Color("#7F8797"))
		if i == m.slashMatchIndex {
			marker = "› "
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("#75AFFF")).Bold(true)
		}
		line := command.Usage
		if command.Description != "" {
			line += " — " + command.Description
		}
		b.WriteString(style.Render(truncateDuelText(marker+line, width)))
	}
	return b.String()
}

func truncateDuelText(text string, width int) string {
	if width <= 0 || lipgloss.Width(text) <= width {
		return text
	}
	r := []rune(text)
	for len(r) > 0 && lipgloss.Width(string(r)+"…") > width {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func (m *duelUI) runSlashCommand(input string) (bool, tea.Cmd) {
	query, ok := duelSlashQuery(input)
	if !ok || query == "" {
		return false, nil
	}
	switch query {
	case "exit":
		m.cancel()
		return true, tea.Quit
	case "f2":
		return true, func() tea.Msg { return tea.KeyPressMsg(tea.Key{Code: tea.KeyF2}) }
	case "f3":
		return true, func() tea.Msg { return tea.KeyPressMsg(tea.Key{Code: tea.KeyF3}) }
	case "f4":
		return true, func() tea.Msg { return tea.KeyPressMsg(tea.Key{Code: tea.KeyF4}) }
	case "f5":
		return true, func() tea.Msg { return tea.KeyPressMsg(tea.Key{Code: tea.KeyF5}) }
	case "help":
		var lines []string
		for _, command := range duelSlashCommands() {
			lines = append(lines, command.Usage+" — "+command.Description)
		}
		m.appendLocalOutput("DUEL COMMANDS", strings.Join(lines, "\n"))
		return true, m.input.Focus()
	case "new":
		return false, nil
	case "retry":
		if !m.stopped {
			m.status = "duel 尚在執行中，不需要 retry"
			return true, m.input.Focus()
		}
		m.stopped = false
		m.status = "重試左右互搏中"
		m.run()
		return true, awaitDuelUpdate(m.updates)
	case "stop":
		if m.stopped {
			m.status = "duel loop 已停止"
			return true, m.input.Focus()
		}
		if m.stopLoop != nil {
			m.stopLoop()
		}
		m.status = "正在停止 LEFT/RIGHT loop…"
		return true, awaitDuelUpdate(m.updates)
	case "single":
		if m.stopLoop != nil {
			m.stopLoop()
		}
		m.switchMode = "single"
		m.status = "切換到 single chat"
		return true, tea.Quit
	default:
		m.status = "未知 slash command：/" + query + " · 輸入 /help 查看可用指令"
		return true, m.input.Focus()
	}
}

func (m *duelUI) appendLocalOutput(title, content string) {
	label := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#75AFFF")).Render("● " + title)
	divider := lipgloss.NewStyle().Foreground(lipgloss.Color("#515968")).Render(strings.Repeat("─", max(1, min(max(16, m.width-2)-3, 42))))
	m.workLines = append(m.workLines, label+"\n"+divider+"\n"+strings.TrimSpace(content))
	m.work.SetContent(strings.Join(m.workLines, "\n\n"))
	m.work.GotoBottom()
}

func (m *duelUI) appendLocalMarkdownOutput(title, content string) {
	width := max(20, m.work.Width()-4)
	m.appendLocalOutput(title, tui.RenderMarkdown(strings.TrimSpace(content), width))
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
	markdown = tui.RenderMarkdown(markdown, max(20, width-5))
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
	if suggestions := m.renderSlashAutocomplete(max(1, m.width-4)); suggestions != "" {
		footer = "\n" + status + "\n" + m.input.View() + "\n" + suggestions + "\n↑↓ 選擇 · Tab/Enter 補全 · /help 指令列表 · Shift+Enter 換行"
	}
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
