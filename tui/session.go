package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/arborlogic/rurushu-go/projectstate"
	"github.com/arborlogic/rurushu-go/provider"
)

func (m *Model) SetProjectSession(store *projectstate.Store, session projectstate.Session) {
	m.sessionStore = store
	m.sessionID = session.ID
	m.sessionSaveError = ""
	m.restoreHistory(session.Messages)
	m.registerSessionSlashCommands()
}

func (m *Model) SessionID() string { return m.sessionID }

func (m *Model) registerSessionSlashCommands() {
	if m.sessionStore == nil {
		return
	}
	_ = m.RegisterSlashCommand(SlashCommand{
		Name:        "sessions",
		Usage:       "/sessions",
		Description: "list project sessions",
		Run: func(args string) (SlashCommandResult, error) {
			if strings.TrimSpace(args) != "" {
				return SlashCommandResult{}, fmt.Errorf("/sessions does not accept arguments")
			}
			sessions, err := m.sessionStore.ListSessions()
			if err != nil {
				return SlashCommandResult{}, err
			}
			return SlashCommandResult{Output: formatSessions(sessions, m.sessionID)}, nil
		},
	})
	_ = m.RegisterSlashCommand(SlashCommand{
		Name:        "resume",
		Usage:       "/resume <session-id|last>",
		Description: "restore a saved project session",
		Run: func(args string) (SlashCommandResult, error) {
			id := strings.TrimSpace(args)
			if id == "" {
				sessions, err := m.sessionStore.ListSessions()
				if err != nil {
					return SlashCommandResult{}, err
				}
				if len(sessions) == 0 {
					return SlashCommandResult{Output: "No saved sessions."}, nil
				}
				items := make([]selectionPickerItem, 0, len(sessions))
				for _, listed := range sessions {
					sessionID := listed.ID
					marker := " "
					if sessionID == m.sessionID {
						marker = "*"
					}
					items = append(items, selectionPickerItem{
						Label:   fmt.Sprintf("%s %s  %d asks  %s", marker, sessionID, countUserAsks(listed.Messages), formatSessionTime(listed.UpdatedAt)),
						Command: "/resume " + sessionID,
						Run: func() (string, error) {
							return m.resumeSession(sessionID)
						},
					})
				}
				return SlashCommandResult{picker: &selectionPicker{Title: "resume session", Items: items}}, nil
			}
			output, err := m.resumeSession(id)
			return SlashCommandResult{Output: output}, err
		},
	})
	_ = m.RegisterSlashCommand(SlashCommand{
		Name:        "new",
		Usage:       "/new",
		Description: "start a new empty project session",
		Run: func(args string) (SlashCommandResult, error) {
			if strings.TrimSpace(args) != "" {
				return SlashCommandResult{}, fmt.Errorf("/new does not accept arguments")
			}
			session, err := m.sessionStore.StartSession(m.modelName, m.provider)
			if err != nil {
				return SlashCommandResult{}, err
			}
			m.restoreSession(session)
			return SlashCommandResult{Output: "started " + session.ID}, nil
		},
	})
	_ = m.RegisterSlashCommand(SlashCommand{
		Name:        "rewind",
		Usage:       "/rewind [ask-number|last]",
		Description: "fork from an earlier ask and load it for editing",
		Run: func(args string) (SlashCommandResult, error) {
			asks := userAskPositions(m.history)
			value := strings.TrimSpace(args)
			if value == "" {
				if len(asks) == 0 {
					return SlashCommandResult{Output: "No asks to rewind."}, nil
				}
				items := make([]selectionPickerItem, 0, len(asks))
				for number, index := range asks {
					askNumber := number + 1
					preview := strings.Join(strings.Fields(m.history[index].Content), " ")
					items = append(items, selectionPickerItem{
						Label:   fmt.Sprintf("#%d  %s", askNumber, truncatePlain(preview, 88)),
						Command: fmt.Sprintf("/rewind %d", askNumber),
						Run: func() (string, error) {
							return m.rewindAsk(askNumber, userAskPositions(m.history))
						},
					})
				}
				return SlashCommandResult{picker: &selectionPicker{Title: "rewind ask", Items: items}}, nil
			}
			askNumber := 0
			if value == "last" {
				askNumber = len(asks)
			} else {
				parsed, err := strconv.Atoi(value)
				if err != nil {
					return SlashCommandResult{}, fmt.Errorf("usage: /rewind [ask-number|last]")
				}
				askNumber = parsed
			}
			output, err := m.rewindAsk(askNumber, asks)
			if err != nil {
				return SlashCommandResult{}, err
			}
			return SlashCommandResult{Output: output}, nil
		},
	})
}

func (m *Model) resumeSession(id string) (string, error) {
	var (
		session projectstate.Session
		err     error
	)
	if id == "last" {
		session, err = m.sessionStore.LastSession()
	} else {
		session, err = m.sessionStore.ActivateSession(id)
	}
	if err != nil {
		return "", err
	}
	if id == "last" {
		if _, err := m.sessionStore.ActivateSession(session.ID); err != nil {
			return "", err
		}
	}
	m.restoreSession(session)
	return fmt.Sprintf("resumed %s · %d asks", session.ID, countUserAsks(session.Messages)), nil
}

func (m *Model) restoreSession(session projectstate.Session) {
	m.sessionID = session.ID
	m.sessionSaveError = ""
	m.restoreHistory(session.Messages)
	m.composer.Reset()
	m.updateSlashAutocomplete()
	m.refreshConversation()
	if m.frame.Transcript.Height > 0 {
		m.viewport.GotoBottom()
	}
}

