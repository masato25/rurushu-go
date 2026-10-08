package tui

import (
	"context"
	"math"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/arborlogic/rurushu-go/permission"
	"github.com/arborlogic/rurushu-go/provider"
)

type Message struct {
	Role     string
	Content  string
	Local    bool
	Activity *activityEntry
}

type Streamer interface {
	Stream(context.Context, provider.CompletionRequest) (<-chan provider.StreamEvent, error)
}

type Model struct {
	width  int
	height int

	frame frameLayout

	viewport viewport.Model
	composer textarea.Model
	messages []Message
	history  []provider.Message

	modelName string
	provider  string
	llm       Streamer

	permissionRequests <-chan *permission.Prompt
	pendingPermission  *permission.Prompt

	streaming           bool
	stream              <-chan provider.StreamEvent
	cancelStream        context.CancelFunc
	streamSeq           uint64
	activeStream        uint64
	followStream        bool
	toolActivity        string
	streamAssistantText string
	streamReasoningText string
	activityMode        ActivityMode
	thinkingVisible     bool
	slashCommands       map[string]SlashCommand
}

type streamStartedMsg struct {
	id     uint64
	stream <-chan provider.StreamEvent
	err    error
}

type streamEventMsg struct {
	id     uint64
	event  provider.StreamEvent
	ok     bool
	stream <-chan provider.StreamEvent
}

type permissionPromptMsg struct {
	prompt *permission.Prompt
	closed bool
}

const (
	roleUser      = "user"
	roleAssistant = "assistant"
	roleActivity  = "activity"
)

func New(modelName, provider string) *Model {
	return NewWithStreamer(modelName, provider, nil)
}

func NewWithStreamer(modelName, providerID string, llm Streamer) *Model {
	composer := textarea.New()
	composer.Placeholder = "type a task…"
	composer.ShowLineNumbers = false
	composer.CharLimit = 20000
	composer.DynamicHeight = true
	composer.MinHeight = 1
	composer.MaxHeight = 4
	composer.MaxContentHeight = 40
	composer.SetHeight(1)
	composer.SetPromptFunc(2, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return "❯ "
		}
		return "  "
	})
	keyMap := textarea.DefaultKeyMap()
	keyMap.InsertNewline.SetKeys("shift+enter", "ctrl+j", "alt+enter")
	composer.KeyMap = keyMap
	composer.Focus()

	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	vp.SoftWrap = true
	vp.FillHeight = true
	vp.MouseWheelEnabled = true

	m := &Model{
		viewport:     vp,
		composer:     composer,
		modelName:    modelName,
		provider:     providerID,
		llm:          llm,
		activityMode: ActivityNormal,
	}
	m.registerDefaultSlashCommands()
	m.refreshConversation()
	return m
}

func (m *Model) SetInitialMessage(content string) {
	if value := strings.TrimSpace(content); value != "" {
		m.messages = append(m.messages, Message{Role: roleAssistant, Content: value, Local: true})
		m.refreshConversation()
	}
}

func (m *Model) SetInitialInput(content string) {
	if value := strings.TrimSpace(content); value != "" {
		m.composer.SetValue(value)
		m.layout()
	}
}

func (m *Model) SetPermissionRequests(requests <-chan *permission.Prompt) {
	m.permissionRequests = requests
}

