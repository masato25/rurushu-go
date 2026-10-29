package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/masato25/rurushu-go/duel"
)

func TestDuelTUISendsOwnerInstruction(t *testing.T) {
	notes := make(chan string, 4)
	updates := make(chan duelUpdate, 4)
	ui := newDuelUI(notes, updates, func() {}, func() {})
	ui.input.SetValue("Please run the full regression suite")
	ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case got := <-notes:
		if got != "Please run the full regression suite" {
			t.Fatalf("instruction %q", got)
		}
	default:
		t.Fatal("instruction was not queued")
	}
	if ui.input.Value() != "" {
		t.Fatal("composer not cleared")
	}
}

func TestDuelTUIQueuesMultipleInstructionsWhileRunning(t *testing.T) {
	notes := make(chan string, 4)
	ui := newDuelUI(notes, make(chan duelUpdate, 4), func() {}, func() {})

	for _, note := range []string{"first steering note", "second steering note"} {
		ui.input.SetValue(note)
		ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	}

	if ui.stopped {
		t.Fatal("running duel should remain active while owner instructions are queued")
	}
	if got := len(notes); got != 2 {
		t.Fatalf("queued instructions=%d want=2", got)
	}
	if view := ui.View().Content; !strings.Contains(view, "待送 2 則") {
		t.Fatalf("queue depth not visible in TUI: %q", view)
	}
	if got := <-notes; got != "first steering note" {
		t.Fatalf("first queued instruction=%q", got)
	}
	if got := <-notes; got != "second steering note" {
		t.Fatalf("second queued instruction=%q", got)
	}
}

func TestDuelTUIEscapeClearsCurrentInput(t *testing.T) {
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	ui.input.SetValue("尚未送出的主人指令")

	ui.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := ui.input.Value(); got != "" {
		t.Fatalf("escape did not clear input: %q", got)
	}
}

func TestDuelTUIRendersBothSides(t *testing.T) {
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {}, "ses-left", "ses-right")
	ui.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	ui.Update(duelUpdate{role: "right", round: 1, turn: duel.Turn{Action: "plan", Summary: "review", Evidence: "checked tests"}})
	ui.Update(duelUpdate{role: "left", round: 1, turn: duel.Turn{Action: "continue", Summary: "implemented", Evidence: "tests pass"}})
	view := ui.View().Content
	for _, term := range []string{"執行者", "決策者", "ses-left", "ses-right", "review", "implemented"} {
		if !strings.Contains(view, term) {
			t.Fatalf("missing %q in %s", term, view)
		}
	}
	ui.Update(duelUpdate{done: true, result: duel.Result{Status: "owner_review", Summary: "done"}})
	if !ui.stopped || !strings.Contains(ui.status, "等待主人驗收") {
		t.Fatalf("unexpected finish state: %s", ui.status)
	}
}

func TestDuelTUIAcceptsTypingAfterProviderError(t *testing.T) {
	notes := make(chan string, 4)
	ui := newDuelUI(notes, make(chan duelUpdate, 4), func() {}, func() {})
	ui.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	ui.Update(duelUpdate{done: true, err: fmt.Errorf("COS decision missing")})
	ui.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if got := ui.input.Value(); got != "a" {
		t.Fatalf("typing failed after error: %q", got)
	}
	ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := <-notes; got != "a" {
		t.Fatalf("instruction not sent: %q", got)
	}
	if ui.stopped {
		t.Fatal("duel did not restart after owner input")
	}
}

func TestDuelTUIAcceptsPasteWhileWaitingForOwnerReview(t *testing.T) {
	notes := make(chan string, 4)
	ui := newDuelUI(notes, make(chan duelUpdate, 4), func() {}, func() {})
	ui.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	ui.Update(duelUpdate{done: true, result: duel.Result{Status: "owner_review", Summary: "ready for acceptance"}})

	pasted := "請協助我去驗收 ss\n並確認測試結果"
	ui.Update(tea.PasteMsg{Content: pasted})
	if got := ui.input.Value(); got != pasted {
		t.Fatalf("pasted owner instruction=%q want=%q", got, pasted)
	}

	ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := <-notes; got != pasted {
		t.Fatalf("submitted pasted owner instruction=%q want=%q", got, pasted)
	}
	if ui.stopped {
		t.Fatal("duel did not restart after pasted owner instruction")
	}
}

func TestDuelOwnerReviewRingsExactlyOnce(t *testing.T) {
	ui := newDuelUI(make(chan string, 1), make(chan duelUpdate, 1), func() {}, func() {})
	_, cmd := ui.Update(duelUpdate{done: true, result: duel.Result{Status: "owner_review"}})
	if cmd == nil {
		t.Fatal("owner review produced no terminal notification")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected batched focus and bell commands, got %T", cmd())
	}
	bellCount := 0
	for _, c := range batch {
		if raw, ok := c().(tea.RawMsg); ok && raw.Msg == "\a" {
			bellCount++
		}
	}
	if bellCount != 1 {
		t.Fatalf("owner review bells = %d, want 1", bellCount)
	}
	_, repeat := ui.Update(duelUpdate{done: true, result: duel.Result{Status: "owner_review"}})
	if _, ok := repeat().(tea.BatchMsg); ok {
		t.Fatal("repeated owner review rang again")
	}
}

