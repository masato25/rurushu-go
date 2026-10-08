package provider

import (
	"fmt"
	"sort"
	"strings"
)

// ToolCallAccumulator reconstructs chunked function calls from streaming APIs.
type ToolCallAccumulator struct {
	calls map[int]*accumulatedCall
}

type accumulatedCall struct {
	id        string
	name      string
	arguments strings.Builder
}

func NewToolCallAccumulator() *ToolCallAccumulator {
	return &ToolCallAccumulator{calls: make(map[int]*accumulatedCall)}
}

func (a *ToolCallAccumulator) AddChunk(index int, id, name, argsChunk string) {
	call, ok := a.calls[index]
	if !ok {
		call = &accumulatedCall{id: id, name: name}
		a.calls[index] = call
	}
	if id != "" && call.id == "" {
		call.id = id
	}
	if name != "" && call.name == "" {
		call.name = name
	}
	if argsChunk != "" {
		call.arguments.WriteString(argsChunk)
	}
}

func (a *ToolCallAccumulator) SetFinal(index int, id, name, args string) {
	call, ok := a.calls[index]
	if !ok {
		call = &accumulatedCall{}
		a.calls[index] = call
	}
	if id != "" {
		call.id = id
	}
	if name != "" {
		call.name = name
	}
	if strings.TrimSpace(args) != "" {
		call.arguments.Reset()
		call.arguments.WriteString(args)
	}
}

func (a *ToolCallAccumulator) Collect() []ToolCall {
	if len(a.calls) == 0 {
		return nil
	}
	indices := make([]int, 0, len(a.calls))
	for idx := range a.calls {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	result := make([]ToolCall, 0, len(indices))
	for _, idx := range indices {
		c := a.calls[idx]
		id := c.id
		if id == "" {
			id = fmt.Sprintf("call_%d", idx)
		}
		args := strings.TrimSpace(c.arguments.String())
		if args == "" {
			args = "{}"
		}
		result = append(result, ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: c.name, Arguments: args}})
	}
	return result
}
