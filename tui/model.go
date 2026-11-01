package tui

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	rurushuconfig "github.com/masato25/rurushu-go/config"
	"github.com/trentkm/spanreed"

	"github.com/masato25/rurushu-go/jobs"
	"github.com/masato25/rurushu-go/permission"
	"github.com/masato25/rurushu-go/projectstate"
	"github.com/masato25/rurushu-go/provider"
)

type Message struct {
	Role     string
	Content  string
	Local    bool
	Activity *activityEntry
}

type markdownCacheEntry struct {
	source   string
	width    int
	rendered string
}

type Streamer interface {
	Stream(context.Context, provider.CompletionRequest) (<-chan provider.StreamEvent, error)
}

type Model struct {
	visibleHistoryLimit  int
	lastTranscriptRender time.Time
	transcriptDirty      bool
	reloadRequested      bool
	modeSwitchRequested  string
	width                int
	height               int

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
	progressFrame       int
	markdownCache       map[int]markdownCacheEntry
	slashCommands       map[string]SlashCommand
	slashMatches        []SlashCommand
	slashMatchIndex     int
	selectionPicker     *selectionPicker
	pendingAttachments  []imageAttachment
	jobManager          *jobs.Manager
	runningJobs         int
	sessionStore        *projectstate.Store
	sessionID           string
	sessionSaveError    string
	setup               *SetupModel
	review              *reviewModel
	browser             *fileBrowser
	terminal            *terminalWorkspace
	terminalService     *TerminalService
	terminalVisible     bool
	queuedInputs        []string
	pastedContent       map[string]string
	pasteCounter        int
	watch               watchSchedule
	watchQueued         bool
	watchRunPending     bool
	watchFileModTime    time.Time
	watchFileSize       int64
}

func (m *Model) ReloadRequested() bool       { return m.reloadRequested }
func (m *Model) ModeSwitchRequested() string { return m.modeSwitchRequested }

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

type queuedSubmitMsg struct{}

type watchTickMsg struct {
	generation uint64
}

type watchSyncMsg struct{}

type permissionPromptMsg struct {
	prompt *permission.Prompt
	closed bool
}

type jobRefreshMsg struct{ running int }

type progressTickMsg struct{}

const streamRenderInterval = 100 * time.Millisecond

var progressFrames = []string{"◐", "◓", "◑", "◒"}

func progressTick() tea.Cmd {
	return tea.Tick(750*time.Millisecond, func(time.Time) tea.Msg { return progressTickMsg{} })
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
		visibleHistoryLimit: 120,
		viewport:            vp,
		composer:            composer,
		modelName:           modelName,
		provider:            providerID,
		llm:                 llm,
		activityMode:        ActivityNormal,
		pastedContent:       make(map[string]string),
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
		m.updateSlashAutocomplete()
		m.layout()
	}
}

func (m *Model) SetPermissionRequests(requests <-chan *permission.Prompt) {
	m.permissionRequests = requests
}

func (m *Model) SetJobManager(manager *jobs.Manager) {
	m.jobManager = manager
	m.registerJobSlashCommands()
	if manager != nil {
		m.runningJobs = manager.RunningCount()
	}
	m.refreshConversation()
}

