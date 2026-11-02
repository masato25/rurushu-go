package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/masato25/rurushu-go/duel"
	"github.com/masato25/rurushu-go/provider"
	"github.com/masato25/rurushu-go/tui"
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
	ui.running = true

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
	if view := ui.View().Content; !strings.Contains(view, "待 LEFT 消費 2 則") {
		t.Fatalf("queue depth not visible in TUI: %q", view)
	}
	if got := <-notes; got != "first steering note" {
		t.Fatalf("first queued instruction=%q", got)
	}
	if got := <-notes; got != "second steering note" {
		t.Fatalf("second queued instruction=%q", got)
	}
}

func TestDuelTUIOwnerInstructionStartsLeftWhenNoRunIsActive(t *testing.T) {
	notes := make(chan string, 4)
	runs := 0
	ui := newDuelUI(notes, make(chan duelUpdate, 4), func() { runs++ }, func() {})
	ui.stopped = true
	ui.running = false
	ui.input.SetValue("commit")
	_, cmd := ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if runs != 1 {
		t.Fatalf("runs=%d want=1", runs)
	}
	if ui.stopped || !strings.Contains(ui.status, "立即啟動 LEFT") {
		t.Fatalf("stopped=%v status=%q", ui.stopped, ui.status)
	}
	if cmd == nil {
		t.Fatal("expected update waiter after starting LEFT")
	}
}

func TestDuelTUIOwnerInstructionRestartsAfterRetryRunFinishes(t *testing.T) {
	notes := make(chan string, 4)
	updates := make(chan duelUpdate, 4)
	runs, stops := 0, 0
	ui := newDuelUI(notes, updates, func() { runs++ }, func() {})
	ui.setStopLoopHandler(func() { stops++ })
	ui.running = true
	ui.retrying = true
	ui.input.SetValue("commit")
	_, cmd := ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if stops != 1 || !ui.restartAfterDone || runs != 0 {
		t.Fatalf("stops=%d restart=%v runs=%d", stops, ui.restartAfterDone, runs)
	}
	if cmd == nil {
		t.Fatal("expected waiter for canceled run completion")
	}
	_, cmd = ui.Update(duelUpdate{done: true, err: context.Canceled})
	if runs != 1 || ui.restartAfterDone {
		t.Fatalf("runs=%d restart=%v", runs, ui.restartAfterDone)
	}
	if cmd == nil {
		t.Fatal("expected waiter after restarted LEFT")
	}
}

