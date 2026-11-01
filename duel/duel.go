// Package duel coordinates a primary LEFT worker with a lightweight RIGHT reviewer.
package duel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/masato25/rurushu-go/provider"
)

// Turn is the display/persistence envelope for one LEFT or RIGHT response.
type Turn struct {
	Action   string `json:"action"`
	Summary  string `json:"summary"`
	Next     string `json:"next"`
	Evidence string `json:"evidence"`
	Content  string `json:"content,omitempty"`
}
type Streamer interface {
	Stream(context.Context, provider.CompletionRequest) (<-chan provider.StreamEvent, error)
}
type Options struct {
	Goal, LeftID, RightID, InitialSummary string
	LeftModel, RightModel                 string
	LeftStreamer, RightStreamer           Streamer
	GoalFunc                              func() string
	OnRoleStart                           func(role string, round int) error
	OnTurn                                func(role string, round int, turn Turn) error
	Instructions                          <-chan string
	RetryDelay                            time.Duration
	RetryClock                            Clock
	OnRetry                               func(role string, round int, delay time.Duration, err error) error
}

func roleStreamer(fallback Streamer, configured Streamer) Streamer {
	if configured != nil {
		return configured
	}
	return fallback
}

func roleModel(configured string) string {
	if model := strings.TrimSpace(configured); model != "" {
		return model
	}
	return duelModel
}

func currentGoal(o Options) string {
	if o.GoalFunc != nil {
		if goal := strings.TrimSpace(o.GoalFunc()); goal != "" {
			return goal
		}
	}
	return strings.TrimSpace(o.Goal)
}

type Result struct {
	Rounds  int
	Status  string
	Summary string
}

// Clock is the minimal scheduler surface used by recurring duels. Tests can
// supply a deterministic fake without sleeping in wall-clock time.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

const (
	duelModel           = "gpt-5-6"
	duelReasoningEffort = "medium"
)

// RecurringOptions runs one LEFT work turn plus one RIGHT feedback turn per tick.
type RecurringOptions struct {
	Options
	PollEvery  time.Duration
	Deadline   time.Time
	NextPollAt time.Time
	Clock      Clock
	OnSchedule func(next time.Time) error
}