func (m *Model) SetActivityMode(mode ActivityMode) {
	if mode == "" {
		mode = ActivityNormal
	}
	m.activityMode = mode
	m.refreshConversation()
}

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.composer.Focus(), progressTick()}
	if m.sessionStore != nil {
		cmds = append(cmds, watchSyncCmd())
	}
	if cmd := m.watchTimerCmd(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.permissionRequests != nil {
		cmds = append(cmds, waitForPermission(m.permissionRequests))
	}
	if m.jobManager != nil {
		cmds = append(cmds, refreshJobs(m.jobManager))
	}
	return tea.Batch(cmds...)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case spanreed.FrameMsg:
		if m.terminal != nil {
			return m, m.terminal.update(msg)
		}
		return m, nil
	case tea.MouseWheelMsg:
		if m.browser != nil {
			m.browser.wheel(msg.Button)
			return m, nil
		}
		if m.terminalVisible && m.terminal != nil {
			return m, m.terminal.update(msg)
		}
		if m.review != nil {
			return m, m.review.wheel(msg.Button)
		}
		if m.setup != nil {
			_, cmd := m.setup.Update(msg)
			return m, cmd
		}
	case reviewFilesMsg:
		if m.review == nil || msg.generation != m.review.generation {
			return m, nil
		}
		m.review.loading = false
		if msg.err != nil {
			m.review.err = msg.err.Error()
			return m, nil
		}
		m.review.files = msg.files
		m.review.commits = msg.commits
		m.review.commitError = msg.commitError
		m.review.selected = min(m.review.selected, max(0, len(msg.files)-1))
		return m, m.review.loadDiff()
	case reviewDiffMsg:
		if m.review == nil || msg.generation != m.review.generation {
			return m, nil
		}
		m.review.loading = false
		if msg.err != nil {
			m.review.err = msg.err.Error()
			return m, nil
		}
		m.review.diff = nil
		if msg.diff != "" {
			m.review.diff = strings.Split(strings.TrimSuffix(msg.diff, "\n"), "\n")
		}
		return m, nil
	case progressTickMsg:
		if m.transcriptDirty {
			m.flushTranscript()
		}
		if m.streaming {
			m.progressFrame = (m.progressFrame + 1) % len(progressFrames)
			// The status line animates independently; rebuilding the whole
			// transcript every tick scales badly with conversation length.
		}
		return m, progressTick()
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		if m.terminalVisible && m.terminal != nil {
			return m, m.terminal.resize(m.width, m.height)
		}
		if m.setup != nil {
			m.setup.Update(msg)
		}
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

	case jobRefreshMsg:
		m.runningJobs = msg.running
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
			if m.jobManager == nil {
				return jobRefreshMsg{}
			}
			return jobRefreshMsg{running: m.jobManager.RunningCount()}
		})

	case streamEventMsg:
		if msg.id != m.activeStream || !m.streaming {
			return m, nil
		}
		if !msg.ok {
			m.watchRunStatus("failed", "stream closed without completion")
			m.finishStream()
			return m, m.nextQueuedSubmitCmd()
		}
		follow := m.followStream
		offset := m.viewport.YOffset()
		done := m.applyStreamEvent(msg.event)
		if done || msg.event.Type == provider.EventError || msg.event.Type == provider.EventToolStart || msg.event.Type == provider.EventToolResult || time.Since(m.lastTranscriptRender) >= streamRenderInterval {
			m.refreshConversation()
			m.lastTranscriptRender = time.Now()
			m.transcriptDirty = false
		} else {
			m.transcriptDirty = true
		}
		if follow {
			m.viewport.GotoBottom()
		} else {
			maxOffset := max(0, m.viewport.TotalLineCount()-m.viewport.VisibleLineCount())
			m.viewport.SetYOffset(min(offset, maxOffset))
		}
		if done {
			return m, m.nextQueuedSubmitCmd()
		}
		return m, waitForStreamEvent(msg.id, msg.stream)

	case queuedSubmitMsg:
		if m.streaming || len(m.queuedInputs) == 0 {
			return m, nil
		}
		value := m.queuedInputs[0]
		m.queuedInputs = m.queuedInputs[1:]
		if m.watchQueued && value == m.watch.Prompt {
			m.watchQueued = false
			m.watchRunPending = true
			m.watch.RunStatus = "running"
			if err := m.persistWatchSchedule(); err != nil {
				m.sessionSaveError = err.Error()
			}
		}
		m.composer.SetValue(value)
		m.layout()
		return m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))

	case watchTickMsg:
		return m.handleWatchTick(msg)
	case watchSyncMsg:
		cmd := m.syncWatchSchedule()
		return m, tea.Batch(cmd, watchSyncCmd())

	case tea.PasteMsg:
		text := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(msg.Content, "\r\n", "\n"), "\r", "\n"))
		if text == "" {
			return m, nil
		}
		lineCount := strings.Count(text, "\n") + 1
		if lineCount >= 3 || len(text) > 200 {
			m.pasteCounter++
			marker := fmt.Sprintf("[Pasted ~%d lines #%d]", lineCount, m.pasteCounter)
			m.pastedContent[marker] = text
			beforeHeight := m.composer.Height()
			m.composer, _ = m.composer.Update(tea.PasteMsg{Content: marker + " "})
			m.updateSlashAutocomplete()
			if beforeHeight != m.composer.Height() {
				m.layout()
			}
			return m, nil
		}

	case tea.KeyPressMsg:
		key := msg.String()
		if m.browser != nil {
			if key == "f5" || (key == "esc" && m.browser.focusFiles) {
				m.browser = nil
				return m, nil
			}
			m.browser.key(key)
			return m, nil
		}
		if key == "f5" && m.pendingPermission == nil && m.review == nil && m.setup == nil && !m.terminalVisible {
			m.browser = newFileBrowser(m.terminalCWD())
			return m, nil
		}
		if m.terminalVisible && m.terminal != nil {
			if key == "f4" || (!m.terminal.focused && key == "esc") {
				m.terminal.terminal.Blur()
				m.terminal.focused = false
				m.terminalVisible = false
				return m, nil
			}
			return m, m.terminal.update(msg)
		}
		if key == "f4" && m.setup == nil && m.review == nil && m.pendingPermission == nil {
			if m.terminalService == nil {
				m.terminalService = NewTerminalService(m.terminalCWD())
			}
			terminal, cmd, err := m.terminalService.attach(m.width, m.height)
			if err != nil {
				m.sessionSaveError = err.Error()
				return m, nil
			}
			m.terminal = terminal
			m.terminalVisible = true
			resizeCmd := m.terminal.resize(m.width, m.height)
			return m, tea.Batch(cmd, resizeCmd)
		}
		if m.review != nil {
			switch key {
			case "f3", "esc":
				m.review = nil
				return m, nil
			case "ctrl+c":
				return m, tea.Quit
			}
			return m, m.review.key(key)
		}
		if key == "f3" && m.pendingPermission == nil && m.setup == nil {
			m.review = newReviewModel()
			return m, m.review.refresh()
		}
		if m.setup != nil {
			switch key {
			case "esc", "f2":
				m.setup = nil
				return m, m.watchTimerCmd()
			case "ctrl+c":
				return m, tea.Quit
			}
			if m.setup.saved {
				if key == "enter" {
					m.setup = nil
				}
				return m, nil
			}
			_, cmd := m.setup.Update(msg)
			return m, cmd
		}
		if key == "f2" && m.pendingPermission == nil {
			cfg, err := rurushuconfig.Load()
			if err != nil {
				m.sessionSaveError = err.Error()
				return m, nil
			}
			m.setup = NewSetupModel(cfg)
			m.setup.embedded = true
			if m.sessionStore != nil {
				_ = m.syncWatchSchedule()
				m.setup.EnableWatchEditor(m.watch.Active, m.watch.Interval, m.watch.Prompt, m.watch.NextRunAt, m.watch.LastRunAt, m.updateWatchFromSetup)
				m.setup.SetWatchRunStatus(m.watch.RunStatus, m.watch.CompletedAt, m.watch.SkippedCount, m.watch.RunError)
			}
			m.setup.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			return m, m.setup.Init()
		}
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
		if handled, command := m.handleSelectionPickerKey(msg); handled {
			return m, command
		}
		if !m.streaming && len(m.slashMatches) > 0 {
			switch msg.String() {
			case "up":
				m.slashMatchIndex = (m.slashMatchIndex - 1 + len(m.slashMatches)) % len(m.slashMatches)
				m.refreshConversation()
				return m, nil
			case "down":
				m.slashMatchIndex = (m.slashMatchIndex + 1) % len(m.slashMatches)
				m.refreshConversation()
				return m, nil
			case "tab":
				m.completeSlashAutocomplete()
				m.layout()
				m.viewport.GotoBottom()
				return m, nil
			case "enter":
				if !m.currentSlashCommandIsExact() {
					m.completeSlashAutocomplete()
					m.layout()
					m.viewport.GotoBottom()
					return m, nil
				}
			}
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.composer.Value() != "" {
				m.composer.Reset()
				m.pastedContent = make(map[string]string)
				m.updateSlashAutocomplete()
				m.layout()
				return m, nil
			}
			if m.streaming {
				m.cancelActiveStream()
				return m, nil
			}
		case "pageup", "pgup":
			if m.viewport.AtTop() && m.visibleHistoryLimit < len(m.messages) {
				m.visibleHistoryLimit += 120
				m.refreshConversation()
				m.viewport.GotoTop()
			}
			m.viewport.PageUp()
			if m.streaming {
				m.followStream = m.viewport.AtBottom()
			}
			return m, nil
		case "pagedown", "pgdown":
			m.viewport.PageDown()
			if m.streaming {
				m.followStream = m.viewport.AtBottom()
			}
			return m, nil
		case "enter":
			if m.streaming {
				value := strings.TrimSpace(m.expandPastedContent(m.composer.Value()))
				if value != "" {
					m.queuedInputs = append(m.queuedInputs, value)
					m.composer.Reset()
					m.pastedContent = make(map[string]string)
					m.updateSlashAutocomplete()
					m.layout()
				}
				return m, nil
			}
			value := strings.TrimSpace(m.expandPastedContent(m.composer.Value()))
			if value == "" && len(m.pendingAttachments) == 0 {
				return m, nil
			}
			m.composer.Reset()
			m.pastedContent = make(map[string]string)
			m.updateSlashAutocomplete()
			if value != "" {
				if shortcut, ok := slashFunctionKey(value); ok {
					return m.Update(shortcut)
				}
				if handled, command := m.runSlashCommand(value); handled {
					m.refreshConversation()
					m.layout()
					m.viewport.GotoBottom()
					return m, command
				}
			}
			value = unescapeSlashPrompt(value)
			images, attachments := m.takePendingImages()
			m.messages = append(m.messages, Message{Role: roleUser, Content: formatUserMessageWithImages(value, attachments)})

			if m.llm != nil {
				m.history = append(m.history, provider.Message{Role: provider.RoleUser, Content: value, Images: images})
				m.streamAssistantText = ""
				m.streamReasoningText = ""
				m.thinkingVisible = false
				m.messages = append(m.messages, Message{Role: roleAssistant})
				m.streaming = true
				m.progressFrame = 0
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
	beforeValue := m.composer.Value()
	beforeSlashUI := strings.HasPrefix(strings.TrimSpace(beforeValue), "/")
	var composerCmd tea.Cmd
	m.composer, composerCmd = m.composer.Update(msg)
	if composerCmd != nil {
		cmds = append(cmds, composerCmd)
	}
	valueChanged := beforeValue != m.composer.Value()
	if valueChanged {
		m.updateSlashAutocomplete()
	}
	if beforeHeight != m.composer.Height() {
		m.layout()
	} else if valueChanged {
		// The composer is rendered independently from the transcript. Rebuilding
		// the entire viewport for every typed character makes long sessions
		// progressively laggy because refreshConversation walks and rejoins all
		// messages before calling viewport.SetContent. Only slash autocomplete is
		// transcript-backed and needs a refresh while the composer value changes.
		afterSlashUI := strings.HasPrefix(strings.TrimSpace(m.composer.Value()), "/")
		if beforeSlashUI || afterSlashUI {
			m.refreshConversation()
			m.viewport.GotoBottom()
		}
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
	if m.browser != nil {
		return m.browser.view(m.width, m.height)
	}
	if m.terminalVisible && m.terminal != nil {
		return m.terminal.view(m.width, m.height)
	}
	if m.review != nil {
		return m.review.view(m.width, m.height)
	}
	if m.setup != nil {
		return m.setup.View()
	}
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

	if m.pendingPermission == nil && m.selectionPicker == nil {
		if cursor := m.composer.Cursor(); cursor != nil {
			cursor.X += m.frame.Composer.X
			cursor.Y += m.frame.Composer.Y
			v.Cursor = cursor
		}
	}
	return v
}

func (m *Model) inputView() string {
	if m.selectionPicker != nil {
		return pickerHelp(max(1, m.frame.Composer.Width))
	}
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
		start := 0
		if m.visibleHistoryLimit > 0 && len(m.messages) > m.visibleHistoryLimit {
			start = len(m.messages) - m.visibleHistoryLimit
			fmt.Fprintf(&b, "  [Older messages hidden: %d · PageUp at top to load more]\n\n", start)
		}
		for idx := start; idx < len(m.messages); idx++ {
			message := m.messages[idx]
			if idx > start {
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
					content = progressFrames[m.progressFrame] + " " + m.progressLabel()
				} else {
					content = m.renderCachedMarkdown(idx, content, max(1, inner-2))
					if m.streaming && idx == len(m.messages)-1 {
						content += "\n" + progressFrames[m.progressFrame] + " " + m.progressLabel()
					}
				}
				b.WriteString(renderTranscript("·", content, inner, agentMarkerStyle))
			}
		}
	}

	if picker := m.renderSelectionPicker(inner); picker != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(picker)
	} else if suggestions := m.renderSlashAutocomplete(inner); suggestions != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(suggestions)
	}
	if len(m.pendingAttachments) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(mutedStyle.Render(truncate(formatPendingAttachmentHint(m.pendingAttachments), inner)))
	}
	m.viewport.SetContent(b.String())
}

