package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/masato25/rurushu-go/permission"
	"github.com/masato25/rurushu-go/tool"
	"github.com/masato25/rurushu-go/tui"
)

func TestTerminalSendRequiresPermissionAndReportsSubmissionOnly(t *testing.T) {
	service := tui.NewTerminalService(t.TempDir())
	defer service.Close()
	sender := &terminalSendTool{service: service}
	raw := json.RawMessage(`{"command":"echo terminal_test"}`)
	denied, _ := sender.Execute(context.Background(), raw, &tool.ExecutionContext{AskPermission: func(permission.Request) bool { return false }})
	if !denied.IsError {
		t.Fatal("denied command was accepted")
	}
	var requested permission.Request
	accepted, _ := sender.Execute(context.Background(), raw, &tool.ExecutionContext{AskPermission: func(r permission.Request) bool { requested = r; return true }})
	if accepted.IsError || requested.Tool != "terminal_send" || accepted.Metadata["completed"] != false {
		t.Fatalf("unsafe terminal outcome: %+v; request=%+v", accepted, requested)
	}
	bad, _ := sender.Execute(context.Background(), json.RawMessage(`{"command":"echo a\necho b"}`), &tool.ExecutionContext{AskPermission: func(permission.Request) bool { return true }})
	if !bad.IsError || !strings.Contains(bad.Output, "one line") {
		t.Fatalf("newline accepted: %+v", bad)
	}
}
