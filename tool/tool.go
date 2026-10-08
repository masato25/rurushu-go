package tool

import (
	"context"
	"encoding/json"

	"github.com/masato25/rurushu-go/permission"
)

type Result struct {
	Title    string         `json:"title"`
	Output   string         `json:"output"`
	IsError  bool           `json:"is_error,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type ExecutionContext struct {
	CWD           string
	AskPermission func(permission.Request) bool
	Metadata      map[string]string
}

type Tool interface {
	ID() string
	Description() string
	Parameters() map[string]any
	Execute(ctx context.Context, args json.RawMessage, execCtx *ExecutionContext) (*Result, error)
}

type Plugin interface {
	Name() string
	Tools() []Tool
}
