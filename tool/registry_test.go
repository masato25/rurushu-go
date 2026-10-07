package tool

import (
	"context"
	"encoding/json"
	"testing"
)

type stubTool string

func (s stubTool) ID() string                 { return string(s) }
func (s stubTool) Description() string        { return "stub " + string(s) }
func (s stubTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (s stubTool) Execute(context.Context, json.RawMessage, *ExecutionContext) (*Result, error) {
	return &Result{Output: "ok"}, nil
}

func TestRegistrySchemasAreStable(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("z"))
	r.Register(stubTool("a"))
	all := r.All()
	if len(all) != 2 || all[0].ID() != "a" || all[1].ID() != "z" {
		t.Fatalf("unexpected order: %#v", all)
	}
	if len(r.Schemas()) != 2 {
		t.Fatalf("schema count = %d", len(r.Schemas()))
	}
}