func TestDuelTUIOwnerInstructionIsPersistedBeforeQueueing(t *testing.T) {
	notes := make(chan string, 4)
	ui := newDuelUI(notes, make(chan duelUpdate, 4), func() {}, func() {})
	var persisted []string
	ui.setOwnerQueueHandler(func(note string) error {
		persisted = append(persisted, note)
		return nil
	})
	ui.input.SetValue("commit current changes")
	ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(persisted) != 1 || persisted[0] != "commit current changes" {
		t.Fatalf("persisted=%v", persisted)
	}
	if got := <-notes; got != "commit current changes" {
		t.Fatalf("queued=%q", got)
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

func TestDuelSlashAutocompleteCompletesBeforeExecuting(t *testing.T) {
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	ui.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	ui.input.SetValue("/he")
	ui.updateSlashAutocomplete()

	if len(ui.slashMatches) != 1 || ui.slashMatches[0].Name != "help" {
		t.Fatalf("matches=%#v", ui.slashMatches)
	}
	if view := ui.View().Content; !strings.Contains(view, "/help") {
		t.Fatalf("autocomplete not rendered: %q", view)
	}

	ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := ui.input.Value(); got != "/help" {
		t.Fatalf("completed input=%q", got)
	}
	if len(ui.workLines) != 0 {
		t.Fatalf("partial command executed: %#v", ui.workLines)
	}

	ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(ui.workLines) != 1 || !strings.Contains(ui.workLines[0], "DUEL COMMANDS") {
		t.Fatalf("/help did not execute: %#v", ui.workLines)
	}
}

func TestDuelSlashAutocompleteNavigationAndTab(t *testing.T) {
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	ui.input.SetValue("/")
	ui.updateSlashAutocomplete()
	if len(ui.slashMatches) < 2 {
		t.Fatalf("matches=%#v", ui.slashMatches)
	}
	ui.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	want := "/" + ui.slashMatches[ui.slashMatchIndex].Name
	ui.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if got := ui.input.Value(); got != want {
		t.Fatalf("tab completion=%q want=%q", got, want)
	}
}

func TestDuelStopSlashCancelsLoopWithoutQuitting(t *testing.T) {
	stops := 0
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	ui.setStopLoopHandler(func() { stops++ })
	ui.input.SetValue("/stop")
	_, cmd := ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/stop should keep waiting for the run completion update")
	}
	if stops != 1 {
		t.Fatalf("stop handler calls=%d", stops)
	}
	if ui.switchMode != "" {
		t.Fatalf("/stop unexpectedly switched mode: %q", ui.switchMode)
	}
	if !strings.Contains(ui.status, "正在停止") {
		t.Fatalf("status=%q", ui.status)
	}
}

func TestDuelSingleSlashStopsLoopAndRequestsModeSwitch(t *testing.T) {
	stops := 0
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	ui.setStopLoopHandler(func() { stops++ })
	ui.input.SetValue("/single")
	_, cmd := ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/single did not request TUI quit")
	}
	if stops != 1 {
		t.Fatalf("stop handler calls=%d", stops)
	}
	if ui.switchMode != "single" {
		t.Fatalf("switch mode=%q", ui.switchMode)
	}
}

func TestDuelCanceledRunShowsStoppedLoopStatus(t *testing.T) {
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	ui.Update(duelUpdate{done: true, err: context.Canceled})
	if !ui.stopped || !strings.Contains(ui.status, "loop 已停止") {
		t.Fatalf("stopped=%v status=%q", ui.stopped, ui.status)
	}
}

func TestDuelInitialLeftHistoryIsVisible(t *testing.T) {
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	ui.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	ui.setInitialLeftHistory([]provider.Message{
		{Role: provider.RoleUser, Content: "single question"},
		{Role: provider.RoleAssistant, Content: "single answer"},
	})
	view := ansi.Strip(ui.View().Content)
	for _, want := range []string{"CONTINUED FROM SINGLE", "single question", "single answer"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q: %q", want, view)
		}
	}
}

func TestDuelInitialLeftHistoryRendersMarkdownLikeSingleChat(t *testing.T) {
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	ui.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	ui.setInitialLeftHistory([]provider.Message{
		{Role: provider.RoleAssistant, Content: "# Result\n**bold** and `code`\n- item"},
	})
	plain := ansi.Strip(ui.View().Content)
	for _, want := range []string{"Result", "bold", "code", "item"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("rendered history missing %q: %q", want, plain)
		}
	}
	for _, raw := range []string{"# Result", "**bold**"} {
		if strings.Contains(plain, raw) {
			t.Fatalf("history still contains raw markdown %q: %q", raw, plain)
		}
	}
}

func TestDuelInitialRightHistoryIsRestoredToTimeline(t *testing.T) {
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	ui.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	ui.setInitialRightHistory([]duelPersistedTurn{{
		Round:   6,
		Time:    time.Date(2026, 11, 2, 10, 52, 0, 0, time.Local),
		Action:  "feedback",
		Summary: "Proceed with instrumentation before further optimization.",
	}})
	plain := ansi.Strip(ui.View().Content)
	for _, want := range []string{"RIGHT", "ROUND 06", "2026-11-02 10:52", "Proceed with instrumentation"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("restored RIGHT timeline missing %q: %q", want, plain)
		}
	}
}