func callTextOnce(ctx context.Context, s Streamer, id, model, prompt string, maxTokens *int) (string, error) {
	events, err := s.Stream(ctx, provider.CompletionRequest{
		Model: model, ReasoningEffort: duelReasoningEffort, SessionID: id, MaxTokens: maxTokens,
		Messages: []provider.Message{{Role: provider.RoleUser, Content: prompt}},
	})
	if err != nil {
		return "", err
	}
	var output strings.Builder
	done := false
	for ev := range events {
		switch ev.Type {
		case provider.EventReplace:
			output.Reset()
			output.WriteString(ev.Text)
		case provider.EventToken:
			output.WriteString(ev.Text)
		case provider.EventDone:
			done = true
		case provider.EventError:
			if ev.Error != nil {
				return "", ev.Error
			}
			return "", errors.New("provider stream failed")
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	if !done {
		return "", errors.New("provider turn closed without completion")
	}
	return strings.TrimSpace(output.String()), nil
}

func callTextWithRetry(ctx context.Context, s Streamer, id, model, prompt, role string, round int, maxTokens *int, o Options) (string, error) {
	text, err := callTextOnce(ctx, s, id, model, prompt, maxTokens)
	if err == nil || ctx.Err() != nil || o.RetryDelay <= 0 {
		return text, err
	}
	if o.OnRetry != nil {
		if notifyErr := o.OnRetry(role, round, o.RetryDelay, err); notifyErr != nil {
			return "", notifyErr
		}
	}
	clock := o.RetryClock
	if clock == nil {
		clock = realClock{}
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-clock.After(o.RetryDelay):
	}
	text, retryErr := callTextOnce(ctx, s, id, model, prompt, maxTokens)
	if retryErr != nil {
		return "", fmt.Errorf("retry after %s failed: %w", o.RetryDelay, retryErr)
	}
	return text, nil
}

func truncateRunes(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 {
		return ""
	}
	r := []rune(text)
	if len(r) <= limit {
		return text
	}
	return strings.TrimSpace(string(r[:limit]))
}

func rightDone(text string) bool {
	upper := strings.ToUpper(strings.TrimSpace(text))
	return strings.HasPrefix(upper, "DONE:") || strings.HasPrefix(upper, "[DONE]") || upper == "DONE"
}

func compactLeftFollowup(ownerNotes, feedback string) string {
	ownerNotes = strings.TrimSpace(ownerNotes)
	feedback = strings.TrimSpace(feedback)
	if ownerNotes == "" {
		return feedback
	}
	if feedback == "" {
		return "Owner instruction: " + ownerNotes
	}
	return "Owner instruction: " + ownerNotes + "\n\n" + feedback
}

// Run makes LEFT the primary working agent. LEFT behaves like a normal single
// chat and produces an unconstrained final response. RIGHT only sees that final
// response and returns a short coaching/review reply, which is fed back to LEFT
// on the next round. RIGHT may prefix its reply with DONE: when owner review is
// appropriate; otherwise the loop continues until cancellation/error.
func Run(ctx context.Context, fallback Streamer, o Options) (Result, error) {
	if currentGoal(o) == "" || o.LeftID == "" || o.RightID == "" || o.LeftID == o.RightID {
		return Result{}, errors.New("a goal and two distinct session IDs are required")
	}
	leftStreamer := roleStreamer(fallback, o.LeftStreamer)
	rightStreamer := roleStreamer(fallback, o.RightStreamer)
	if leftStreamer == nil || rightStreamer == nil {
		return Result{}, errors.New("left and right streamers are required")
	}
	result := Result{Status: "running"}
	feedback := strings.TrimSpace(o.InitialSummary)
	continuingSession := feedback != ""
	for round := 1; ; round++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if o.OnRoleStart != nil {
			if err := o.OnRoleStart("left", round); err != nil {
				return result, err
			}
		}
		ownerNotes := drainInstructions(o.Instructions)
		goal := currentGoal(o)
		var leftPrompt string
		if round == 1 && !continuingSession {
			leftPrompt = fmt.Sprintf(`You are LEFT, the primary implementation/execution agent in a duel. Work exactly like a normal single-chat coding/research session: investigate, use tools, make edits, run tests, and drive the task forward autonomously.
Goal: %s
Owner instructions (highest priority): %s
RIGHT feedback from the previous round: %s

Do the useful work now. Do not wait for RIGHT to make a plan for you. Your final response should be your normal concise work report for this round; do not wrap it in JSON or a special protocol.`, goal, ownerNotes, firstNonEmptyText(feedback, "No RIGHT feedback yet; start from the goal."))
		} else {
			leftPrompt = compactLeftFollowup(ownerNotes, feedback)
		}
		leftText, err := callTextWithRetry(ctx, leftStreamer, o.LeftID, roleModel(o.LeftModel), leftPrompt, "left", round, nil, o)
		if err != nil {
			return result, fmt.Errorf("left round %d: %w", round, err)
		}
		leftTurn := Turn{Action: "work", Summary: leftText, Content: leftText}
		if o.OnTurn != nil {
			if err := o.OnTurn("left", round, leftTurn); err != nil {
				return result, err
			}
		}

		goal = currentGoal(o)
		if o.OnRoleStart != nil {
			if err := o.OnRoleStart("right", round); err != nil {
				return result, err
			}
		}
		rightPrompt := fmt.Sprintf(`You are RIGHT, a lightweight reviewer/coach. LEFT is the primary agent and has just completed a work round.
Goal: %s
LEFT's final response:
%s

Reply directly to LEFT with only the most useful reaction, correction, question, or suggestion for what it should do next. Do not take over planning or execution. Keep the entire reply within 100 Unicode characters. No JSON, no markdown wrapper. If the task is genuinely ready for owner review, begin with "DONE:" and briefly say why.`, goal, leftText)
		rightMaxTokens := 96
		rightText, err := callTextWithRetry(ctx, rightStreamer, o.RightID, roleModel(o.RightModel), rightPrompt, "right", round, &rightMaxTokens, o)
		if err != nil {
			return result, fmt.Errorf("right round %d: %w", round, err)
		}
		rightText = truncateRunes(rightText, 100)
		rightTurn := Turn{Action: "feedback", Summary: rightText, Content: rightText}
		if o.OnTurn != nil {
			if err := o.OnTurn("right", round, rightTurn); err != nil {
				return result, err
			}
		}
		result.Rounds = round
		result.Summary = leftText
		if rightDone(rightText) {
			result.Status = "owner_review"
			return result, nil
		}
		feedback = rightText
	}
}

// RunRecurring executes one LEFT/RIGHT cycle per poll tick until the persisted
// deadline is reached or RIGHT marks the work ready for owner review.
func RunRecurring(ctx context.Context, fallback Streamer, o RecurringOptions) (Result, error) {
	if currentGoal(o.Options) == "" || o.LeftID == "" || o.RightID == "" || o.LeftID == o.RightID {
		return Result{}, errors.New("a goal and two distinct session IDs are required")
	}
	if o.PollEvery <= 0 {
		return Result{}, errors.New("poll interval must be positive")
	}
	clock := o.Clock
	if clock == nil {
		clock = realClock{}
	}
	if o.Deadline.IsZero() {
		return Result{}, errors.New("recurring duel requires a deadline")
	}

	leftStreamer := roleStreamer(fallback, o.LeftStreamer)
	rightStreamer := roleStreamer(fallback, o.RightStreamer)
	if leftStreamer == nil || rightStreamer == nil {
		return Result{}, errors.New("left and right streamers are required")
	}
	result := Result{Status: "running"}
	feedback := strings.TrimSpace(o.InitialSummary)
	continuingSession := feedback != ""
	next := o.NextPollAt
	if next.IsZero() {
		next = clock.Now()
	}

	for round := 1; ; round++ {
		now := clock.Now()
		if !next.Before(o.Deadline) || !now.Before(o.Deadline) {
			result.Status = "deadline_reached"
			return result, nil
		}
		if wait := next.Sub(now); wait > 0 {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-clock.After(wait):
			}
		}
		if !clock.Now().Before(o.Deadline) {
			result.Status = "deadline_reached"
			return result, nil
		}

		if o.OnRoleStart != nil {
			if err := o.OnRoleStart("left", round); err != nil {
				return result, err
			}
		}

		ownerNotes := drainInstructions(o.Instructions)
		goal := currentGoal(o.Options)
		var leftPrompt string
		if round == 1 && !continuingSession {
			leftPrompt = fmt.Sprintf(`You are LEFT, the primary implementation/execution agent in a recurring duel. Work like a normal single-chat session and make concrete progress now.
Goal: %s
Owner instructions (highest priority): %s
RIGHT feedback from the previous round: %s

Use tools and perform useful work. Return a normal concise final work report; no JSON protocol.`, goal, ownerNotes, firstNonEmptyText(feedback, "No RIGHT feedback yet; inspect the current state and act if useful."))
		} else {
			leftPrompt = compactLeftFollowup(ownerNotes, feedback)
		}
		leftText, err := callTextWithRetry(ctx, leftStreamer, o.LeftID, roleModel(o.LeftModel), leftPrompt, "left", round, nil, o.Options)
		if err != nil {
			return result, fmt.Errorf("left round %d: %w", round, err)
		}
		leftTurn := Turn{Action: "work", Summary: leftText, Content: leftText}
		if o.OnTurn != nil {
			if err := o.OnTurn("left", round, leftTurn); err != nil {
				return result, err
			}
		}

		goal = currentGoal(o.Options)
		if o.OnRoleStart != nil {
			if err := o.OnRoleStart("right", round); err != nil {
				return result, err
			}
		}
		rightPrompt := fmt.Sprintf(`You are RIGHT, a lightweight reviewer/coach. LEFT is the primary agent.
Goal: %s
LEFT's final response:
%s

Reply directly to LEFT with the single most useful reaction/correction/suggestion. Do not take over execution. Maximum 100 Unicode characters. No JSON. If ready for owner review, begin with "DONE:".`, goal, leftText)
		rightMaxTokens := 96
		rightText, err := callTextWithRetry(ctx, rightStreamer, o.RightID, roleModel(o.RightModel), rightPrompt, "right", round, &rightMaxTokens, o.Options)
		if err != nil {
			return result, fmt.Errorf("right round %d: %w", round, err)
		}
		rightText = truncateRunes(rightText, 100)
		rightTurn := Turn{Action: "feedback", Summary: rightText, Content: rightText}
		if o.OnTurn != nil {
			if err := o.OnTurn("right", round, rightTurn); err != nil {
				return result, err
			}
		}
		result.Rounds = round
		result.Summary = leftText
		if rightDone(rightText) {
			result.Status = "owner_review"
			return result, nil
		}
		feedback = rightText

		next = clock.Now().Add(o.PollEvery)
		if o.OnSchedule != nil {
			if err := o.OnSchedule(next); err != nil {
				return result, err
			}
		}
	}
}

func firstNonEmptyText(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func drainInstructions(ch <-chan string) string {
	var notes []string
	for ch != nil {
		select {
		case note, ok := <-ch:
			if !ok {
				return strings.Join(notes, "\n")
			}
			if text := strings.TrimSpace(note); text != "" {
				notes = append(notes, text)
			}
		default:
			return strings.Join(notes, "\n")
		}
	}
	return strings.Join(notes, "\n")
}
