package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/arborlogic/rurushu-go/permission"
	"github.com/arborlogic/rurushu-go/provider"
)

type fakeStreamer struct {
	events []provider.StreamEvent
	req    provider.CompletionRequest
	err    error
}

func (f *fakeStreamer) Stream(_ context.Context, req provider.CompletionRequest) (<-chan provider.StreamEvent, error) {
	f.req = req
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan provider.StreamEvent, len(f.events))
	for _, event := range f.events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

type controlledStreamer struct {
	ctx context.Context
	req provider.CompletionRequest
	ch  chan provider.StreamEvent
}

func newControlledStreamer() *controlledStreamer {
	return &controlledStreamer{ch: make(chan provider.StreamEvent, 8)}
}

func (f *controlledStreamer) Stream(ctx context.Context, req provider.CompletionRequest) (<-chan provider.StreamEvent, error) {
	f.ctx = ctx
	f.req = req
	return f.ch, nil
}

func TestComposerDoesNotGrowOnFirstCharacter(t *testing.T) {
	m := New("test-model", "openai-compatible")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	initial := m.composer.Height()

	m.Update(tea.KeyPressMsg(tea.Key{Code: 'a', Text: "a"}))
	if m.composer.Height() != initial {
		t.Fatalf("first character changed composer height: %d -> %d", initial, m.composer.Height())
	}
	if m.composer.Value() != "a" {
		t.Fatalf("composer value=%q", m.composer.Value())
	}
}

func TestPlainEnterSubmitsAndShiftEnterDoesNot(t *testing.T) {
	m := New("test-model", "openai-compatible")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.composer.SetValue("line one")

	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter, Mod: tea.ModShift}))
	if !strings.Contains(m.composer.Value(), "\n") {
		t.Fatalf("shift+enter did not insert newline: %q", m.composer.Value())
	}
	if len(m.messages) != 0 {
		t.Fatalf("shift+enter submitted unexpectedly")
	}

	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if len(m.messages) != 1 {
		t.Fatalf("plain enter did not submit, messages=%d", len(m.messages))
	}
	if m.composer.Value() != "" {
		t.Fatalf("composer was not reset: %q", m.composer.Value())
	}
}

func TestComposerDynamicHeightIsBounded(t *testing.T) {
	m := New("test-model", "openai-compatible")
	m.Update(tea.WindowSizeMsg{Width: 36, Height: 20})
	m.composer.SetValue(strings.Repeat("0123456789 ", 30))
	m.layout()
	if m.composer.Height() < 1 || m.composer.Height() > 4 {
		t.Fatalf("composer height out of bounds: %d", m.composer.Height())
	}
}

func TestViewNeverExceedsTerminalWidth(t *testing.T) {
	for _, width := range []int{32, 48, 80, 120} {
		m := New("hf.co/example/very-long-model-name", "openai-compatible")
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		view := m.View().Content
		for index, line := range strings.Split(view, "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("width=%d line=%d overflow=%d: %q", width, index, got, line)
			}
		}
	}
}

func TestWorkbenchIsLeftAnchoredAndHasNoLegacyChrome(t *testing.T) {
	m := New("test-model", "openai-compatible")
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 28})
	view := m.View().Content

	if m.frame.Workspace.Width != 120 {
		t.Fatalf("workspace width=%d, want 120", m.frame.Workspace.Width)
	}
	if m.frame.Workspace.X != 3 {
		t.Fatalf("expected compact left gutter x=3, got %d", m.frame.Workspace.X)
	}
	for _, legacy := range []string{"Enter send", "PgUp/PgDn", "────────────────", "YOU"} {
		if strings.Contains(view, legacy) {
			t.Fatalf("legacy chrome %q still present:\n%s", legacy, view)
		}
	}
	if !strings.Contains(view, "rurushu") || !strings.Contains(view, "workspace ready") || !strings.Contains(view, "❯") {
		t.Fatalf("workbench shell missing expected elements:\n%s", view)
	}
}

func TestZeroAPIStreamAppendsAssistantTokens(t *testing.T) {
	streamer := &fakeStreamer{events: []provider.StreamEvent{
		{Type: provider.EventToken, Text: "hello"},
		{Type: provider.EventToken, Text: " world"},
		{Type: provider.EventDone},
	}}
	m := NewWithStreamer("gpt-test", "openai-compatible", streamer)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.composer.SetValue("ping")

	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if cmd == nil || !m.streaming {
		t.Fatal("submit did not start stream")
	}

	msg := cmd()
	_, cmd = m.Update(msg)
	for cmd != nil {
		msg = cmd()
		_, cmd = m.Update(msg)
	}

	if m.streaming {
		t.Fatal("stream still marked active after done")
	}
	if len(m.messages) != 2 || m.messages[1].Content != "hello world" {
		t.Fatalf("unexpected transcript: %#v", m.messages)
	}
	if len(streamer.req.Messages) != 1 || streamer.req.Messages[0].Content != "ping" {
		t.Fatalf("unexpected request messages: %#v", streamer.req.Messages)
	}
}

