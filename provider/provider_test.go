package provider

import "testing"

func TestSanitizeMessages(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Content: "hello"},
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "1", Function: FunctionCall{Name: "lookup"}}}},
		{Role: RoleTool, ToolCallID: "1", Content: "ok"},
		{Role: RoleTool, ToolCallID: "missing", Content: "bad"},
	}
	got := SanitizeMessages(messages)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[1].ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("arguments = %q", got[1].ToolCalls[0].Function.Arguments)
	}
}

func TestToolCallAccumulatorFinalIsAuthoritative(t *testing.T) {
	a := NewToolCallAccumulator()
	a.AddChunk(0, "call-1", "lookup", `{"q":"par`)
	a.AddChunk(0, "", "", `tial"}`)
	a.SetFinal(0, "call-1", "lookup", `{"q":"final"}`)
	calls := a.Collect()
	if len(calls) != 1 || calls[0].Function.Arguments != `{"q":"final"}` {
		t.Fatalf("calls = %#v", calls)
	}
}
