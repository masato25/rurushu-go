package permission

import (
	"context"
	"sync"
)

// Prompt is a pending permission request emitted by TUIHandler.
// Resolve is idempotent; the first decision wins.
type Prompt struct {
	Request Request
	result  chan bool
	once    sync.Once
}

func (p *Prompt) Resolve(approved bool) {
	if p == nil {
		return
	}
	p.once.Do(func() { p.result <- approved })
}

// TUIHandler bridges tool permission requests to an event-driven terminal UI.
// It never reads stdin directly, so Bubble Tea can remain the sole terminal owner.
type TUIHandler struct {
	requests chan *Prompt
}

func NewTUIHandler() *TUIHandler {
	return &TUIHandler{requests: make(chan *Prompt)}
}

func (h *TUIHandler) Requests() <-chan *Prompt { return h.requests }

func (h *TUIHandler) Ask(ctx context.Context, req Request) (bool, error) {
	prompt := &Prompt{Request: req, result: make(chan bool, 1)}
	select {
	case h.requests <- prompt:
	case <-ctx.Done():
		return false, ctx.Err()
	}

	select {
	case approved := <-prompt.result:
		return approved, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
