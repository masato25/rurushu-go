package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	rurushuconfig "github.com/masato25/rurushu-go/config"
)

type SetupModel struct {
	width       int
	height      int
	inputs      []textinput.Model
	focus       int
	config      rurushuconfig.Config
	saved       bool
	path        string
	err         error
	embedded    bool
	scroll      int
	goalSave    func(string) error
	watchEditor *setupWatchEditor
	watchTab    bool
}

type setupWatchEditor struct {
	interval textinput.Model
	prompt   textinput.Model
	focus    int
	active   bool
	next     time.Time
	last     time.Time
	save     func(bool, string, string) error
}

const (
	setupBaseURL = iota
	setupAPIKey
	setupModel
	setupSystemPrompt
	setupMaxSteps
	setupMaxContext
	setupCompactAt
	setupFieldCount
)

func NewSetupModel(cfg rurushuconfig.Config) *SetupModel {
	inputs := make([]textinput.Model, setupFieldCount)
	labels := []string{"Base URL", "API key", "Model", "System prompt", "Max steps", "Max context tokens", "Compact at %"}
	values := []string{
		cfg.BaseURL,
		cfg.APIKey,
		cfg.Model,
		cfg.SystemPrompt,
		strconv.Itoa(cfg.MaxSteps),
		strconv.Itoa(cfg.MaxContextTokens),
		strconv.FormatFloat(cfg.CompactAt, 'f', -1, 64),
	}
	for i := range inputs {
		inputs[i] = textinput.New()
		inputs[i].Prompt = ""
		inputs[i].Placeholder = labels[i]
		inputs[i].SetValue(values[i])
		inputs[i].SetWidth(72)
	}
	inputs[setupAPIKey].EchoMode = textinput.EchoPassword
	inputs[setupAPIKey].EchoCharacter = '•'
	inputs[setupBaseURL].Focus()
	return &SetupModel{inputs: inputs, config: cfg}
}

func (m *SetupModel) EnableGoalEditor(goal string, save func(string) error) {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = "Duel goal"
	input.SetValue(strings.TrimSpace(goal))
	input.SetWidth(72)
	m.inputs = append(m.inputs, input)
	m.goalSave = save
}

func (m *SetupModel) EnableWatchEditor(active bool, interval, prompt string, next, last time.Time, save func(bool, string, string) error) {
	a := textinput.New()
	a.Prompt = ""
	a.Placeholder = "10m"
	a.SetValue(interval)
	b := textinput.New()
	b.Prompt = ""
	b.Placeholder = "Instructions for each scheduled run"
	b.SetValue(prompt)
	m.watchEditor = &setupWatchEditor{interval: a, prompt: b, active: active, next: next, last: last, save: save}
}

func (m *SetupModel) toggleWatchTab() tea.Cmd {
	if m.watchEditor == nil {
		return nil
	}
	m.watchTab = !m.watchTab
	m.err = nil
	m.scroll = 0
	for i := range m.inputs {
		m.inputs[i].Blur()
	}
	m.watchEditor.interval.Blur()
	m.watchEditor.prompt.Blur()
	if m.watchTab {
		return m.watchEditor.interval.Focus()
	}
	return m.inputs[m.focus].Focus()
}

func (m *SetupModel) stopWatch() error {
	w := m.watchEditor
	if w == nil {
		return nil
	}
	if err := w.save(false, w.interval.Value(), w.prompt.Value()); err != nil {
		return err
	}
	w.active = false
	w.next = time.Time{}
	return nil
}

func (m *SetupModel) saveWatch() error {
	w := m.watchEditor
	if w == nil {
		return nil
	}
	interval := strings.TrimSpace(w.interval.Value())
	d, err := time.ParseDuration(interval)
	if err != nil || d < time.Second || d > 24*time.Hour {
		return fmt.Errorf("watch interval must be between 1s and 24h (e.g. 10m)")
	}
	prompt := strings.TrimSpace(w.prompt.Value())
	if prompt == "" {
		return fmt.Errorf("watch instruction is required")
	}
	if err := w.save(true, interval, prompt); err != nil {
		return err
	}
	w.active = true
	w.next = time.Now().Add(d)
	return nil
}

func (m *SetupModel) Init() tea.Cmd { return textinput.Blink }

