package duel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/masato25/rurushu-go/provider"
)

type scripted struct {
	calls    []string
	requests []provider.CompletionRequest
	outputs  []string
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	close(ch)
	return ch
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

func TestRunRetriesRoleOnceAfterConfiguredDelay(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	streamer := &lifecycleStreamer{store: &lifecycleStore{}, outcomes: []lifecycleOutcome{
		{err: errors.New("temporary COS failure")},
		{output: `{"action":"propose_review","summary":"ready","next":"review","evidence":"ok"}`},
		{output: `{"action":"agree_review","summary":"verified","next":"owner","evidence":"ok"}`},
	}}
	var retries []string
	res, err := Run(context.Background(), streamer, Options{
		Goal:       "recover",
		LeftID:     "left",
		RightID:    "right",
		RetryDelay: time.Minute,
		RetryClock: clock,
		OnRetry: func(role string, round int, delay time.Duration, err error) error {
			retries = append(retries, fmt.Sprintf("%s:%d:%s:%v", role, round, delay, err))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "owner_review" {
		t.Fatalf("status=%q", res.Status)
	}
	if len(retries) != 1 || !strings.Contains(retries[0], "right:1:1m0s:temporary COS failure") {
		t.Fatalf("retries=%#v", retries)
	}
	if got := clock.Now(); !got.Equal(time.Unix(1000, 0).Add(time.Minute)) {
		t.Fatalf("clock=%v", got)
	}
	if len(streamer.store.sends) != 3 || streamer.store.sends[0].localID != "right" || streamer.store.sends[1].localID != "right" {
		t.Fatalf("unexpected sends=%#v", streamer.store.sends)
	}
}

func TestRunStopsAfterRetryAlsoFails(t *testing.T) {
	clock := &fakeClock{now: time.Unix(2000, 0)}
	streamer := &lifecycleStreamer{store: &lifecycleStore{}, outcomes: []lifecycleOutcome{
		{err: errors.New("first failure")},
		{err: errors.New("second failure")},
	}}
	_, err := Run(context.Background(), streamer, Options{
		Goal: "recover", LeftID: "left", RightID: "right", RetryDelay: time.Minute, RetryClock: clock,
	})
	if err == nil || !strings.Contains(err.Error(), "retry after 1m0s failed: second failure") {
		t.Fatalf("err=%v", err)
	}
	if len(streamer.store.sends) != 2 {
		t.Fatalf("send count=%d want=2", len(streamer.store.sends))
	}
}

func TestRunRecurringThreeTicksProduceThreeRightDecisions(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: start}
	s := &scripted{outputs: []string{
		`{"action":"plan","summary":"r1","next":"work","evidence":"e1"}`,
		`{"action":"continue","summary":"l1","next":"again","evidence":"done1"}`,
		`{"action":"research","summary":"r2","next":"research","evidence":"e2"}`,
		`{"action":"continue","summary":"l2","next":"again","evidence":"done2"}`,
		`{"action":"plan","summary":"r3","next":"work","evidence":"e3"}`,
		`{"action":"continue","summary":"l3","next":"again","evidence":"done3"}`,
	}}
	var schedules []time.Time
	res, err := RunRecurring(context.Background(), s, RecurringOptions{
		Options:   Options{Goal: "recurring", LeftID: "left", RightID: "right"},
		PollEvery: time.Minute,
		Deadline:  start.Add(3 * time.Minute),
		Clock:     clock,
		OnSchedule: func(next time.Time) error {
			schedules = append(schedules, next)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "deadline_reached" || res.Rounds != 3 {
		t.Fatalf("result=%+v", res)
	}
	if got := strings.Join(s.calls, ","); got != "right,left,right,left,right,left" {
		t.Fatalf("calls=%s", got)
	}
	if len(schedules) != 3 {
		t.Fatalf("schedules=%v", schedules)
	}
}

func TestRunRecurringWaitSkipsLeftAndPollsRightAgain(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := &scripted{outputs: []string{
		`{"action":"wait","summary":"executor attempts=0","next":"check observer again","evidence":"arm window expired"}`,
		`{"action":"propose_review","summary":"stop","next":"owner","evidence":"done"}`,
	}}
	res, err := RunRecurring(context.Background(), s, RecurringOptions{
		Options:   Options{Goal: "recurring", LeftID: "left", RightID: "right"},
		PollEvery: time.Minute,
		Deadline:  start.Add(5 * time.Minute),
		Clock:     &fakeClock{now: start},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "propose_review" || res.Rounds != 1 {
		t.Fatalf("result=%+v", res)
	}
	if got := strings.Join(s.calls, ","); got != "right,right" {
		t.Fatalf("wait should skip LEFT, calls=%s", got)
	}
	if len(s.requests) < 2 || !strings.Contains(s.requests[1].Messages[0].Content, "executor attempts=0") || !strings.Contains(s.requests[1].Messages[0].Content, "arm window expired") {
		t.Fatalf("wait observation not carried to next RIGHT: %+v", s.requests)
	}
}

func TestRunRecurringWaitWaitPlanCallsLeftOnlyOnPlan(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := &scripted{outputs: []string{
		`{"action":"wait","summary":"no READY yet","next":"poll preflight","evidence":"observer running"}`,
		`{"action":"wait","summary":"still no READY","next":"poll preflight","evidence":"observer running"}`,
		`{"action":"plan","summary":"READY found","next":"start executor","evidence":"net positive READY_TO_SEND"}`,
		`{"action":"continue","summary":"executor started","next":"observe result","evidence":"live run active"}`,
		`{"action":"propose_review","summary":"stop test","next":"owner","evidence":"scenario complete"}`,
	}}
	res, err := RunRecurring(context.Background(), s, RecurringOptions{
		Options:   Options{Goal: "profit", LeftID: "left", RightID: "right"},
		PollEvery: time.Minute,
		Deadline:  start.Add(10 * time.Minute),
		Clock:     &fakeClock{now: start},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "propose_review" || res.Rounds != 3 {
		t.Fatalf("result=%+v", res)
	}
	if got := strings.Join(s.calls, ","); got != "right,right,right,left,right" {
		t.Fatalf("calls=%s", got)
	}
}

func TestRunRecurringBlockedStopsWithoutFutureTick(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := &scripted{outputs: []string{
		`{"action":"blocked","summary":"owner signature required","next":"owner must sign","evidence":"manual authorization required"}`,
	}}
	res, err := RunRecurring(context.Background(), s, RecurringOptions{
		Options:   Options{Goal: "recurring", LeftID: "left", RightID: "right"},
		PollEvery: time.Minute,
		Deadline:  start.Add(10 * time.Minute),
		Clock:     &fakeClock{now: start},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "blocked" || len(s.calls) != 1 {
		t.Fatalf("result=%+v calls=%v", res, s.calls)
	}
}

func TestRunRecurringContinuousWaitStopsAtDeadline(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := &scripted{outputs: []string{
		`{"action":"wait","summary":"w1","next":"poll","evidence":"e1"}`,
		`{"action":"wait","summary":"w2","next":"poll","evidence":"e2"}`,
		`{"action":"wait","summary":"w3","next":"poll","evidence":"e3"}`,
	}}
	res, err := RunRecurring(context.Background(), s, RecurringOptions{
		Options:   Options{Goal: "recurring", LeftID: "left", RightID: "right"},
		PollEvery: time.Minute,
		Deadline:  start.Add(3 * time.Minute),
		Clock:     &fakeClock{now: start},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(s.calls, ",")
	if res.Status != "deadline_reached" || res.Rounds != 3 || got != "right,right,right" {
		t.Fatalf("result=%+v calls=%v", res, s.calls)
	}
}

func TestRunRecurringProfitObservationE2E(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := &scripted{outputs: []string{
		`{"action":"wait","summary":"executor arm window expired attempts=0","next":"re-check preflight","evidence":"executor stopped without submission"}`,
		`{"action":"wait","summary":"observer still running, no READY_TO_SEND","next":"re-check for net-positive READY","evidence":"preflight has no actionable candidate"}`,
		`{"action":"plan","summary":"net-positive READY_TO_SEND appeared","next":"start one-shot executor","evidence":"gross exceeds gas and min net gate"}`,
		`{"action":"continue","summary":"one-shot executor started","next":"observe tx outcome","evidence":"live executor armed"}`,
		`{"action":"propose_review","summary":"scenario verified","next":"review recurring behavior","evidence":"wait transitioned to plan"}`,
	}}
	res, err := RunRecurring(context.Background(), s, RecurringOptions{
		Options:   Options{Goal: "achieve profitable execution", LeftID: "left", RightID: "right"},
		PollEvery: 30 * time.Second,
		Deadline:  start.Add(5 * time.Minute),
		Clock:     &fakeClock{now: start},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(s.calls, ",")
	if res.Status != "propose_review" || got != "right,right,right,left,right" {
		t.Fatalf("result=%+v calls=%v", res, s.calls)
	}
	planPrompt := s.requests[2].Messages[0].Content
	if !strings.Contains(planPrompt, "observer still running, no READY_TO_SEND") || !strings.Contains(planPrompt, "preflight has no actionable candidate") {
		t.Fatalf("third RIGHT lost prior observation: %s", planPrompt)
	}
}

func TestRunRecurringResumeFromWaitUsesPersistedScheduleAndObservation(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: start.Add(time.Minute)}
	s := &scripted{outputs: []string{
		`{"action":"propose_review","summary":"resume observed state","next":"owner","evidence":"prior wait context present"}`,
	}}
	res, err := RunRecurring(context.Background(), s, RecurringOptions{
		Options: Options{
			Goal:           "profit",
			LeftID:         "left",
			RightID:        "right",
			InitialSummary: "Last recurring action=wait. Observation: executor attempts=0 Evidence: arm window expired",
		},
		PollEvery:  time.Minute,
		Deadline:   start.Add(10 * time.Minute),
		NextPollAt: start.Add(2 * time.Minute),
		Clock:      clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "propose_review" || !clock.Now().Equal(start.Add(2*time.Minute)) {
		t.Fatalf("result=%+v clock=%v", res, clock.Now())
	}
	if len(s.requests) != 1 {
		t.Fatalf("requests=%d", len(s.requests))
	}
	prompt := s.requests[0].Messages[0].Content
	if !strings.Contains(prompt, "executor attempts=0") || !strings.Contains(prompt, "arm window expired") {
		t.Fatalf("resume prompt lost wait observation: %s", prompt)
	}
}

func TestRunRecurringProposeReviewStopsBeforeLeftAndFutureTicks(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := &scripted{outputs: []string{
		`{"action":"propose_review","summary":"ready","next":"owner","evidence":"tests pass"}`,
	}}
	res, err := RunRecurring(context.Background(), s, RecurringOptions{
		Options:   Options{Goal: "recurring", LeftID: "left", RightID: "right"},
		PollEvery: time.Minute,
		Deadline:  start.Add(10 * time.Minute),
		Clock:     &fakeClock{now: start},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "propose_review" || res.Rounds != 0 {
		t.Fatalf("result=%+v", res)
	}
	if got := strings.Join(s.calls, ","); got != "right" {
		t.Fatalf("calls=%s", got)
	}
}

func TestRunRecurringDeadlinePreventsNextTick(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := &scripted{outputs: []string{
		`{"action":"plan","summary":"r1","next":"work","evidence":"e1"}`,
		`{"action":"continue","summary":"l1","next":"again","evidence":"done1"}`,
	}}
	res, err := RunRecurring(context.Background(), s, RecurringOptions{
		Options:   Options{Goal: "recurring", LeftID: "left", RightID: "right"},
		PollEvery: time.Minute,
		Deadline:  start.Add(time.Minute),
		Clock:     &fakeClock{now: start},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "deadline_reached" || res.Rounds != 1 || len(s.calls) != 2 {
		t.Fatalf("result=%+v calls=%v", res, s.calls)
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
	t.Run("wait remains recurring-only", func(t *testing.T) {
		s := &scripted{outputs: []string{
			`{"action":"wait","summary":"temporary condition","next":"poll later","evidence":"none"}`,
		}}
		if _, err := Run(context.Background(), s, Options{Goal: "task", LeftID: "left", RightID: "right"}); err == nil || !strings.Contains(err.Error(), "invalid action") {
			t.Fatalf("one-shot accepted recurring-only wait action: %v", err)
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

func TestCallRejectsInvalidJSONWithoutReplay(t *testing.T) {
	s := &scripted{outputs: []string{"not json"}}
	_, err := call(context.Background(), s, "right", "review")
	if err == nil || len(s.calls) != 1 {
		t.Fatalf("expected one malformed completion with no replay, got err=%v calls=%d", err, len(s.calls))
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

func TestCallRecoversContentReferenceOnceWithoutReplayingTask(t *testing.T) {
	s := &scripted{outputs: []string{
		`::chatgpt-content-reference{index="0" source_message_id="abc"}`,
		`{"action":"plan","summary":"continue work","next":"run tests","evidence":"already checked"}`,
	}}
	turn, err := call(context.Background(), s, "right", "review the goal")
	if err != nil {
		t.Fatal(err)
	}
	if turn.Action != "plan" || len(s.calls) != 2 || len(s.requests) != 2 {
		t.Fatalf("failed bounded recovery: turn=%+v calls=%d requests=%d", turn, len(s.calls), len(s.requests))
	}
	retryPrompt := s.requests[1].Messages[0].Content
	if strings.Contains(retryPrompt, "review the goal") {
		t.Fatalf("recovery replayed original task: %q", retryPrompt)
	}
	if !strings.Contains(retryPrompt, "Do not repeat or redo") {
		t.Fatalf("recovery missing no-replay guard: %q", retryPrompt)
	}
	for i, req := range s.requests {
		if req.Model != duelModel || req.ReasoningEffort != duelReasoningEffort {
			t.Fatalf("request %d selection=%q/%q", i, req.Model, req.ReasoningEffort)
		}
	}
}

func TestCallMalformedDecisionSendsExactlyOnce(t *testing.T) {
	s := &scripted{outputs: []string{"not-json"}}
	_, err := call(context.Background(), s, "right", "review")
	if err == nil || len(s.calls) != 1 {
		t.Fatalf("unexpected result: %v calls=%d", err, len(s.calls))
	}
}

func TestCallContentReferenceRecoveryStopsAfterOneFollowup(t *testing.T) {
	s := &scripted{outputs: []string{
		`::chatgpt-content-reference{index="0" source_message_id="abc"}`,
		`::chatgpt-content-reference{index="0" source_message_id="def"}`,
	}}
	_, err := call(context.Background(), s, "right", "review")
	if err == nil || len(s.calls) != 2 {
		t.Fatalf("expected one bounded recovery, got err=%v calls=%d", err, len(s.calls))
	}
}

func TestContentReferenceOnlyRejectsQuotedMarker(t *testing.T) {
	if !contentReferenceOnly(`::chatgpt-content-reference{index="0" source_message_id="abc"}`) {
		t.Fatal("standalone marker was not recognized")
	}
	for _, raw := range []string{
		`{"action":"plan","evidence":"saw ::chatgpt-content-reference{...}"}`,
		"The bad response was `::chatgpt-content-reference{...}`.",
		"::chatgpt-content-reference{index=\"0\"}\nDone",
	} {
		if contentReferenceOnly(raw) {
			t.Fatalf("normal content misclassified as marker-only: %q", raw)
		}
	}
}
