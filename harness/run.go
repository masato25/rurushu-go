package harness

import (
	"context"
	"fmt"
	"strings"

	"github.com/masato25/rurushu-go/provider"
)

type Result struct {
	Text        string
	Reasoning   string
	Usage       provider.TokenUsage
	ToolCalls   int
	Compactions int
}

type Validator func(Result) error

// Run consumes a streaming interaction and returns its final aggregate result.
func (c *Client) Run(ctx context.Context, req provider.CompletionRequest, validators ...Validator) (Result, error) {
	stream, err := c.Stream(ctx, req)
	if err != nil {
		return Result{}, err
	}
	var result Result
	var text, reasoning strings.Builder
	for event := range stream {
		switch event.Type {
		case provider.EventToken:
			text.WriteString(event.Text)
		case provider.EventReasoning:
			reasoning.WriteString(event.ReasoningText)
		case provider.EventToolStart:
			result.ToolCalls++
		case provider.EventContextCompact:
			result.Compactions++
		case provider.EventUsage, provider.EventDone:
			if event.Usage != nil {
				result.Usage = *event.Usage
			}
		case provider.EventError:
			if event.Error == nil {
				return Result{}, fmt.Errorf("harness stream failed")
			}
			return Result{}, event.Error
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	result.Text = strings.TrimSpace(text.String())
	result.Reasoning = strings.TrimSpace(reasoning.String())
	for _, validate := range validators {
		if validate != nil {
			if err := validate(result); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}