func (m *Model) SetActivityMode(mode ActivityMode) {
	if mode == "" {
		mode = ActivityNormal
	}
	m.activityMode = mode
	m.refreshConversation()
}

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.composer.Focus()}
	if m.permissionRequests != nil {
		cmds = append(cmds, waitForPermission(m.permissionRequests))
	}
	return tea.Batch(cmds...)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil

	case streamStartedMsg:
		if msg.id != m.activeStream || !m.streaming {
			return m, nil
		}
		if msg.err != nil {
			m.finishStreamWithError(msg.err)
			return m, nil
		}
		m.stream = msg.stream
		return m, waitForStreamEvent(msg.id, msg.stream)

	case permissionPromptMsg:
		if msg.closed {
			m.permissionRequests = nil
			return m, nil
		}
		if msg.prompt == nil {
			if m.permissionRequests != nil {
				return m, waitForPermission(m.permissionRequests)
			}
			return m, nil
		}
		m.pendingPermission = msg.prompt
		m.composer.Blur()
		m.layout()
		return m, nil

	case streamEventMsg:
		if msg.id != m.activeStream || !m.streaming {
			return m, nil
		}
		if !msg.ok {
			m.finishStream()
			return m, nil
		}
		follow := m.followStream
		offset := m.viewport.YOffset()
		done := m.applyStreamEvent(msg.event)
		m.refreshConversation()
		if follow {
			m.viewport.GotoBottom()
		} else {
			maxOffset := max(0, m.viewport.TotalLineCount()-m.viewport.VisibleLineCount())
			m.viewport.SetYOffset(min(offset, maxOffset))
		}
		if done {
			return m, nil
		}
		return m, waitForStreamEvent(msg.id, msg.stream)

	case tea.KeyPressMsg:
		if m.pendingPermission != nil {
			switch msg.String() {
			case "ctrl+c":
				m.resolvePermission(false)
				return m, tea.Quit
			case "y":
				m.resolvePermission(true)
				if m.permissionRequests != nil {
					return m, waitForPermission(m.permissionRequests)
				}
				return m, nil
			case "n", "enter", "esc":
				m.resolvePermission(false)
				if m.permissionRequests != nil {
					return m, waitForPermission(m.permissionRequests)
				}
				return m, nil
			default:
				return m, nil
			}
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.streaming {
				m.cancelActiveStream()
				return m, nil
			}
		case "pageup":
			m.viewport.PageUp()
			if m.streaming {
				m.followStream = m.viewport.AtBottom()
			}
			return m, nil
		case "pagedown":
			m.viewport.PageDown()
			if m.streaming {
				m.followStream = m.viewport.AtBottom()
			}
			return m, nil
		case "enter":
			if m.streaming {
				return m, nil
			}
			value := strings.TrimSpace(m.composer.Value())
			if value == "" {
				return m, nil
			}
			m.composer.Reset()
			if m.runSlashCommand(value) {
				m.refreshConversation()
				m.layout()
				m.viewport.GotoBottom()
				return m, nil
			}
			value = unescapeSlashPrompt(value)
			m.messages = append(m.messages, Message{Role: roleUser, Content: value})

			if m.llm != nil {
				m.history = append(m.history, provider.Message{Role: provider.RoleUser, Content: value})
				m.streamAssistantText = ""
				m.streamReasoningText = ""
				m.thinkingVisible = false
				m.messages = append(m.messages, Message{Role: roleAssistant})
				m.streaming = true
				m.streamSeq++
				m.activeStream = m.streamSeq
				m.followStream = true
				ctx, cancel := context.WithCancel(context.Background())
				m.cancelStream = cancel
				req := m.completionRequest()
				m.refreshConversation()
				m.layout()
				m.viewport.GotoBottom()
				return m, startStream(m.activeStream, ctx, m.llm, req)
			}

			m.refreshConversation()
			m.layout()
			m.viewport.GotoBottom()
			return m, nil
		}
	}

	beforeHeight := m.composer.Height()
	var composerCmd tea.Cmd
	m.composer, composerCmd = m.composer.Update(msg)
	if composerCmd != nil {
		cmds = append(cmds, composerCmd)
	}
	if beforeHeight != m.composer.Height() {
		m.layout()
	}

	beforeOffset := m.viewport.YOffset()
	var viewportCmd tea.Cmd
	m.viewport, viewportCmd = m.viewport.Update(msg)
	if viewportCmd != nil {
		cmds = append(cmds, viewportCmd)
	}
	if m.streaming && m.viewport.YOffset() != beforeOffset {
		m.followStream = m.viewport.AtBottom()
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) View() tea.View {
	if m.width <= 0 || m.height <= 0 {
		v := tea.NewView("starting…")
		v.AltScreen = true
		return v
	}

	parts := make([]string, 0, 4)
	if m.frame.ShowStatus {
		parts = append(parts, m.statusView())
	}
	if m.frame.GapHeight > 0 {
		parts = append(parts, "")
	}
	if m.frame.Transcript.Height > 0 {
		parts = append(parts, m.viewport.View())
	}
	parts = append(parts, m.inputView())

	workbench := strings.Join(parts, "\n")
	workbench = lipgloss.NewStyle().
		Width(m.frame.Workspace.Width).
		Height(m.frame.Workspace.Height).
		Render(workbench)
	content := placeWorkspace(m.frame.Root, m.frame.Workspace, workbench)

	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "Rurushu"
	v.KeyboardEnhancements.ReportAlternateKeys = true
	v.KeyboardEnhancements.ReportAllKeysAsEscapeCodes = true
	v.KeyboardEnhancements.ReportAssociatedText = true

	if m.pendingPermission == nil {
		if cursor := m.composer.Cursor(); cursor != nil {
			cursor.X += m.frame.Composer.X
			cursor.Y += m.frame.Composer.Y
			v.Cursor = cursor
		}
	}
	return v
}

