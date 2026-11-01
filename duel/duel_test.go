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

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	close(ch)
	return ch
}

type failOnceStreamer struct {
	inner *scripted
	err   error
	used  bool
}

func (s *failOnceStreamer) Stream(ctx context.Context, req provider.CompletionRequest) (<-chan provider.StreamEvent, error) {
	if !s.used {
		s.used = true
		return nil, s.err
	}
	return s.inner.Stream(ctx, req)
}

func TestRunIsLeftFirstAndFeedsRightReplyBackToLeft(t *testing.T) {
	s := &scripted{outputs: []string{
		"LEFT completed implementation and tests.",
		"再檢查 migration 相容性。",
		"LEFT checked migration compatibility; all good.",
		"DONE: 已完成且測試通過。",
	}}
	var turns []string
	res, err := Run(context.Background(), s, Options{
		Goal: "build feature", LeftID: "left", RightID: "right",
		OnTurn: func(role string, round int, turn Turn) error {
			turns = append(turns, fmt.Sprintf("%s:%d:%s", role, round, turn.Summary))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "owner_review" || res.Rounds != 2 {
		t.Fatalf("result=%+v", res)
	}
	if got := strings.Join(s.calls, ","); got != "left,right,left,right" {
		t.Fatalf("call order=%s", got)
	}
	if !strings.Contains(s.requests[2].Messages[0].Content, "再檢查 migration 相容性。") {
		t.Fatalf("RIGHT feedback not fed to LEFT: %s", s.requests[2].Messages[0].Content)
	}
	if got := s.requests[2].Messages[0].Content; got != "再檢查 migration 相容性。" {
		t.Fatalf("follow-up LEFT prompt should contain only RIGHT feedback, got %q", got)
	}
	if strings.Contains(s.requests[2].Messages[0].Content, "You are LEFT") || strings.Contains(s.requests[2].Messages[0].Content, "Goal:") {
		t.Fatalf("follow-up LEFT prompt repeated bootstrap context: %q", s.requests[2].Messages[0].Content)
	}
	if !strings.Contains(s.requests[1].Messages[0].Content, "LEFT completed implementation and tests.") {
		t.Fatalf("LEFT final response not sent to RIGHT: %s", s.requests[1].Messages[0].Content)
	}
	if len(turns) != 4 || !strings.HasPrefix(turns[0], "left:1:") || !strings.HasPrefix(turns[1], "right:1:") {
		t.Fatalf("turns=%v", turns)
	}
}

func TestRunCompactLeftFollowupIncludesOnlyNewOwnerInstructionAndFeedback(t *testing.T) {
	s := &scripted{outputs: []string{"first work", "keep going", "second work", "DONE: ready"}}
	instructions := make(chan string, 1)
	var injected bool
	_, err := Run(context.Background(), s, Options{
		Goal: "task", LeftID: "left", RightID: "right", Instructions: instructions,
		OnTurn: func(role string, round int, turn Turn) error {
			if role == "right" && round == 1 && !injected {
				instructions <- "NEW_OWNER_NOTE"
				injected = true
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Owner instruction: NEW_OWNER_NOTE\n\nkeep going"
	if got := s.requests[2].Messages[0].Content; got != want {
		t.Fatalf("compact follow-up=%q want=%q", got, want)
	}
}

func TestRunResumeStartsWithCompactLeftFollowup(t *testing.T) {
	s := &scripted{outputs: []string{"resumed work", "DONE: ready"}}
	_, err := Run(context.Background(), s, Options{
		Goal: "task", LeftID: "left", RightID: "right", InitialSummary: "previous RIGHT feedback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.requests[0].Messages[0].Content; got != "previous RIGHT feedback" {
		t.Fatalf("resume LEFT prompt should be compact, got %q", got)
	}
}

func TestRunSupportsIndependentRoleStreamersAndModels(t *testing.T) {
	left := &scripted{outputs: []string{"implemented"}}
	right := &scripted{outputs: []string{"DONE: ok"}}
	res, err := Run(context.Background(), nil, Options{
		Goal: "task", LeftID: "left-session", RightID: "right-session",
		LeftModel: "left-model", RightModel: "right-model",
		LeftStreamer: left, RightStreamer: right,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "owner_review" {
		t.Fatalf("result=%+v", res)
	}
	if len(left.requests) != 1 || left.requests[0].Model != "left-model" || left.requests[0].SessionID != "left-session" {
		t.Fatalf("left request=%+v", left.requests)
	}
	if len(right.requests) != 1 || right.requests[0].Model != "right-model" || right.requests[0].SessionID != "right-session" {
		t.Fatalf("right request=%+v", right.requests)
	}
	if right.requests[0].MaxTokens == nil {
		t.Fatal("RIGHT request should have a small completion budget")
	}
}

func TestRunRightReplyIsHardLimitedTo100UnicodeCharacters(t *testing.T) {
	long := strings.Repeat("界", 130)
	s := &scripted{outputs: []string{"work one", long, "work two", "DONE: finished"}}
	_, err := Run(context.Background(), s, Options{Goal: "task", LeftID: "left", RightID: "right"})
	if err != nil {
		t.Fatal(err)
	}
	prompt := s.requests[2].Messages[0].Content
	if !strings.Contains(prompt, strings.Repeat("界", 100)) {
		t.Fatalf("truncated feedback missing: %q", prompt)
	}
	if strings.Contains(prompt, strings.Repeat("界", 101)) {
		t.Fatal("feedback exceeded 100 runes")
	}
}

func TestRunInjectsOwnerInstructionsIntoNextAvailableRole(t *testing.T) {
	s := &scripted{outputs: []string{"first work", "check docs", "second work", "DONE: ready"}}
	instructions := make(chan string, 2)
	instructions <- "OWNER_NOTE_42"
	_, err := Run(context.Background(), s, Options{Goal: "task", LeftID: "left", RightID: "right", Instructions: instructions})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.requests[0].Messages[0].Content, "OWNER_NOTE_42") {
		t.Fatalf("owner instruction missing from LEFT prompt: %s", s.requests[0].Messages[0].Content)
	}
}

func TestRunRetriesLeftOnceAfterConfiguredDelay(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	left := &failOnceStreamer{err: errors.New("temporary failure"), inner: &scripted{outputs: []string{"recovered"}}}
	right := &scripted{outputs: []string{"DONE: ok"}}
	var retries []string
	res, err := Run(context.Background(), nil, Options{
		Goal: "recover", LeftID: "left", RightID: "right", LeftStreamer: left, RightStreamer: right,
		RetryDelay: time.Minute, RetryClock: clock,
		OnRetry: func(role string, round int, delay time.Duration, err error) error {
			retries = append(retries, fmt.Sprintf("%s:%d:%s:%v", role, round, delay, err))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "owner_review" || len(retries) != 1 || !strings.HasPrefix(retries[0], "left:1:1m0s") {
		t.Fatalf("result=%+v retries=%v", res, retries)
	}
}

func TestRunRecurringRunsLeftThenRightEachTickUntilDeadline(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: start}
	s := &scripted{outputs: []string{"l1", "r1", "l2", "r2", "l3", "r3"}}
	res, err := RunRecurring(context.Background(), s, RecurringOptions{
		Options:   Options{Goal: "observe", LeftID: "left", RightID: "right"},
		PollEvery: time.Minute, Deadline: start.Add(3 * time.Minute), Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "deadline_reached" || res.Rounds != 3 {
		t.Fatalf("result=%+v", res)
	}
	if got := strings.Join(s.calls, ","); got != "left,right,left,right,left,right" {
		t.Fatalf("calls=%s", got)
	}
	if got := s.requests[2].Messages[0].Content; got != "r1" {
		t.Fatalf("recurring follow-up LEFT prompt should be compact, got %q", got)
	}
}

func TestRunRecurringStopsWhenRightSaysDone(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s := &scripted{outputs: []string{"completed checks", "DONE: verified"}}
	res, err := RunRecurring(context.Background(), s, RecurringOptions{
		Options:   Options{Goal: "observe", LeftID: "left", RightID: "right"},
		PollEvery: time.Minute, Deadline: start.Add(time.Hour), Clock: &fakeClock{now: start},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "owner_review" || res.Rounds != 1 {
		t.Fatalf("result=%+v", res)
	}
}

func TestRunCancellationIsNotApproval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, &scripted{}, Options{Goal: "task", LeftID: "left", RightID: "right"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestCallTextPropagatesStreamFailures(t *testing.T) {
	want := errors.New("provider failed")
	_, err := callTextOnce(context.Background(), eventScript{err: want}, "left", "model", "work", nil)
	if !errors.Is(err, want) {
		t.Fatalf("expected provider error, got %v", err)
	}
	_, err = callTextOnce(context.Background(), eventScript{events: []provider.StreamEvent{{Type: provider.EventReplace, Text: "partial"}}}, "left", "model", "work", nil)
	if err == nil || !strings.Contains(err.Error(), "without completion") {
		t.Fatalf("expected incomplete stream error, got %v", err)
	}
}