func TestParseDuelNewSessionCommand(t *testing.T) {
	for _, tc := range []struct {
		input string
		ok    bool
	}{
		{"/new", true},
		{" /NEW ", true},
		{"/new left", false},
		{"/new right", false},
	} {
		if got := isDuelNewSessionCommand(tc.input); got != tc.ok {
			t.Fatalf("parse %q = %v, want %v", tc.input, got, tc.ok)
		}
	}
}

func TestDuelTUINewSessionRequiresStoppedDuel(t *testing.T) {
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	called := false
	ui.setNewSessionHandler(func() (string, string, error) {
		called = true
		return "ses-left-new", "ses-right-new", nil
	})
	ui.input.SetValue("/new")
	ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if called {
		t.Fatal("running duel replaced a session")
	}
	if !strings.Contains(ui.status, "只能在 duel 停止後使用") {
		t.Fatalf("status=%q", ui.status)
	}
}

func TestDuelTUINewSessionReplacesBothStoppedSessionsAndRestarts(t *testing.T) {
	runCalled := make(chan struct{}, 1)
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() { runCalled <- struct{}{} }, func() {}, "ses-left-old", "ses-right")
	ui.stopped = true
	ui.setNewSessionHandler(func() (string, string, error) {
		return "ses-left-new", "ses-right-new", nil
	})
	ui.input.SetValue("/new")
	_, cmd := ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("restart did not arm update listener")
	}
	if ui.leftID != "ses-left-new" || ui.rightID != "ses-right-new" || ui.stopped {
		t.Fatalf("leftID=%q rightID=%q stopped=%v", ui.leftID, ui.rightID, ui.stopped)
	}
	select {
	case <-runCalled:
	case <-time.After(time.Second):
		t.Fatal("duel run was not restarted")
	}
}

func TestDuelTUIShowsAutomaticRetryStatus(t *testing.T) {
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	_, cmd := ui.Update(duelUpdate{role: "right", round: 2, retry: true, retryAfter: time.Minute, err: errors.New("COS unavailable")})
	if cmd == nil {
		t.Fatal("retry status did not keep listening for updates")
	}
	if !strings.Contains(ui.status, "RIGHT session") || !strings.Contains(ui.status, "1m0s") {
		t.Fatalf("status=%q", ui.status)
	}
}

func TestDuelTUIShowsActiveRoleStatus(t *testing.T) {
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	_, cmd := ui.Update(duelUpdate{role: "left", round: 3, active: true})
	if cmd == nil {
		t.Fatal("active role status did not keep listening for updates")
	}
	if ui.status != "LEFT 執行中 · ROUND 03" {
		t.Fatalf("status=%q", ui.status)
	}

	_, cmd = ui.Update(duelUpdate{role: "right", round: 3, active: true})
	if cmd == nil {
		t.Fatal("active role status did not keep listening for updates")
	}
	if ui.status != "RIGHT 執行中 · ROUND 03" {
		t.Fatalf("status=%q", ui.status)
	}
}

func TestDuelTUIFirstInputInitializesGoalAndStarts(t *testing.T) {
	runCalled := make(chan struct{}, 1)
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() { runCalled <- struct{}{} }, func() {})
	var goal string
	ui.setInitialGoalHandler(func(value string) error {
		goal = value
		ui.leftID = "ses-left"
		ui.rightID = "ses-right"
		return nil
	})
	if cmd := ui.Init(); cmd == nil {
		t.Fatal("waiting-for-goal TUI should still focus input")
	}
	ui.input.SetValue("build the next feature")
	_, cmd := ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("initial goal did not arm duel update listener")
	}
	if goal != "build the next feature" || ui.waitingForGoal || ui.stopped {
		t.Fatalf("goal=%q waiting=%v stopped=%v", goal, ui.waitingForGoal, ui.stopped)
	}
	select {
	case <-runCalled:
	case <-time.After(time.Second):
		t.Fatal("duel did not start after initial goal")
	}
}