func (m *Model) inputView() string {
	if m.pendingPermission == nil {
		return m.composer.View()
	}
	width := max(1, m.frame.Composer.Width)
	req := m.pendingPermission.Request
	target := strings.TrimSpace(req.Description)
	if target == "" {
		target = strings.TrimSpace(req.Pattern)
	}
	first := "allow " + req.Tool
	if target != "" {
		first += " — " + target
	}
	first = truncate(first, width)
	if m.frame.Composer.Height <= 1 {
		return first
	}
	second := truncate("[y] allow  [n/enter/esc] deny", width)
	return first + "\n" + mutedStyle.Render(second)
}

func placeWorkspace(root, workspace rect, content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) > root.Height {
		lines = lines[:root.Height]
	}
	for len(lines) < root.Height {
		lines = append(lines, "")
	}

	left := strings.Repeat(" ", max(0, workspace.X))
	for idx := range lines {
		line := left + lines[idx]
		if width := lipgloss.Width(line); width < root.Width {
			line += strings.Repeat(" ", root.Width-width)
		}
		lines[idx] = line
	}
	return strings.Join(lines, "\n")
}

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}

	wasAtBottom := m.viewport.AtBottom()
	scrollPercent := m.viewport.ScrollPercent()

	// Pass 1 computes the responsive chrome budget and therefore the maximum
	// height the composer is allowed to consume at this terminal size.
	desiredComposerHeight := m.composer.Height()
	if m.pendingPermission != nil {
		desiredComposerHeight = 2
	}
	frame := computeFrameLayout(m.width, m.height, desiredComposerHeight)
	m.composer.MaxHeight = frame.MaxComposerHeight
	m.composer.SetWidth(frame.Workspace.Width)

	// SetWidth can change DynamicHeight because soft wrapping changed. Pass 2
	// uses that actual textarea height to produce the final child rectangles.
	desiredComposerHeight = m.composer.Height()
	if m.pendingPermission != nil {
		desiredComposerHeight = 2
	}
	m.frame = computeFrameLayout(m.width, m.height, desiredComposerHeight)
	m.viewport.SetWidth(m.frame.Transcript.Width)
	m.viewport.SetHeight(m.frame.Transcript.Height)
	m.refreshConversation()

	if m.frame.Transcript.Height > 0 {
		if wasAtBottom {
			m.viewport.GotoBottom()
		} else {
			maxOffset := max(0, m.viewport.TotalLineCount()-m.viewport.VisibleLineCount())
			m.viewport.SetYOffset(int(math.Round(scrollPercent * float64(maxOffset))))
		}
	}
}

func (m *Model) refreshConversation() {
	width := m.viewport.Width()
	if width <= 0 {
		width = 80
	}
	inner := max(1, width-2)

	var b strings.Builder
	if len(m.messages) == 0 {
		b.WriteString(idleStyle.Render("workspace ready"))
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render("describe a change, ask a question, or start a task"))
	} else {
		for idx, message := range m.messages {
			if idx > 0 {
				previous := m.messages[idx-1]
				if message.Role == roleActivity || previous.Role == roleActivity {
					b.WriteString("\n")
				} else {
					b.WriteString("\n\n")
				}
			}
			if message.Role == roleUser {
				b.WriteString(renderTranscript("›", message.Content, inner, userMarkerStyle))
			} else if message.Role == roleActivity {
				b.WriteString(renderActivity(message.Activity, m.activityMode, inner))
			} else {
				content := message.Content
				if content == "" && m.streaming && idx == len(m.messages)-1 {
					content = "…"
				}
				b.WriteString(renderTranscript("·", content, inner, agentMarkerStyle))
			}
		}
	}
	m.viewport.SetContent(b.String())
}

