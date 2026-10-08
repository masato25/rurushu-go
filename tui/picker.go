package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type selectionPicker struct {
	Title string
	Items []selectionPickerItem
	Index int
}

type selectionPickerItem struct {
	Label   string
	Command string
	Run     func() (string, error)
}

func (m *Model) openSelectionPicker(picker *selectionPicker) {
	if picker == nil || len(picker.Items) == 0 {
		return
	}
	if picker.Index < 0 || picker.Index >= len(picker.Items) {
		picker.Index = 0
	}
	m.selectionPicker = picker
	m.composer.Blur()
	m.refreshConversation()
	m.layout()
	m.viewport.GotoBottom()
}

func (m *Model) closeSelectionPicker() {
	m.selectionPicker = nil
	m.composer.Focus()
	m.refreshConversation()
	m.layout()
	m.viewport.GotoBottom()
}

func (m *Model) handleSelectionPickerKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if m.selectionPicker == nil {
		return false, nil
	}
	picker := m.selectionPicker
	switch msg.String() {
	case "ctrl+c":
		m.selectionPicker = nil
		return true, tea.Quit
	case "esc":
		m.closeSelectionPicker()
		return true, nil
	case "up":
		picker.Index = (picker.Index - 1 + len(picker.Items)) % len(picker.Items)
		m.refreshConversation()
		return true, nil
	case "down":
		picker.Index = (picker.Index + 1) % len(picker.Items)
		m.refreshConversation()
		return true, nil
	case "enter":
		item := picker.Items[picker.Index]
		m.selectionPicker = nil
		output, err := item.Run()
		m.composer.Focus()
		if strings.TrimSpace(item.Command) != "" {
			m.messages = append(m.messages, Message{Role: roleUser, Content: item.Command, Local: true})
		}
		if err != nil {
			m.messages = append(m.messages, Message{Role: roleAssistant, Content: "error: " + err.Error(), Local: true})
		} else if strings.TrimSpace(output) != "" {
			m.messages = append(m.messages, Message{Role: roleAssistant, Content: strings.TrimSpace(output), Local: true})
		}
		m.refreshConversation()
		m.layout()
		m.viewport.GotoBottom()
		return true, nil
	default:
		return true, nil
	}
}

func (m *Model) renderSelectionPicker(width int) string {
	if m.selectionPicker == nil || len(m.selectionPicker.Items) == 0 || width <= 0 {
		return ""
	}
	picker := m.selectionPicker
	const maxVisible = 9
	limit := min(maxVisible, len(picker.Items))
	start := 0
	if picker.Index >= limit {
		start = picker.Index - limit + 1
	}
	end := min(len(picker.Items), start+limit)

	var b strings.Builder
	if strings.TrimSpace(picker.Title) != "" {
		b.WriteString(statusNameStyle.Render(truncate(picker.Title, width)))
		b.WriteString("\n")
	}
	for i := start; i < end; i++ {
		if i > start {
			b.WriteString("\n")
		}
		marker := "  "
		style := mutedStyle
		if i == picker.Index {
			marker = "› "
			style = statusNameStyle
		}
		b.WriteString(style.Render(truncate(marker+picker.Items[i].Label, width)))
	}
	if len(picker.Items) > limit {
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render(truncate(fmt.Sprintf("  %d/%d", picker.Index+1, len(picker.Items)), width)))
	}
	return b.String()
}

func pickerHelp(width int) string {
	return mutedStyle.Render(truncate("↑/↓ select  Enter choose  Esc close", width))
}
