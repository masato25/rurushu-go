package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/masato25/rurushu-go/tool"
)

func TestWatchToolCreateStatusStop(t *testing.T) {
	dir := t.TempDir()
	execCtx := &tool.ExecutionContext{Metadata: map[string]string{"session_id": "ses_test"}}
	create := &watchTool{stateDir: dir, mode: "create"}
	result, err := create.Execute(context.Background(), json.RawMessage(`{"interval":"10m","prompt":"Inspect monitor and report"}`), execCtx)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("create watch: %+v %v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "watch-ses_test.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state watchToolState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if !state.Active || state.Interval != "10m0s" || state.Prompt != "Inspect monitor and report" || time.Until(state.NextRunAt) < 9*time.Minute {
		t.Fatalf("unexpected watch state: %+v", state)
	}
	status := &watchTool{stateDir: dir, mode: "status"}
	result, err = status.Execute(context.Background(), nil, execCtx)
	if err != nil || !strings.Contains(result.Output, "Inspect monitor") {
		t.Fatalf("status: %+v %v", result, err)
	}
	stop := &watchTool{stateDir: dir, mode: "stop"}
	result, err = stop.Execute(context.Background(), nil, execCtx)
	if err != nil || result.IsError {
		t.Fatalf("stop: %+v %v", result, err)
	}
	data, err = os.ReadFile(filepath.Join(dir, "watch-ses_test.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil || state.Active {
		t.Fatalf("stop not persisted: %+v %v", state, err)
	}
}

func TestWatchToolRejectsInvalidInput(t *testing.T) {
	dir := t.TempDir()
	create := &watchTool{stateDir: dir, mode: "create"}
	for _, tc := range []struct{ session, args string }{
		{"../sessions/evil", `{"interval":"10m","prompt":"hello"}`},
		{"ses_valid", `{"interval":"0s","prompt":"hello"}`},
		{"ses_valid", `{"interval":"10m","prompt":""}`},
	} {
		result, err := create.Execute(context.Background(), json.RawMessage(tc.args), &tool.ExecutionContext{Metadata: map[string]string{"session_id": tc.session}})
		if err != nil || result == nil || !result.IsError {
			t.Fatalf("should reject %+v: %+v %v", tc, result, err)
		}
	}
}
