package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/arborlogic/rurushu-go/jobs"
	"github.com/arborlogic/rurushu-go/permission"
	"github.com/arborlogic/rurushu-go/tool"
)

func TestBashRequiresPermission(t *testing.T) {
	root := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"command": "echo should-not-run"})
	result, err := (&BashTool{}).Execute(context.Background(), raw, &tool.ExecutionContext{CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Output, "permission handler is required") {
		t.Fatalf("result=%+v", result)
	}
}

func TestBashForegroundRunsAfterApproval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell assertion is Unix-oriented")
	}
	root := t.TempDir()
	approved := false
	execCtx := &tool.ExecutionContext{
		CWD: root,
		AskPermission: func(req permission.Request) bool {
			approved = req.Tool == "bash" && strings.Contains(req.Pattern, "printf")
			return approved
		},
	}
	raw, _ := json.Marshal(map[string]any{"command": "printf 'hello-rurushu'"})
	result, err := (&BashTool{}).Execute(context.Background(), raw, execCtx)
	if err != nil {
		t.Fatal(err)
	}
	if !approved || result.IsError || result.Output != "hello-rurushu" {
		t.Fatalf("approved=%v result=%+v", approved, result)
	}
}

func TestRegisterExecution(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".rurushu")
	manager, err := jobs.Open(root, stateDir, "test-rurushu")
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	RegisterExecution(registry, manager)
	for _, id := range []string{"bash", "job_list", "job_output", "job_stop"} {
		if _, ok := registry.Get(id); !ok {
			t.Fatalf("tool %q not registered", id)
		}
	}
}