func (m *Model) flushTranscript() {
	if !m.transcriptDirty {
		return
	}
	follow := m.followStream
	offset := m.viewport.YOffset()
	m.refreshConversation()
	m.lastTranscriptRender = time.Now()
	m.transcriptDirty = false
	if follow {
		m.viewport.GotoBottom()
	} else {
		m.viewport.SetYOffset(min(offset, max(0, m.viewport.TotalLineCount()-m.viewport.VisibleLineCount())))
	}
}

func (m *Model) expandPastedContent(input string) string {
	for marker, content := range m.pastedContent {
		input = strings.ReplaceAll(input, marker, content)
	}
	return input
}

func (m *Model) renderCachedMarkdown(index int, content string, width int) string {
	if cached, ok := m.markdownCache[index]; ok && cached.source == content && cached.width == width {
		return cached.rendered
	}
	rendered := renderMarkdown(content, width)
	if m.markdownCache == nil {
		m.markdownCache = make(map[int]markdownCacheEntry)
	}
	if len(m.markdownCache) >= 128 {
		clear(m.markdownCache)
	}
	m.markdownCache[index] = markdownCacheEntry{source: content, width: width, rendered: rendered}
	return rendered
}

func renderMarkdown(content string, width int) string {
	if strings.TrimSpace(content) == "" {
		return content
	}
	content = normalizeCOSLinks(content)
	renderer, err := glamour.NewTermRenderer(glamour.WithStyles(rurushuMarkdownStyle()), glamour.WithWordWrap(width), glamour.WithTableWrap(true))
	if err != nil {
		return content
	}
	rendered, err := renderer.Render(content)
	if err != nil {
		return content
	}
	// Bubble Tea's viewport can expose OSC 8 hyperlink control sequences as
	// stray glyphs. Glamour already renders the destination as visible text,
	// so remove only the hyperlink controls while preserving ANSI colors.
	return strings.TrimRight(markdownOSC8.ReplaceAllString(rendered, ""), "\n")
}