func TestToolEventsOnlyAffectCompactStatus(t *testing.T) {
	m := New("test-model", "openai-compatible")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.streaming = true

	m.applyStreamEvent(provider.StreamEvent{
		Type: provider.EventToolStart,
		ToolExecution: &provider.ToolExecutionInfo{
			Tool: "git_status",
		},
	})
	if !strings.Contains(m.statusView(), "tool git_status") {
		t.Fatalf("tool activity missing from status: %q", m.statusView())
	}
	if len(m.messages) != 0 {
		t.Fatalf("tool status polluted transcript: %#v", m.messages)
	}

	m.applyStreamEvent(provider.StreamEvent{Type: provider.EventToolResult})
	if strings.Contains(m.statusView(), "tool git_status") {
		t.Fatalf("tool activity did not clear: %q", m.statusView())
	}
}

func TestInlinePermissionApproveAndDeny(t *testing.T) {
	handler := permission.NewTUIHandler()
	m := New("test-model", "openai-compatible")
	m.SetPermissionRequests(handler.Requests())
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	approvedResult := make(chan bool, 1)
	go func() {
		approved, _ := handler.Ask(context.Background(), permission.Request{Tool: "write", Pattern: "main.go", Description: "Write main.go"})
		approvedResult <- approved
	}()
	msg := waitForPermission(handler.Requests())()
	m.Update(msg)
	if m.pendingPermission == nil || m.composer.Focused() {
		t.Fatal("permission prompt did not take input focus")
	}
	if view := m.inputView(); !strings.Contains(view, "allow write") || !strings.Contains(view, "[y] allow") {
		t.Fatalf("permission prompt not rendered: %q", view)
	}
	_, next := m.Update(tea.KeyPressMsg(tea.Key{Code: 'y', Text: "y"}))
	if next == nil {
		t.Fatal("permission listener was not re-armed after approval")
	}
	if approved := <-approvedResult; !approved {
		t.Fatal("permission approval did not reach handler")
	}
	if m.pendingPermission != nil || !m.composer.Focused() {
		t.Fatal("composer did not recover after permission approval")
	}

	deniedResult := make(chan bool, 1)
	go func() {
		approved, _ := handler.Ask(context.Background(), permission.Request{Tool: "bash", Pattern: "make test", Description: "Execute make test"})
		deniedResult <- approved
	}()
	msg = waitForPermission(handler.Requests())()
	m.Update(msg)
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if approved := <-deniedResult; approved {
		t.Fatal("enter should deny permission by default")
	}
}

func TestStreamingRejectsSecondSubmit(t *testing.T) {
	streamer := newControlledStreamer()
	m := NewWithStreamer("gpt-test", "openai-compatible", streamer)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.composer.SetValue("first")

	_, start := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if start == nil || !m.streaming {
		t.Fatal("first submit did not start stream")
	}
	_, wait := m.Update(start())
	if wait == nil {
		t.Fatal("stream did not begin waiting for events")
	}

	m.composer.SetValue("second")
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if len(m.messages) != 2 {
		t.Fatalf("second submit changed transcript while streaming: %#v", m.messages)
	}
	if m.composer.Value() != "second" {
		t.Fatalf("second draft was modified while submit was blocked: %q", m.composer.Value())
	}
}

func TestEscCancelsStreamAndIgnoresLateEvents(t *testing.T) {
	streamer := newControlledStreamer()
	m := NewWithStreamer("gpt-test", "openai-compatible", streamer)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.composer.SetValue("cancel me")

	_, start := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	started := start()
	_, wait := m.Update(started)
	if streamer.ctx == nil {
		t.Fatal("streamer did not receive context")
	}
	streamID := m.activeStream

	streamer.ch <- provider.StreamEvent{Type: provider.EventToken, Text: "partial"}
	_, wait = m.Update(wait())
	if got := m.messages[len(m.messages)-1].Content; got != "partial" {
		t.Fatalf("partial response=%q", got)
	}

	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if m.streaming || m.activeStream != 0 {
		t.Fatalf("stream still active after Esc: streaming=%v active=%d", m.streaming, m.activeStream)
	}
	select {
	case <-streamer.ctx.Done():
	default:
		t.Fatal("Esc did not cancel provider context")
	}
	if !m.composer.Focused() {
		t.Fatal("composer lost focus after cancellation")
	}

	streamer.ch <- provider.StreamEvent{Type: provider.EventToken, Text: " late"}
	m.Update(wait())
	if got := m.messages[len(m.messages)-1].Content; got != "partial" {
		t.Fatalf("late event mutated cancelled response: %q (stream id %d)", got, streamID)
	}
}