func (m *Model) restoreHistory(history []provider.Message) {
	m.history = cloneProviderMessages(history)
	m.messages = transcriptFromHistory(m.history)
	m.streamAssistantText = ""
	m.streamReasoningText = ""
	m.toolActivity = ""
	m.thinkingVisible = false
}

func transcriptFromHistory(history []provider.Message) []Message {
	result := make([]Message, 0, len(history))
	toolActivities := make(map[string]int)
	for _, message := range history {
		switch message.Role {
		case provider.RoleUser:
			if strings.TrimSpace(message.Content) != "" {
				result = append(result, Message{Role: roleUser, Content: message.Content})
			}
		case provider.RoleAssistant:
			if strings.TrimSpace(message.Content) != "" {
				result = append(result, Message{Role: roleAssistant, Content: message.Content})
			}
			for _, call := range message.ToolCalls {
				activity := &activityEntry{Kind: "tool", ID: call.ID, Tool: call.Function.Name, Args: call.Function.Arguments}
				result = append(result, Message{Role: roleActivity, Local: true, Activity: activity})
				if call.ID != "" {
					toolActivities[call.ID] = len(result) - 1
				}
			}
		case provider.RoleTool:
			if index, ok := toolActivities[message.ToolCallID]; ok && index >= 0 && index < len(result) {
				activity := result[index].Activity
				activity.Output = message.Content
				activity.IsError = message.ToolIsError
				activity.Done = true
				continue
			}
			result = append(result, Message{Role: roleActivity, Local: true, Activity: &activityEntry{
				Kind: "tool", ID: message.ToolCallID, Tool: message.Name, Output: message.Content, IsError: message.ToolIsError, Done: true,
			}})
		}
	}
	return result
}

func (m *Model) persistSessionWithWarning() {
	if m.sessionStore == nil || m.sessionID == "" {
		return
	}
	_, err := m.sessionStore.SaveSession(m.sessionID, m.modelName, m.provider, m.history)
	if err == nil {
		m.sessionSaveError = ""
		return
	}
	message := err.Error()
	if message == m.sessionSaveError {
		return
	}
	m.sessionSaveError = message
	m.messages = append(m.messages, Message{Role: roleAssistant, Content: "warning: session persistence failed: " + message, Local: true})
}

func (m *Model) rewindAsk(askNumber int, asks []int) (string, error) {
	if len(asks) == 0 {
		return "", fmt.Errorf("there are no asks to rewind")
	}
	if askNumber <= 0 || askNumber > len(asks) {
		return "", fmt.Errorf("ask number must be between 1 and %d", len(asks))
	}
	index := asks[askNumber-1]
	original := m.history[index].Content
	prefix := cloneProviderMessages(m.history[:index])
	parent := m.sessionID

	if m.sessionStore != nil && parent != "" {
		fork, err := m.sessionStore.ForkSession(parent, askNumber, m.modelName, m.provider, prefix)
		if err != nil {
			return "", err
		}
		m.sessionID = fork.ID
	}
	m.restoreHistory(prefix)
	m.composer.SetValue(original)
	m.composer.CursorEnd()
	m.updateSlashAutocomplete()
	if m.sessionID != "" && m.sessionID != parent {
		return fmt.Sprintf("rewound ask #%d into %s; edit the loaded ask and press Enter\noriginal session preserved as %s", askNumber, m.sessionID, parent), nil
	}
	return fmt.Sprintf("rewound ask #%d; edit the loaded ask and press Enter", askNumber), nil
}

func formatSessions(sessions []projectstate.Session, currentID string) string {
	if len(sessions) == 0 {
		return "No saved sessions."
	}
	var b strings.Builder
	b.WriteString("sessions")
	for _, session := range sessions {
		marker := " "
		if session.ID == currentID {
			marker = "*"
		}
		fmt.Fprintf(&b, "\n\n%s %s  %d asks  %s", marker, session.ID, countUserAsks(session.Messages), formatSessionTime(session.UpdatedAt))
		if session.ParentSession != "" {
			fmt.Fprintf(&b, "\n    rewind of %s ask #%d", session.ParentSession, session.RewoundFromAsk)
		}
	}
	return b.String()
}

func formatRewindAsks(history []provider.Message, asks []int) string {
	if len(asks) == 0 {
		return "No asks to rewind."
	}
	var b strings.Builder
	b.WriteString("rewind asks")
	for number, index := range asks {
		preview := strings.Join(strings.Fields(history[index].Content), " ")
		preview = truncatePlain(preview, 88)
		fmt.Fprintf(&b, "\n\n#%d  %s", number+1, preview)
	}
	b.WriteString("\n\nUse /rewind <ask-number> to fork there and load that ask into the editor.")
	return b.String()
}

func userAskPositions(history []provider.Message) []int {
	positions := make([]int, 0)
	for index, message := range history {
		if message.Role == provider.RoleUser {
			positions = append(positions, index)
		}
	}
	return positions
}

func countUserAsks(history []provider.Message) int {
	return len(userAskPositions(history))
}

func cloneProviderMessages(messages []provider.Message) []provider.Message {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]provider.Message, len(messages))
	copy(cloned, messages)
	for i := range cloned {
		if messages[i].Images != nil {
			cloned[i].Images = append([]string(nil), messages[i].Images...)
		}
		if messages[i].ToolCalls != nil {
			cloned[i].ToolCalls = append([]provider.ToolCall(nil), messages[i].ToolCalls...)
		}
	}
	return cloned
}

func formatSessionTime(value time.Time) string {
	if value.IsZero() {
		return "unknown time"
	}
	return value.Local().Format("2006-01-02 15:04")
}

func truncatePlain(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}
