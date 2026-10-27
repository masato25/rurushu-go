package main

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
