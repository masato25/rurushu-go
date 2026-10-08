package permission

import (
	"context"
	"errors"
	"testing"
)

func TestTUIHandlerResolvesPrompt(t *testing.T) {
	h := NewTUIHandler()
	result := make(chan bool, 1)
	go func() {
		approved, err := h.Ask(context.Background(), Request{Tool: "write", Pattern: "main.go"})
		if err != nil {
			result <- false
			return
		}
		result <- approved
	}()
	prompt := <-h.Requests()
	prompt.Resolve(true)
	prompt.Resolve(false)
	if !<-result {
		t.Fatal("expected approval")
	}
}

func TestTUIHandlerContextCancellation(t *testing.T) {
	h := NewTUIHandler()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h.Ask(ctx, Request{Tool: "write"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}