func TestDuelBlockedDoesNotRing(t *testing.T) {
	ui := newDuelUI(make(chan string, 1), make(chan duelUpdate, 1), func() {}, func() {})
	_, cmd := ui.Update(duelUpdate{done: true, result: duel.Result{Status: "blocked"}})
	if _, ok := cmd().(tea.BatchMsg); ok {
		t.Fatal("blocked state rang terminal bell")
	}
}

func TestDuelErrorShowsRetryHint(t *testing.T) {
	ui := newDuelUI(make(chan string, 1), make(chan duelUpdate, 1), func() {}, func() {})
	ui.Update(duelUpdate{done: true, err: errors.New("left round 44: provider unavailable")})
	if !ui.stopped {
		t.Fatal("error should stop current duel run")
	}
	if !strings.Contains(ui.status, "provider unavailable") || !strings.Contains(ui.status, "Ctrl+R") {
		t.Fatalf("error status missing diagnostic/retry hint: %q", ui.status)
	}
}

func TestDuelTurnHeaderAndMarkdown(t *testing.T) {
	localTime := time.Date(2026, 10, 27, 15, 42, 0, 0, time.Local)
	got := renderDuelTurn(duelUpdate{role: "left", round: 32, roundTime: localTime, turn: duel.Turn{Action: "continue", Summary: "**done** and `code`", Evidence: "- test passed"}}, 65)
	plain := ansi.Strip(got)
	for _, want := range []string{"ROUND 32", "2026-10-27 15:42", "CONTINUE", "done", "code", "Evidence", "test passed"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("missing %q in %q", want, plain)
		}
	}
	if strings.Contains(plain, "**done**") {
		t.Fatalf("markdown not rendered: %q", plain)
	}
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("expected styled round header: %q", got)
	}
}

func TestDuelTurnHeaderSupportsLegacyRoundWithoutTimestamp(t *testing.T) {
	got := ansi.Strip(renderDuelTurn(duelUpdate{role: "left", round: 7, turn: duel.Turn{Action: "continue", Summary: "legacy"}}, 65))
	if !strings.Contains(got, "ROUND 07") || strings.Contains(got, "0001-") {
		t.Fatalf("legacy round header=%q", got)
	}
}

func TestDuelActionColors(t *testing.T) {
	tests := map[string]string{
		"plan":           "#75AFFF",
		"research":       "#C39BFF",
		"continue":       "#6ED6AA",
		"blocked":        "#E6A34B",
		"propose_review": "#E3C86F",
		"agree_review":   "#E3C86F",
		"unknown":        "#7DAEA3",
	}
	for action, want := range tests {
		if got := duelActionColor(action); got != want {
			t.Fatalf("action %q color=%q want=%q", action, got, want)
		}
	}
}

func TestDuelViewportMouseScrolling(t *testing.T) {
	ui := newDuelUI(make(chan string, 1), make(chan duelUpdate, 1), func() {}, func() {})
	ui.Update(tea.WindowSizeMsg{Width: 90, Height: 24})
	if !ui.View().AltScreen || ui.View().MouseMode == tea.MouseModeNone {
		t.Fatal("duel must capture terminal wheel events")
	}
	ui.left.SetContent(strings.Repeat("left line\n", 100))
	ui.right.SetContent(strings.Repeat("right line\n", 100))
	ui.left.GotoBottom()
	ui.right.GotoBottom()
	rightBefore := ui.right.YOffset()
	leftBefore := ui.left.YOffset()
	ui.Update(tea.MouseWheelMsg{X: 5, Y: 8, Button: tea.MouseWheelUp})
	if ui.left.YOffset() >= leftBefore {
		t.Fatal("left pane did not scroll")
	}
	if ui.right.YOffset() != rightBefore {
		t.Fatal("right pane scrolled with left")
	}
}

func TestDuelBlockedDialogDismissesWithoutResuming(t *testing.T) {
	ui := newDuelUI(make(chan string, 1), make(chan duelUpdate, 1), func() {}, func() {})
	ui.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	ui.Update(duelUpdate{done: true, result: duel.Result{Status: "blocked", Summary: "Awaiting owner authorization"}})
	if !strings.Contains(ui.View().Content, "[ OK ]") || ui.modal == "" {
		t.Fatal("blocked dialog missing")
	}
	layout := ui.modalLayout()
	ui.Update(tea.MouseClickMsg{X: layout.okX1, Y: layout.okY + 1, Button: tea.MouseLeft})
	if ui.modal == "" {
		t.Fatal("click outside OK unexpectedly dismissed dialog")
	}
	ui.Update(tea.MouseClickMsg{X: layout.okX1, Y: layout.okY, Button: tea.MouseLeft})
	if ui.modal != "" || !ui.stopped {
		t.Fatal("OK dismissal should not restart blocked duel")
	}
	if !ui.input.Focused() {
		t.Fatal("input should regain focus after dialog dismissal")
	}
}

func TestDuelBlockedDialogKeyboardDismissesAndRestoresFocus(t *testing.T) {
	ui := newDuelUI(make(chan string, 1), make(chan duelUpdate, 1), func() {}, func() {})
	ui.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	ui.Update(duelUpdate{done: true, result: duel.Result{Status: "blocked", Summary: "Awaiting owner authorization"}})
	ui.input.Blur()
	ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if ui.modal != "" || !ui.input.Focused() {
		t.Fatal("keyboard dismissal should close dialog and restore input focus")
	}
}