func (m *Model) statusView() string {
	width := m.frame.Status.Width
	if width <= 0 {
		return ""
	}
	if width < 18 {
		return truncate(statusNameStyle.Render("rurushu"), width)
	}
	model := compactModel(m.provider, m.modelName, max(1, width-16))
	state := ""
	if m.toolActivity != "" {
		state = "  " + m.toolActivity
	} else if m.streaming {
		state = "  streaming"
	}
	details := truncate(model+state, max(1, width-7))
	return statusNameStyle.Render("rurushu") + mutedStyle.Render("  "+details)
}

func (m *Model) completionRequest() provider.CompletionRequest {
	return provider.CompletionRequest{
		Model:    m.modelName,
		Messages: append([]provider.Message(nil), m.history...),
	}
}

func startStream(id uint64, ctx context.Context, llm Streamer, req provider.CompletionRequest) tea.Cmd {
	return func() tea.Msg {
		stream, err := llm.Stream(ctx, req)
		return streamStartedMsg{id: id, stream: stream, err: err}
	}
}

func waitForStreamEvent(id uint64, stream <-chan provider.StreamEvent) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-stream
		return streamEventMsg{id: id, event: event, ok: ok, stream: stream}
	}
}

func waitForPermission(requests <-chan *permission.Prompt) tea.Cmd {
	return func() tea.Msg {
		prompt, ok := <-requests
		if !ok {
			return permissionPromptMsg{closed: true}
		}
		return permissionPromptMsg{prompt: prompt}
	}
}

func (m *Model) resolvePermission(approved bool) {
	if m.pendingPermission == nil {
		return
	}
	m.pendingPermission.Resolve(approved)
	m.pendingPermission = nil
	m.composer.Focus()
	m.layout()
}

func (m *Model) applyStreamEvent(event provider.StreamEvent) bool {
	switch event.Type {
	case provider.EventToken:
		m.ensureAssistantMessage()
		m.messages[len(m.messages)-1].Content += event.Text
		m.streamAssistantText += event.Text
		m.thinkingVisible = false
		m.toolActivity = ""
	case provider.EventReasoning:
		m.streamReasoningText += event.ReasoningText
		if !m.thinkingVisible {
			m.appendActivity(&activityEntry{Kind: "thinking"})
			m.thinkingVisible = true
		}
	case provider.EventToolCall:
		m.history = append(m.history, provider.Message{
			Role:             provider.RoleAssistant,
			Content:          m.streamAssistantText,
			ReasoningContent: m.streamReasoningText,
			ToolCalls:        append([]provider.ToolCall(nil), event.ToolCalls...),
		})
		m.streamAssistantText = ""
		m.streamReasoningText = ""
	case provider.EventToolStart:
		if event.ToolExecution != nil {
			m.toolActivity = "tool " + event.ToolExecution.Tool
			m.thinkingVisible = false
			m.appendActivity(&activityEntry{Kind: "tool", ID: event.ToolExecution.ID, Tool: event.ToolExecution.Tool, Args: event.ToolExecution.Args})
		}
	case provider.EventToolResult:
		m.toolActivity = ""
		if event.ToolExecution != nil {
			m.finishToolActivity(event.ToolExecution)
			m.history = append(m.history, provider.Message{
				Role:       provider.RoleTool,
				ToolCallID: event.ToolExecution.ID,
				Name:       event.ToolExecution.Tool,
				Content:    event.ToolExecution.Output,
			})
		}
	case provider.EventUsage:
		if m.activityMode == ActivityDebug && event.Usage != nil {
			usage := *event.Usage
			m.appendActivity(&activityEntry{Kind: "usage", Usage: &usage, Done: true})
		}
	case provider.EventContextCompact:
		if m.activityMode == ActivityDebug && event.Compaction != nil {
			compact := *event.Compaction
			m.appendActivity(&activityEntry{Kind: "compact", Compact: &compact, Done: true})
		}
	case provider.EventError:
		m.finishStreamWithError(event.Error)
		return true
	case provider.EventDone:
		m.commitAssistantHistory()
		m.finishStream()
		return true
	}
	return false
}

