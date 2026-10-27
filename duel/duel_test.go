package duel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/masato25/rurushu-go/provider"
)

type scripted struct {
	calls    []string
	requests []provider.CompletionRequest
	outputs  []string
}

func (s *scripted) Stream(_ context.Context, req provider.CompletionRequest) (<-chan provider.StreamEvent, error) {
	s.calls = append(s.calls, req.SessionID)
	s.requests = append(s.requests, req)
	if len(s.outputs) == 0 {
		return nil, fmt.Errorf("unexpected stream")
	}
	output := s.outputs[0]
	s.outputs = s.outputs[1:]
	ch := make(chan provider.StreamEvent, 2)
	ch <- provider.StreamEvent{Type: provider.EventReplace, Text: output}
	ch <- provider.StreamEvent{Type: provider.EventDone}
	close(ch)
	return ch, nil
}

type eventScript struct {
	events []provider.StreamEvent
	err    error
}

type lifecycleOutcome struct {
	output string
	err    error
}

type lifecycleSend struct {
	localID, remoteID, prompt string
}

type lifecycleStore struct {
	remote  map[string]string
	creates int
	sends   []lifecycleSend
}

type lifecycleStreamer struct {
	store    *lifecycleStore
	outcomes []lifecycleOutcome
}

func (s *lifecycleStreamer) Stream(_ context.Context, req provider.CompletionRequest) (<-chan provider.StreamEvent, error) {
	if s.store.remote == nil {
		s.store.remote = map[string]string{}
	}
	remoteID := s.store.remote[req.SessionID]
	if remoteID == "" {
		s.store.creates++
		remoteID = fmt.Sprintf("remote-%d", s.store.creates)
		s.store.remote[req.SessionID] = remoteID
	}
	prompt := ""
	if len(req.Messages) > 0 {
		prompt = req.Messages[len(req.Messages)-1].Content
	}
	s.store.sends = append(s.store.sends, lifecycleSend{localID: req.SessionID, remoteID: remoteID, prompt: prompt})
	if len(s.outcomes) == 0 {
		return nil, errors.New("unexpected lifecycle send")
	}
	outcome := s.outcomes[0]
	s.outcomes = s.outcomes[1:]
	if outcome.err != nil {
		return nil, outcome.err
	}
	ch := make(chan provider.StreamEvent, 2)
	ch <- provider.StreamEvent{Type: provider.EventReplace, Text: outcome.output}
	ch <- provider.StreamEvent{Type: provider.EventDone}
	close(ch)
	return ch, nil
}

func (s eventScript) Stream(_ context.Context, _ provider.CompletionRequest) (<-chan provider.StreamEvent, error) {
	if s.err != nil {
		return nil, s.err
	}
	ch := make(chan provider.StreamEvent, len(s.events))
	for _, ev := range s.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}
