package permission

import "context"

type Request struct {
	Tool        string `json:"tool"`
	Pattern     string `json:"pattern,omitempty"`
	Description string `json:"description,omitempty"`
}

type Handler interface {
	Ask(ctx context.Context, req Request) (bool, error)
}

type HandlerFunc func(context.Context, Request) (bool, error)

func (f HandlerFunc) Ask(ctx context.Context, req Request) (bool, error) { return f(ctx, req) }

type AllowAll struct{}

func (AllowAll) Ask(context.Context, Request) (bool, error) { return true, nil }

type DenyAll struct{}

func (DenyAll) Ask(context.Context, Request) (bool, error) { return false, nil }