func TestEscRemovesEmptyAssistantPlaceholder(t *testing.T) {
	streamer := newControlledStreamer()
	m := NewWithStreamer("gpt-test", "openai-compatible", streamer)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.composer.SetValue("cancel before first token")

	_, start := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m.Update(start())
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))

	if len(m.messages) != 1 || m.messages[0].Role != roleUser {
		t.Fatalf("empty assistant placeholder survived cancellation: %#v", m.messages)
	}
}

func TestStreamErrorRendersExactlyOneAssistantMessage(t *testing.T) {
	streamer := &fakeStreamer{err: errors.New("gateway unavailable")}
	m := NewWithStreamer("gpt-test", "openai-compatible", streamer)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.composer.SetValue("ping")

	_, start := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m.Update(start())

	if m.streaming {
		t.Fatal("stream still active after provider error")
	}
	if len(m.messages) != 2 {
		t.Fatalf("provider error produced duplicate messages: %#v", m.messages)
	}
	if got := m.messages[1].Content; got != "error: gateway unavailable" {
		t.Fatalf("assistant error=%q", got)
	}
}

func TestStreamingDoesNotYankViewportAfterUserScrollsUp(t *testing.T) {
	streamer := newControlledStreamer()
	m := NewWithStreamer("gpt-test", "openai-compatible", streamer)
	m.Update(tea.WindowSizeMsg{Width: 56, Height: 10})
	m.messages = []Message{{Role: roleAssistant, Content: strings.Repeat("history line\n", 60)}}
	m.refreshConversation()
	m.viewport.GotoBottom()
	m.composer.SetValue("continue")

	_, start := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	_, wait := m.Update(start())
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyPgUp}))
	if m.followStream {
		t.Fatal("page up did not disable stream following")
	}
	offset := m.viewport.YOffset()
	if offset == 0 {
		t.Fatal("test transcript was not scrollable")
	}

	streamer.ch <- provider.StreamEvent{Type: provider.EventToken, Text: strings.Repeat("new output line\n", 12)}
	m.Update(wait())
	if got := m.viewport.YOffset(); got != offset {
		t.Fatalf("stream yanked viewport after scroll up: offset=%d -> %d", offset, got)
	}
}

func TestSequentialResizesPreserveExactTerminalGeometry(t *testing.T) {
	m := New("test-model", "openai-compatible")
	m.messages = []Message{
		{Role: roleUser, Content: strings.Repeat("resize transcript content ", 8)},
		{Role: roleAssistant, Content: strings.Repeat("assistant response content ", 10)},
	}
	m.composer.SetValue("a multiline draft that should wrap as the terminal gets narrower")

	for _, size := range []tea.WindowSizeMsg{
		{Width: 120, Height: 32},
		{Width: 72, Height: 20},
		{Width: 42, Height: 14},
		{Width: 24, Height: 7},
		{Width: 16, Height: 3},
		{Width: 96, Height: 26},
		{Width: 160, Height: 40},
	} {
		m.Update(size)
		view := m.View()
		if got := lipgloss.Height(view.Content); got != size.Height {
			t.Fatalf("resize %dx%d rendered height=%d\n%s", size.Width, size.Height, got, view.Content)
		}
		for lineIndex, line := range strings.Split(view.Content, "\n") {
			if got := lipgloss.Width(line); got != size.Width {
				t.Fatalf("resize %dx%d line=%d rendered width=%d, want exact root width\n%q", size.Width, size.Height, lineIndex, got, line)
			}
		}
		if view.Cursor != nil {
			if view.Cursor.X < 0 || view.Cursor.X >= size.Width || view.Cursor.Y < 0 || view.Cursor.Y >= size.Height {
				t.Fatalf("resize %dx%d cursor escaped window: x=%d y=%d", size.Width, size.Height, view.Cursor.X, view.Cursor.Y)
			}
		}
	}
}
