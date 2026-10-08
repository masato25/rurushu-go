package tool

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/arborlogic/rurushu-go/permission"
)

func TestExternalToolForwardsJSONAndReadOnlySkipsPermission(t *testing.T) {
	t.Setenv("GO_WANT_EXTERNAL_TOOL_HELPER", "1")
	ext, err := NewExternal(ExternalSpec{
		ID: "sample", Description: "sample tool", Command: os.Args[0],
		Args: []string{"-test.run=TestExternalToolHelperProcess"}, ReadOnly: true,
		InputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	result, err := ext.Execute(context.Background(), json.RawMessage(`{"value":"ok"}`), &ExecutionContext{
		CWD: t.TempDir(), AskPermission: func(_ permission.Request) bool { asked = true; return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if asked || result.IsError || result.Output != "ok" {
		t.Fatalf("asked=%v result=%+v", asked, result)
	}
}

func TestExternalToolHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_EXTERNAL_TOOL_HELPER") != "1" {
		return
	}
	var payload struct {
		Value string `json:"value"`
	}
	if json.NewDecoder(os.Stdin).Decode(&payload) != nil || payload.Value != "ok" {
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString("ok")
	os.Exit(0)
}

func TestExternalToolRequiresPermissionWhenMutable(t *testing.T) {
	ext, err := NewExternal(ExternalSpec{ID: "mutate", Description: "mutate", Command: "never-run"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ext.Execute(context.Background(), json.RawMessage(`{}`), &ExecutionContext{
		AskPermission: func(permission.Request) bool { return false },
	})
	if err != nil || !result.IsError || !strings.Contains(result.Output, "permission denied") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestExternalToolPreauthorizedMutableSkipsHarnessPermission(t *testing.T) {
	t.Setenv("GO_WANT_EXTERNAL_TOOL_HELPER", "1")
	ext, err := NewExternal(ExternalSpec{
		ID: "bounded-write", Description: "caller-enforced write", Command: os.Args[0],
		Args: []string{"-test.run=TestExternalToolHelperProcess"}, Preauthorized: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	result, err := ext.Execute(context.Background(), json.RawMessage(`{"value":"ok"}`), &ExecutionContext{
		AskPermission: func(permission.Request) bool { asked = true; return false },
	})
	if err != nil || asked || result.IsError || result.Output != "ok" {
		t.Fatalf("asked=%v result=%+v err=%v", asked, result, err)
	}
}
