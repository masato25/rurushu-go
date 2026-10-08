package harness

import (
	"strings"
	"testing"

	"github.com/masato25/rurushu-go/provider"
)

func TestCompactContextRetainsObjectiveAndRecentToolTurns(t *testing.T) {
	c := &Client{maxContextTokens: 220, autoCompactPercent: 50}
	big := strings.Repeat("x", 500)
	messages := []provider.Message{{Role: provider.RoleSystem, Content: "system"}, {Role: provider.RoleUser, Content: "objective"}}
	for i := 0; i < 4; i++ {
		messages = append(messages,
			provider.Message{Role: provider.RoleAssistant, Content: big, ToolCalls: []provider.ToolCall{{ID: string(rune('a' + i)), Function: provider.FunctionCall{Name: "read", Arguments: "{}"}}}},
			provider.Message{Role: provider.RoleTool, Name: "read", Content: big},
		)
	}
	got, info := c.compactContext(messages, nil)
	if info == nil {
		t.Fatal("expected compaction")
	}
	if got[0].Content != "system" || got[1].Content != "objective" {
		t.Fatalf("objective prefix changed: %#v", got[:2])
	}
	if info.AfterTokens >= info.BeforeTokens {
		t.Fatalf("tokens did not shrink: %#v", info)
	}
}