func TestDuelTUIRoutesFunctionKeysToAuxiliaryWorkspace(t *testing.T) {
	root := t.TempDir()
	service := tui.NewTerminalService(root)
	defer service.Close()
	uiModel := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {})
	aux := tui.NewAuxiliaryWorkspace(root, service)
	uiModel.setAuxiliaryWorkspace(aux)
	uiModel.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	uiModel.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if !aux.Active() || !strings.Contains(uiModel.View().Content, "rurushu setup") {
		t.Fatal("duel F2 did not open shared setup panel")
	}
	uiModel.Update(tea.KeyPressMsg{Code: tea.KeyF2})

	uiModel.Update(tea.KeyPressMsg{Code: tea.KeyF5})
	if !aux.Active() || !strings.Contains(uiModel.View().Content, "Files") {
		t.Fatal("duel F5 did not open shared file browser")
	}
}

func TestDuelTUIRendersSingleTimeline(t *testing.T) {
	ui := newDuelUI(make(chan string, 4), make(chan duelUpdate, 4), func() {}, func() {}, "ses-left", "ses-right")
	ui.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	ui.Update(duelUpdate{role: "right", round: 1, turn: duel.Turn{Action: "feedback", Summary: "review"}})
	ui.Update(duelUpdate{role: "left", round: 1, turn: duel.Turn{Action: "work", Summary: "implemented"}})
	view := ui.View().Content
	for _, term := range []string{"單一工作區", "ses-left", "ses-right", "RIGHT", "LEFT", "review", "implemented"} {
		if !strings.Contains(view, term) {
			t.Fatalf("missing %q in %s", term, view)
		}
	}
	if strings.Index(view, "review") > strings.Index(view, "implemented") {
		t.Fatalf("timeline did not preserve insertion order: %s", view)
	}
	ui.Update(duelUpdate{done: true, result: duel.Result{Status: "owner_review", Summary: strings.Repeat("very long review summary ", 100)}})
	if !ui.stopped || !strings.Contains(ui.status, "等待主人驗收") {
		t.Fatalf("unexpected finish state: %s", ui.status)
	}
	if strings.Contains(ui.status, "very long review summary") {
		t.Fatalf("owner review status should not duplicate long summary: %q", ui.status)
	}
	if !strings.Contains(ui.View().Content, "請直接在下方輸入指令") {
		t.Fatalf("owner review should keep a concise input hint visible: %q", ui.View().Content)
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
	view := ansi.Strip(ui.View().Content)
	if !strings.Contains(view, "OWNER · INSTRUCTION") || !strings.Contains(view, "請協助我去驗收 ss") || !strings.Contains(view, "並確認測試結果") {
		t.Fatalf("submitted owner instruction missing from timeline: %q", view)
	}
	if ui.stopped {
		t.Fatal("duel did not restart after pasted owner instruction")
	}
}

func TestDuelTUIShowsQueuedOwnerInstructionInTimeline(t *testing.T) {
	notes := make(chan string, 4)
	ui := newDuelUI(notes, make(chan duelUpdate, 4), func() {}, func() {})
	ui.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	ui.Update(duelUpdate{role: "right", round: 4, turn: duel.Turn{Action: "feedback", Summary: "please verify"}})
	ui.input.SetValue("驗收完成，繼續優化")
	ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	view := ansi.Strip(ui.View().Content)
	if !strings.Contains(view, "RIGHT · ROUND 04") || !strings.Contains(view, "OWNER · INSTRUCTION") || !strings.Contains(view, "驗收完成，繼續優化") {
		t.Fatalf("timeline missing owner instruction context: %q", view)
	}
	if strings.Index(view, "OWNER · INSTRUCTION") < strings.Index(view, "RIGHT · ROUND 04") {
		t.Fatalf("owner instruction appeared before preceding RIGHT feedback: %q", view)
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
	ui.work.SetContent(strings.Repeat("timeline line\n", 100))
	ui.work.GotoBottom()
	before := ui.work.YOffset()
	ui.Update(tea.MouseWheelMsg{X: 5, Y: 8, Button: tea.MouseWheelUp})
	if ui.work.YOffset() >= before {
		t.Fatal("timeline did not scroll")
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