func (m *SetupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		width := min(72, max(20, msg.Width-8))
		for i := range m.inputs {
			m.inputs[i].SetWidth(width)
		}
		if m.watchEditor != nil {
			m.watchEditor.interval.SetWidth(width)
			m.watchEditor.prompt.SetWidth(width)
		}
		return m, nil
	case tea.MouseWheelMsg:
		if msg.Button == tea.MouseWheelDown {
			m.scroll += 3
		}
		if msg.Button == tea.MouseWheelUp {
			m.scroll = max(0, m.scroll-3)
		}
		return m, nil
	case tea.KeyPressMsg:
		if m.watchEditor != nil && msg.String() == "ctrl+w" {
			return m, m.toggleWatchTab()
		}
		if m.watchTab && m.watchEditor != nil {
			w := m.watchEditor
			switch msg.String() {
			case "esc", "f2":
				return m, tea.Quit
			case "ctrl+d":
				m.err = m.stopWatch()
				return m, nil
			case "tab", "down", "shift+tab", "up":
				if w.focus == 0 {
					w.interval.Blur()
					w.focus = 1
					return m, w.prompt.Focus()
				}
				w.prompt.Blur()
				w.focus = 0
				return m, w.interval.Focus()
			case "enter":
				if w.focus == 0 {
					w.interval.Blur()
					w.focus = 1
					return m, w.prompt.Focus()
				}
				m.err = m.saveWatch()
				return m, nil
			}
			var cmd tea.Cmd
			if w.focus == 0 {
				w.interval, cmd = w.interval.Update(msg)
			} else {
				w.prompt, cmd = w.prompt.Update(msg)
			}
			return m, cmd
		}
		if m.saved {
			switch msg.String() {
			case "enter", "esc", "ctrl+c", "q":
				return m, tea.Quit
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "tab", "down":
			return m, m.moveFocus(1)
		case "shift+tab", "up":
			return m, m.moveFocus(-1)
		case "enter":
			if m.focus < len(m.inputs)-1 {
				return m, m.moveFocus(1)
			}
			if err := m.save(); err != nil {
				m.err = err
				return m, nil
			}
			m.saved = true
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.inputs[m.focus], cmd = m.inputs[m.focus].Update(msg)
	return m, cmd
}

func (m *SetupModel) moveFocus(delta int) tea.Cmd {
	m.inputs[m.focus].Blur()
	m.focus = (m.focus + delta + len(m.inputs)) % len(m.inputs)
	return m.inputs[m.focus].Focus()
}

func (m *SetupModel) save() error {
	maxSteps, err := strconv.Atoi(strings.TrimSpace(m.inputs[setupMaxSteps].Value()))
	if err != nil || maxSteps <= 0 {
		return fmt.Errorf("max steps must be a positive integer")
	}
	maxContext, err := strconv.Atoi(strings.TrimSpace(m.inputs[setupMaxContext].Value()))
	if err != nil || maxContext <= 0 {
		return fmt.Errorf("max context tokens must be a positive integer")
	}
	compactAt, err := strconv.ParseFloat(strings.TrimSpace(m.inputs[setupCompactAt].Value()), 64)
	if err != nil || compactAt <= 0 || compactAt > 100 {
		return fmt.Errorf("compact at must be between 0 and 100")
	}
	baseURL := strings.TrimSpace(m.inputs[setupBaseURL].Value())
	if baseURL == "" {
		return fmt.Errorf("base URL is required")
	}
	model := strings.TrimSpace(m.inputs[setupModel].Value())
	if model == "" {
		return fmt.Errorf("model is required")
	}
	m.config = rurushuconfig.Config{
		BaseURL:          baseURL,
		APIKey:           strings.TrimSpace(m.inputs[setupAPIKey].Value()),
		Model:            model,
		SystemPrompt:     strings.TrimSpace(m.inputs[setupSystemPrompt].Value()),
		MaxSteps:         maxSteps,
		MaxContextTokens: maxContext,
		CompactAt:        compactAt,
		AutoApprove:      m.config.AutoApprove,
	}
	path, err := rurushuconfig.Save(m.config)
	if err != nil {
		return err
	}
	m.path = path
	if m.goalSave != nil {
		goal := strings.TrimSpace(m.inputs[len(m.inputs)-1].Value())
		if goal == "" {
			return fmt.Errorf("duel goal is required")
		}
		if err := m.goalSave(goal); err != nil {
			return err
		}
	}
	m.err = nil
	return nil
}

func (m *SetupModel) View() tea.View {
	if m.watchTab && m.watchEditor != nil {
		return m.watchView()
	}
	if m.saved {
		instruction := "press Enter to exit"
		if m.embedded {
			instruction = "press Enter to return to chat (restart to apply changed runtime settings)"
		}
		content := statusNameStyle.Render("rurushu setup") + "\n\n" +
			"saved configuration\n" + mutedStyle.Render(m.path) + "\n\n" +
			mutedStyle.Render(instruction)
		view := tea.NewView(content)
		view.AltScreen = true
		view.WindowTitle = "Rurushu Setup"
		return view
	}

	labels := []string{"Base URL", "API key", "Model", "System prompt", "Max steps", "Max context tokens", "Compact at %"}
	if len(m.inputs) > setupFieldCount {
		labels = append(labels, "Duel goal")
	}
	var b strings.Builder
	b.WriteString(statusNameStyle.Render("rurushu setup"))
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("Configure the standalone terminal client. API key is stored locally with file mode 0600."))
	if m.watchEditor != nil {
		b.WriteString("\n" + mutedStyle.Render("Ctrl+W: Scheduled tasks (view, edit, stop)"))
	}
	b.WriteString("\n\n")
	for i := range m.inputs {
		marker := "  "
		labelStyle := mutedStyle
		if i == m.focus {
			marker = "› "
			labelStyle = lipgloss.NewStyle().Bold(true)
		}
		b.WriteString(marker + labelStyle.Render(labels[i]) + "\n")
		b.WriteString("  " + m.inputs[i].View() + "\n\n")
	}
	if m.err != nil {
		b.WriteString(lipgloss.NewStyle().Bold(true).Render("error: ") + m.err.Error() + "\n\n")
	}
	footer := "Tab/↓ next  Shift+Tab/↑ previous  Enter advance/save  Esc cancel"
	if m.embedded {
		footer = "Tab/↓ next  Shift+Tab/↑ previous  Enter advance/save  Esc/F2 back to chat"
	}
	b.WriteString(mutedStyle.Render(footer))

	content := b.String()
	if m.width > 0 {
		content = lipgloss.NewStyle().Width(max(1, min(m.width, 88))).Render(content)
	}
	if m.height > 0 {
		lines := strings.Split(content, "\n")
		m.scroll = min(m.scroll, max(0, len(lines)-m.height))
		content = strings.Join(lines[m.scroll:min(len(lines), m.scroll+m.height)], "\n")
	}
	view := tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.WindowTitle = "Rurushu Setup"
	if cursor := m.inputs[m.focus].Cursor(); cursor != nil {
		view.Cursor = cursor
	}
	return view
}

func (m *SetupModel) Config() rurushuconfig.Config { return m.config }
func (m *SetupModel) SavedPath() string            { return m.path }

func (m *SetupModel) watchView() tea.View {
	w := m.watchEditor
	state := "stopped"
	if w.active {
		state = "running"
	}
	var b strings.Builder
	b.WriteString(statusNameStyle.Render("rurushu setup / Scheduled tasks"))
	b.WriteString("\n\nCurrent session watch: " + state)
	if !w.next.IsZero() && w.active {
		b.WriteString("\nNext run: " + w.next.Format(time.RFC3339))
	}
	if !w.last.IsZero() {
		b.WriteString("\nLast run: " + w.last.Format(time.RFC3339))
	}
	b.WriteString("\n\n")
	for i, field := range []struct {
		label string
		input textinput.Model
	}{{"Interval", w.interval}, {"Instruction", w.prompt}} {
		marker := "  "
		if i == w.focus {
			marker = "› "
		}
		b.WriteString(marker + field.label + "\n  " + field.input.View() + "\n\n")
	}
	if m.err != nil {
		b.WriteString("error: " + m.err.Error() + "\n\n")
	}
	b.WriteString(mutedStyle.Render("Enter: next/save & start  Ctrl+D: stop watch  Ctrl+W: settings  Esc/F2: back to chat"))
	content := b.String()
	if m.width > 0 {
		content = lipgloss.NewStyle().Width(max(1, min(m.width, 88))).Render(content)
	}
	if m.height > 0 {
		lines := strings.Split(content, "\n")
		m.scroll = min(m.scroll, max(0, len(lines)-m.height))
		content = strings.Join(lines[m.scroll:min(len(lines), m.scroll+m.height)], "\n")
	}
	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "Rurushu Scheduled Tasks"
	return v
}