func (m *Model) ensureAssistantMessage() {
	if len(m.messages) > 0 && m.messages[len(m.messages)-1].Role == roleAssistant {
		return
	}
	m.messages = append(m.messages, Message{Role: roleAssistant})
}

func (m *Model) appendActivity(activity *activityEntry) {
	entry := Message{Role: roleActivity, Local: true, Activity: activity}
	if len(m.messages) > 0 {
		last := len(m.messages) - 1
		if m.messages[last].Role == roleAssistant && strings.TrimSpace(m.messages[last].Content) == "" {
			m.messages[last] = entry
			return
		}
	}
	m.messages = append(m.messages, entry)
}

func (m *Model) finishToolActivity(info *provider.ToolExecutionInfo) {
	for i := len(m.messages) - 1; i >= 0; i-- {
		activity := m.messages[i].Activity
		if m.messages[i].Role != roleActivity || activity == nil || activity.Kind != "tool" {
			continue
		}
		if activity.ID == info.ID || (activity.ID == "" && activity.Tool == info.Tool && !activity.Done) {
			activity.Output = info.Output
			activity.IsError = info.IsError
			activity.Done = true
			return
		}
	}
	m.appendActivity(&activityEntry{Kind: "tool", ID: info.ID, Tool: info.Tool, Args: info.Args, Output: info.Output, IsError: info.IsError, Done: true})
}

func (m *Model) commitAssistantHistory() {
	if m.streamAssistantText == "" && m.streamReasoningText == "" {
		return
	}
	m.history = append(m.history, provider.Message{
		Role:             provider.RoleAssistant,
		Content:          m.streamAssistantText,
		ReasoningContent: m.streamReasoningText,
	})
	m.streamAssistantText = ""
	m.streamReasoningText = ""
}

func (m *Model) finishStream() {
	if m.cancelStream != nil {
		m.cancelStream()
	}
	m.cancelStream = nil
	m.stream = nil
	m.streaming = false
	m.activeStream = 0
	m.followStream = false
	m.toolActivity = ""
	m.composer.Focus()
	m.refreshConversation()
}

func (m *Model) finishStreamWithError(err error) {
	message := "request failed"
	if err != nil {
		message = "error: " + err.Error()
	}
	if len(m.messages) > 0 && m.messages[len(m.messages)-1].Role == roleAssistant {
		content := strings.TrimSpace(m.messages[len(m.messages)-1].Content)
		if content == "" {
			m.messages[len(m.messages)-1].Content = message
		} else {
			m.messages[len(m.messages)-1].Content = content + "\n\n" + message
		}
	} else {
		m.messages = append(m.messages, Message{Role: roleAssistant, Content: message})
	}
	m.finishStream()
}

func (m *Model) cancelActiveStream() {
	if !m.streaming {
		return
	}
	if m.cancelStream != nil {
		m.cancelStream()
	}
	if len(m.messages) > 0 {
		last := len(m.messages) - 1
		if m.messages[last].Role == roleAssistant && strings.TrimSpace(m.messages[last].Content) == "" {
			m.messages = m.messages[:last]
		}
	}
	m.cancelStream = nil
	m.stream = nil
	m.streaming = false
	m.activeStream = 0
	m.followStream = false
	m.toolActivity = ""
	m.composer.Focus()
	m.refreshConversation()
}

func renderTranscript(marker, content string, width int, markerStyle lipgloss.Style) string {
	bodyWidth := max(1, width-2)
	body := lipgloss.NewStyle().Width(bodyWidth).Render(content)
	lines := strings.Split(body, "\n")
	for idx := range lines {
		prefix := "  "
		if idx == 0 {
			prefix = markerStyle.Render(marker) + " "
		}
		lines[idx] = prefix + lines[idx]
	}
	return strings.Join(lines, "\n")
}

func compactModel(provider, model string, maxWidth int) string {
	value := model
	if provider != "" {
		value = provider + "/" + model
	}
	return truncate(value, maxWidth)
}

func truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	var b strings.Builder
	used := 0
	for _, r := range value {
		rw := lipgloss.Width(string(r))
		if used+rw > width-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + "…"
}
