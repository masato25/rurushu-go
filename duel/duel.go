// Package duel coordinates two COS sessions until both agree to request owner review.
package duel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/masato25/rurushu-go/provider"
)

// Turn is a model's explicit decision; its verdict is not inferred from prose.
type Turn struct {
	Action   string `json:"action"`
	Summary  string `json:"summary"`
	Next     string `json:"next"`
	Evidence string `json:"evidence"`
}
type Streamer interface {
	Stream(context.Context, provider.CompletionRequest) (<-chan provider.StreamEvent, error)
}
type Options struct {
	Goal, LeftID, RightID, InitialSummary string
	OnTurn                                func(role string, round int, turn Turn) error
	Instructions                          <-chan string
}
type Result struct {
	Rounds  int
	Status  string
	Summary string
}

func call(ctx context.Context, s Streamer, id, prompt string) (Turn, error) {
	var lastErr error
	originalPrompt := prompt
	for attempt := 0; attempt < 3; attempt++ {
		turn, err := callOnce(ctx, s, id, prompt)
		if err == nil {
			return turn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return Turn{}, ctx.Err()
		}
		var malformed *decisionFormatError
		if !errors.As(err, &malformed) {
			return Turn{}, err
		}
		prompt = originalPrompt + "\n\nYour previous reply was not a machine-readable decision (it may have been only a content-reference marker). Reply with ONE complete JSON object containing action, summary, next, evidence. No tools, markdown, citations or explanation outside JSON."
	}
	return Turn{}, lastErr
}

type decisionFormatError struct{ text string }

func (e *decisionFormatError) Error() string { return e.text }

func callOnce(ctx context.Context, s Streamer, id, prompt string) (Turn, error) {
	events, err := s.Stream(ctx, provider.CompletionRequest{Model: "chatgpt-web", SessionID: id, Messages: []provider.Message{{Role: provider.RoleUser, Content: prompt}}})
	if err != nil {
		return Turn{}, err
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
				return Turn{}, ev.Error
			}
			return Turn{}, errors.New("COS stream failed")
		}
		if err := ctx.Err(); err != nil {
			return Turn{}, err
		}
	}
	if !done {
		return Turn{}, errors.New("COS turn closed without completion")
	}
	return parseTurn(output.String())
}

// COS can include tool activity and intermediate messages before the final
// assistant response. Scan for complete JSON objects and use the last valid
// decision, rather than parsing the full mixed transcript as one JSON value.
func parseTurn(raw string) (Turn, error) {
	var last Turn
	found := false
	for offset := 0; offset < len(raw); offset++ {
		if raw[offset] != '{' {
			continue
		}
		var turn Turn
		decoder := json.NewDecoder(strings.NewReader(raw[offset:]))
		if err := decoder.Decode(&turn); err != nil {
			continue
		}
		if strings.TrimSpace(turn.Action) == "" || strings.TrimSpace(turn.Summary) == "" {
			continue
		}
		last, found = turn, true
	}
	if !found {
		return Turn{}, &decisionFormatError{text: fmt.Sprintf("COS response has no complete decision JSON: %.200s", strings.TrimSpace(raw))}
	}
	return last, nil
}

// Run has no maximum rounds. It stops normally only on bilateral agreement.
// Cancellation or provider errors are exceptional exits, never success.
func Run(ctx context.Context, s Streamer, o Options) (Result, error) {
	if strings.TrimSpace(o.Goal) == "" || o.LeftID == "" || o.RightID == "" || o.LeftID == o.RightID {
		return Result{}, errors.New("a goal and two distinct session IDs are required")
	}
	result := Result{Status: "running"}
	previous := strings.TrimSpace(o.InitialSummary)
	if previous == "" {
		previous = "No work yet."
	}
	for round := 1; ; round++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ownerNotes := drainInstructions(o.Instructions)
		rightPrompt := fmt.Sprintf(`You are RIGHT (decision/review). Goal: %s
Owner instructions (highest task priority): %s
Current evidence from LEFT: %s
Decide the next concrete, testable work step. If stuck, research on the web yourself using available browsing tools. Report verified source URLs and findings in evidence; if browsing is unavailable use action=research and next must request external research from LEFT. If no actionable local work exists and progress requires an external event or owner authorization, action=blocked; do not repeat previous work. If you believe the goal is satisfied, action=propose_review with reasons in evidence. Never claim an online search unless you actually performed it.
Reply ONLY JSON: {"action":"plan|research|blocked|propose_review","summary":"current concise summary","next":"specific instructions or precise unblock condition","evidence":"test evidence or caveats"}`, o.Goal, ownerNotes, previous)
		right, err := call(ctx, s, o.RightID, rightPrompt)
		if err != nil {
			return result, fmt.Errorf("right round %d: %w", round, err)
		}
		if right.Action != "plan" && right.Action != "research" && right.Action != "blocked" && right.Action != "propose_review" {
			return result, fmt.Errorf("right round %d: invalid action %q", round, right.Action)
		}
		if o.OnTurn != nil {
			if err := o.OnTurn("right", round, right); err != nil {
				return result, err
			}
		}
		ownerNotes = strings.TrimSpace(ownerNotes + "\n" + drainInstructions(o.Instructions))
		leftPrompt := fmt.Sprintf(`You are LEFT (implementation/execution). Goal: %s
Owner instructions (highest task priority): %s
RIGHT's assessment: %s
RIGHT's action: %s
RIGHT's requested next step: %s
RIGHT's evidence: %s
Execute or investigate using available tools, verify results, and report facts. If action is research, collect current online sources if browser/search tools are available; never invent citations. If RIGHT says blocked, independently check whether any actionable work remains; reply action=blocked if both sides must wait for an external condition, without repeating tests or making unnecessary requests. If action is propose_review, independently verify completion, and action=agree_review only if genuinely ready; otherwise action=continue.
Reply ONLY JSON: {"action":"continue|blocked|agree_review","summary":"work completed and remaining issues","next":"what RIGHT should consider or unblock condition","evidence":"tests or URLs actually checked"}`, o.Goal, ownerNotes, right.Summary, right.Action, right.Next, right.Evidence)
		left, err := call(ctx, s, o.LeftID, leftPrompt)
		if err != nil {
			return result, fmt.Errorf("left round %d: %w", round, err)
		}
		if left.Action != "continue" && left.Action != "blocked" && left.Action != "agree_review" {
			return result, fmt.Errorf("left round %d: invalid action %q", round, left.Action)
		}
		if o.OnTurn != nil {
			if err := o.OnTurn("left", round, left); err != nil {
				return result, err
			}
		}
		result.Rounds = round
		result.Summary = left.Summary
		if right.Action == "propose_review" && left.Action == "agree_review" {
			result.Status = "owner_review"
			return result, nil
		}
		if right.Action == "blocked" && left.Action == "blocked" {
			result.Status = "blocked"
			result.Summary = fmt.Sprintf("RIGHT: %s; LEFT: %s; unblock: %s", right.Summary, left.Summary, right.Next)
			return result, nil
		}
		previous = fmt.Sprintf("Round %d: RIGHT: %s. LEFT: %s. Evidence: %s. LEFT next: %s", round, right.Summary, left.Summary, left.Evidence, left.Next)
	}
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
