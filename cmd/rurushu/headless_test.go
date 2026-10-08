package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arborlogic/rurushu-go/harness"
	"github.com/arborlogic/rurushu-go/provider"
	"github.com/arborlogic/rurushu-go/tool"
)

func TestExecuteHeadlessRejectsUnsupportedVersion(t *testing.T) {
	resp, err := executeHeadless(context.Background(), headlessRequest{Version: 3, Task: "test"})
	if err == nil || resp.Status != "error" || resp.ErrorCode != "invalid_request" || !strings.Contains(resp.Error, "unsupported request version") {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestExecuteHeadlessRejectsExternalToolsInV1(t *testing.T) {
	resp, err := executeHeadless(context.Background(), headlessRequest{
		Version: 1, Task: "test", ExternalTools: []tool.ExternalSpec{{ID: "x", Description: "x", Command: "x", ReadOnly: true}},
	})
	if err == nil || resp.ErrorCode != "invalid_request" || !strings.Contains(resp.Error, "requires request version 2") {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestHeadlessRunRequestedDoesNotStealPositionalTUIDraft(t *testing.T) {
	if headlessRunRequested([]string{"tests"}) {
		t.Fatal("positional text after 'run' must remain a TUI draft, not a headless subcommand")
	}
	if !headlessRunRequested([]string{"--input", "request.json"}) {
		t.Fatal("explicit headless flags must select the headless command")
	}
}

func TestExecuteHeadlessRequiresTaskBeforeProviderAccess(t *testing.T) {
	resp, err := executeHeadless(context.Background(), headlessRequest{Version: 1})
	if err == nil || resp.Status != "error" || resp.ErrorCode != "invalid_request" || !strings.Contains(resp.Error, "task is required") {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestExecuteHeadlessValidatesExecutionPermissionBeforeWorkspaceSideEffects(t *testing.T) {
	cwd := t.TempDir()
	resp, err := executeHeadless(context.Background(), headlessRequest{
		Version: 1, Task: "test", CWD: cwd, Model: "model-a", ToolProfile: "execution", PermissionMode: "bogus",
	})
	if err == nil || resp.ErrorCode != "invalid_request" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if _, statErr := os.Stat(filepath.Join(cwd, ".rurushu")); !os.IsNotExist(statErr) {
		t.Fatalf("invalid request created Rurushu project state: stat err=%v", statErr)
	}

	resp, err = executeHeadless(context.Background(), headlessRequest{
		Version: 1, Task: "test", CWD: cwd, Model: "model-a", ToolProfile: "execution", PermissionMode: "deny",
	})
	if err == nil || resp.ErrorCode != "invalid_request" || !strings.Contains(resp.Error, "requires permission_mode") {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if _, statErr := os.Stat(filepath.Join(cwd, ".rurushu")); !os.IsNotExist(statErr) {
		t.Fatalf("denied execution request created Rurushu project state: stat err=%v", statErr)
	}
}

func TestRunHeadlessCLIWritesJSONErrorForMalformedRequest(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "request.json")
	output := filepath.Join(dir, "response.json")
	if err := os.WriteFile(input, []byte(`{"version":1,"task":`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runHeadlessCLI([]string{"--input", input, "--output", output})
	if err == nil {
		t.Fatal("expected malformed request error")
	}
	data, readErr := os.ReadFile(output)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var resp headlessResponse
	if json.Unmarshal(data, &resp) != nil || resp.Version != headlessProtocolVersion || resp.Status != "error" || !strings.Contains(resp.Error, "decode headless request") {
		t.Fatalf("response=%s", data)
	}
}

func TestDecodeHeadlessRequestRejectsTrailingJSON(t *testing.T) {
	_, err := decodeHeadlessRequest(strings.NewReader(`{"version":1,"task":"one"} {"version":1,"task":"two"}`))
	if err == nil || !strings.Contains(err.Error(), "trailing JSON value") {
		t.Fatalf("err=%v", err)
	}
}

func TestHeadlessFailurePreservesMachineReadableRetrySemantics(t *testing.T) {
	rateLimited := headlessFailure(&provider.HTTPError{StatusCode: 429, RetryAfter: 7 * time.Second})
	if rateLimited.ErrorCode != "rate_limited" || rateLimited.RetryAfter != 7000 {
		t.Fatalf("rate-limited response=%+v", rateLimited)
	}
	budget := headlessFailure(harness.ErrMaxSteps)
	if budget.ErrorCode != "budget_exhausted" {
		t.Fatalf("budget response=%+v", budget)
	}
	transient := headlessFailure(&provider.HTTPError{StatusCode: 503})
	if transient.ErrorCode != "transient" {
		t.Fatalf("transient response=%+v", transient)
	}
}