func TestRunStopsOnlyAfterBothApprove(t *testing.T) {
	s := &scripted{outputs: []string{
		`{"action":"plan","summary":"more work","next":"implement","evidence":"none"}`,
		`{"action":"agree_review","summary":"claimed ready","evidence":"manual"}`,
		`{"action":"propose_review","summary":"ready","evidence":"tests pass"}`,
		`{"action":"agree_review","summary":"verified ready","evidence":"tests pass"}`,
	}}
	res, err := Run(context.Background(), s, Options{Goal: "build feature", LeftID: "left", RightID: "right"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rounds != 2 || res.Status != "owner_review" {
		t.Fatalf("result: %+v", res)
	}
	if strings.Join(s.calls, ",") != "right,left,right,left" {
		t.Fatalf("sessions: %v", s.calls)
	}
}

func TestRunBlocksWithoutSpinning(t *testing.T) {
	s := &scripted{outputs: []string{
		`{"action":"blocked","summary":"waiting for owner authorization","next":"owner approval","evidence":"no new work"}`,
		`{"action":"blocked","summary":"no safe task remaining","next":"owner approval","evidence":"no new work"}`,
	}}
	result, err := Run(context.Background(), s, Options{Goal: "improve", LeftID: "left", RightID: "right"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "blocked" || result.Rounds != 1 || len(s.calls) != 2 {
		t.Fatalf("unexpected blocked result: %+v calls=%v", result, s.calls)
	}
}
func TestRunCancellationIsNotApproval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, &scripted{}, Options{Goal: "task", LeftID: "a", RightID: "b"})
	if err != context.Canceled {
		t.Fatalf("expected cancellation: %v", err)
	}
}

func TestRunContinuesWhenLeftRejectsProposedReview(t *testing.T) {
	s := &scripted{outputs: []string{
		`{"action":"propose_review","summary":"looks ready","next":"verify","evidence":"tests pass"}`,
		`{"action":"continue","summary":"not ready","next":"fix edge case","evidence":"missing coverage"}`,
		`{"action":"propose_review","summary":"ready now","next":"verify","evidence":"coverage added"}`,
		`{"action":"agree_review","summary":"verified","next":"owner review","evidence":"all tests pass"}`,
	}}
	res, err := Run(context.Background(), s, Options{Goal: "task", LeftID: "left", RightID: "right"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rounds != 2 || res.Status != "owner_review" {
		t.Fatalf("result: %+v", res)
	}
}

func TestRunRejectsInvalidActions(t *testing.T) {
	t.Run("right", func(t *testing.T) {
		s := &scripted{outputs: []string{
			`{"action":"agree_review","summary":"wrong role action","next":"none","evidence":"none"}`,
		}}
		if _, err := Run(context.Background(), s, Options{Goal: "task", LeftID: "left", RightID: "right"}); err == nil || !strings.Contains(err.Error(), "invalid action") {
			t.Fatalf("expected invalid action error, got %v", err)
		}
	})
	t.Run("left", func(t *testing.T) {
		s := &scripted{outputs: []string{
			`{"action":"plan","summary":"work","next":"implement","evidence":"none"}`,
			`{"action":"propose_review","summary":"wrong role action","next":"none","evidence":"none"}`,
		}}
		if _, err := Run(context.Background(), s, Options{Goal: "task", LeftID: "left", RightID: "right"}); err == nil || !strings.Contains(err.Error(), "invalid action") {
			t.Fatalf("expected invalid action error, got %v", err)
		}
	})
}

func TestRunCarriesInitialSummaryAndSessionIDs(t *testing.T) {
	s := &scripted{outputs: []string{
		`{"action":"propose_review","summary":"ready","next":"verify","evidence":"tests pass"}`,
		`{"action":"agree_review","summary":"verified","next":"owner review","evidence":"tests pass"}`,
	}}
	_, err := Run(context.Background(), s, Options{
		Goal:           "task",
		LeftID:         "saved-left",
		RightID:        "saved-right",
		InitialSummary: "persisted summary from previous run",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.calls, ","); got != "saved-right,saved-left" {
		t.Fatalf("sessions: %s", got)
	}
	if len(s.requests) == 0 || len(s.requests[0].Messages) == 0 || !strings.Contains(s.requests[0].Messages[0].Content, "persisted summary from previous run") {
		t.Fatalf("initial summary missing from right prompt: %+v", s.requests)
	}
}

func TestRunInjectsOwnerInstructionAfterRightTurn(t *testing.T) {
	instructions := make(chan string, 2)
	s := &scripted{outputs: []string{
		`{"action":"plan","summary":"right decided","next":"left should execute","evidence":"none"}`,
		`{"action":"continue","summary":"left executed","next":"keep going","evidence":"checked owner note"}`,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Run(ctx, s, Options{
		Goal:         "task",
		LeftID:       "left",
		RightID:      "right",
		Instructions: instructions,
		OnTurn: func(role string, _ int, _ Turn) error {
			if role == "right" {
				instructions <- "OWNER_NOTE_42"
			}
			if role == "left" {
				cancel()
			}
			return nil
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation after first round, got %v", err)
	}
	if len(s.requests) < 2 || len(s.requests[1].Messages) == 0 || !strings.Contains(s.requests[1].Messages[0].Content, "OWNER_NOTE_42") {
		t.Fatalf("owner instruction missing from left prompt: %+v", s.requests)
	}
}

func TestSessionLifecycleReusesExactlyTwoRemoteSessionsAcrossErrorAndResume(t *testing.T) {
	store := &lifecycleStore{}
	instructions := make(chan string, 2)
	first := &lifecycleStreamer{store: store, outcomes: []lifecycleOutcome{
		{output: "{\"action\":\"plan\",\"summary\":\"round one\",\"next\":\"execute\",\"evidence\":\"none\"}"},
		{output: "{\"action\":\"continue\",\"summary\":\"left one\",\"next\":\"continue\",\"evidence\":\"OWNER_NOTE_42 seen\"}"},
		{output: "{\"action\":\"plan\",\"summary\":\"round two\",\"next\":\"execute\",\"evidence\":\"none\"}"},
		{err: errors.New("Chat On Steroids input error (429): rate_limited")},
	}}
	firstLeftSummary := ""
	_, err := Run(context.Background(), first, Options{
		Goal:         "lifecycle regression",
		LeftID:       "local-left",
		RightID:      "local-right",
		Instructions: instructions,
		OnTurn: func(role string, _ int, turn Turn) error {
			if role == "right" && turn.Summary == "round one" {
				instructions <- "OWNER_NOTE_42"
			}
			if role == "left" {
				firstLeftSummary = turn.Summary
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected simulated 429, got %v", err)
	}
	if store.creates != 2 {
		t.Fatalf("remote creates after first run = %d, want 2", store.creates)
	}

	second := &lifecycleStreamer{store: store, outcomes: []lifecycleOutcome{
		{output: "{\"action\":\"propose_review\",\"summary\":\"ready after resume\",\"next\":\"verify\",\"evidence\":\"reused sessions\"}"},
		{output: "{\"action\":\"agree_review\",\"summary\":\"verified after resume\",\"next\":\"owner review\",\"evidence\":\"reused sessions\"}"},
	}}
	res, err := Run(context.Background(), second, Options{
		Goal:           "lifecycle regression",
		LeftID:         "local-left",
		RightID:        "local-right",
		InitialSummary: firstLeftSummary,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "owner_review" {
		t.Fatalf("resume result = %+v", res)
	}
	if store.creates != 2 {
		t.Fatalf("remote creates after resume = %d, want exactly 2", store.creates)
	}
	if got := store.remote["local-left"]; got != "remote-2" {
		t.Fatalf("left remote = %q, want remote-2", got)
	}
	if got := store.remote["local-right"]; got != "remote-1" {
		t.Fatalf("right remote = %q, want remote-1", got)
	}
	for _, send := range store.sends {
		want := store.remote[send.localID]
		if send.remoteID != want {
			t.Fatalf("session changed for %s: send=%s want=%s", send.localID, send.remoteID, want)
		}
	}
	if len(store.sends) < 2 || !strings.Contains(store.sends[1].prompt, "OWNER_NOTE_42") {
		t.Fatalf("owner instruction missing from LEFT send: %+v", store.sends)
	}
}

func TestCallPropagatesStreamFailures(t *testing.T) {
	t.Run("stream error", func(t *testing.T) {
		want := errors.New("provider failed")
		_, err := call(context.Background(), eventScript{err: want}, "right", "review")
		if !errors.Is(err, want) {
			t.Fatalf("expected provider error, got %v", err)
		}
	})
	t.Run("event error", func(t *testing.T) {
		want := errors.New("event failed")
		_, err := call(context.Background(), eventScript{events: []provider.StreamEvent{{Type: provider.EventError, Error: want}}}, "right", "review")
		if !errors.Is(err, want) {
			t.Fatalf("expected event error, got %v", err)
		}
	})
	t.Run("event error without detail", func(t *testing.T) {
		_, err := call(context.Background(), eventScript{events: []provider.StreamEvent{{Type: provider.EventError}}}, "right", "review")
		if err == nil || !strings.Contains(err.Error(), "COS stream failed") {
			t.Fatalf("expected generic stream error, got %v", err)
		}
	})
	t.Run("missing done", func(t *testing.T) {
		_, err := call(context.Background(), eventScript{events: []provider.StreamEvent{{Type: provider.EventReplace, Text: `{"action":"plan","summary":"valid","next":"work","evidence":"none"}`}}}, "right", "review")
		if err == nil || !strings.Contains(err.Error(), "without completion") {
			t.Fatalf("expected incomplete stream error, got %v", err)
		}
	})
}

func TestCallRejectsInvalidJSONAfterRetries(t *testing.T) {
	s := &scripted{outputs: []string{"not json", "{broken", "still not json"}}
	_, err := call(context.Background(), s, "right", "review")
	if err == nil || len(s.calls) != 3 {
		t.Fatalf("expected malformed response after three attempts, got err=%v calls=%d", err, len(s.calls))
	}
}

func TestParseTurnAfterCOSToolActivity(t *testing.T) {
	raw := "> ⚙️ `[Tool: exec_command]` Ran 3 commands: git status; go test\n\n" +
		"Checked the current tree.\n```json\n" +
		`{"action":"plan","summary":"Tests are green","next":"review errors","evidence":"go test ./... passed"}` + "\n```"
	got, err := parseTurn(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "plan" || got.Summary != "Tests are green" {
		t.Fatalf("unexpected turn: %+v", got)
	}
}

func TestParseTurnUsesLatestCompleteDecision(t *testing.T) {
	raw := `{"action":"plan","summary":"old"}` + "\n> ⚙️ `[Tool: read]` file\n" +
		`{"action":"research","summary":"new","next":"search docs"}`
	got, err := parseTurn(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "research" || got.Summary != "new" {
		t.Fatalf("unexpected turn: %+v", got)
	}
}

func TestParseTurnRejectsNonDecisions(t *testing.T) {
	for _, raw := range []string{"> ⚙️ `[Tool: exec_command]` ran tests", `{"action":"plan"}`, "{truncated"} {
		if _, err := parseTurn(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestCallRecoversFromContentReferenceOnly(t *testing.T) {
	s := &scripted{outputs: []string{
		`::chatgpt-content-reference{index="0" source_message_id="abc"}`,
		`{"action":"plan","summary":"continue work","next":"run tests","evidence":"not yet verified"}`,
	}}
	turn, err := call(context.Background(), s, "right", "review the goal")
	if err != nil {
		t.Fatal(err)
	}
	if turn.Action != "plan" || len(s.calls) != 2 {
		t.Fatalf("failed to recover: %+v calls=%d", turn, len(s.calls))
	}
}

func TestCallDoesNotRetryForeverOnContentReferences(t *testing.T) {
	s := &scripted{outputs: []string{"::chatgpt-content-reference", "::chatgpt-content-reference", "::chatgpt-content-reference"}}
	_, err := call(context.Background(), s, "right", "review")
	if err == nil || len(s.calls) != 3 {
		t.Fatalf("unexpected retry result: %v calls=%d", err, len(s.calls))
	}
}