var markdownOSC8 = regexp.MustCompile(`\x1b\]8;[^\x07\x1b]*(?:\x07|\x1b\\)`)

// COS may transmit ChatGPT's rich link tokens rather than Markdown links.
// Convert only well-formed HTTP(S) tokens; leave other tokens unchanged.
var cosLinkToken = regexp.MustCompile(`url([^]+)(https?://[^[:space:]]+)`)

func normalizeCOSLinks(content string) string {
	return cosLinkToken.ReplaceAllStringFunc(content, func(token string) string {
		parts := cosLinkToken.FindStringSubmatch(token)
		if len(parts) != 3 {
			return token
		}
		label := strings.NewReplacer("[", "\\[", "]", "\\]").Replace(parts[1])
		return "[" + label + "](" + parts[2] + ")"
	})
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
		state = "  " + progressFrames[m.progressFrame] + " " + m.progressLabel()
	}
	if m.runningJobs > 0 {
		state += "  " + jobCountLabel(m.runningJobs)
	}
	if m.watch.Active {
		state += "  watch " + m.watch.Interval
	}
	if len(m.pendingAttachments) > 0 {
		if len(m.pendingAttachments) == 1 {
			state += "  1 image"
		} else {
			state += fmt.Sprintf("  %d images", len(m.pendingAttachments))
		}
	}
	details := truncate(model+state, max(1, width-7))
	return statusNameStyle.Render("rurushu") + mutedStyle.Render("  "+details)
}

func (m *Model) progressLabel() string {
	if m.thinkingVisible {
		return "reasoning"
	}
	if m.streamAssistantText != "" {
		return "responding"
	}
	return "waiting for response"
}

func refreshJobs(manager *jobs.Manager) tea.Cmd {
	return func() tea.Msg {
		if manager == nil {
			return jobRefreshMsg{}
		}
		return jobRefreshMsg{running: manager.RunningCount()}
	}
}

func jobCountLabel(count int) string {
	if count == 1 {
		return "1 job"
	}
	return fmt.Sprintf("%d jobs", count)
}

func (m *Model) completionRequest() provider.CompletionRequest {
	return provider.CompletionRequest{
		Model:     m.modelName,
		Messages:  boundVisionHistory(m.history, maxRequestHistoryImages),
		SessionID: m.sessionID,
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
	case provider.EventReplace:
		m.ensureAssistantMessage()
		m.messages[len(m.messages)-1].Content = event.Text
		m.streamAssistantText = event.Text
		m.thinkingVisible = false
		m.toolActivity = ""
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
				Role:        provider.RoleTool,
				ToolCallID:  event.ToolExecution.ID,
				Name:        event.ToolExecution.Tool,
				Content:     event.ToolExecution.Output,
				ToolIsError: event.ToolExecution.IsError,
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
		m.persistSessionWithWarning()
		m.watchRunStatus("completed", "")
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

func (m *Model) nextQueuedSubmitCmd() tea.Cmd {
	if len(m.queuedInputs) == 0 {
		return nil
	}
	return func() tea.Msg { return queuedSubmitMsg{} }
}

func (m *Model) finishStreamWithError(err error) {
	message := "request failed"
	if err != nil {
		message = "error: " + err.Error()
	}
	m.watchRunStatus("failed", message)
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
	m.watchRunStatus("interrupted", "stream cancelled")
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
