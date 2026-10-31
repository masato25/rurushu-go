package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/masato25/rurushu-go/permission"
)

func TestPermissionHandlerAutoApprove(t *testing.T) {
	handler, requests := newPermissionHandler(true)
	if requests != nil {
		t.Fatal("auto approve must not start an interactive permission listener")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	approved, err := handler.Ask(ctx, permission.Request{Tool: "bash", Pattern: "make test"})
	if err != nil || !approved {
		t.Fatalf("approved = %v, err = %v", approved, err)
	}
}

func TestPermissionHandlerManualApproval(t *testing.T) {
	for _, decision := range []bool{true, false} {
		handler, requests := newPermissionHandler(false)
		if requests == nil {
			t.Fatal("manual approval requires an interactive permission listener")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		result := make(chan bool, 1)
		go func() {
			approved, err := handler.Ask(ctx, permission.Request{Tool: "bash"})
			if err != nil {
				t.Errorf("Ask: %v", err)
			}
			result <- approved
		}()
		select {
		case prompt := <-requests:
			prompt.Resolve(decision)
		case <-ctx.Done():
			cancel()
			t.Fatal("permission prompt not received")
		}
		if approved := <-result; approved != decision {
			t.Fatalf("approved = %v, want %v", approved, decision)
		}
		cancel()
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty(" ", " second ", "third"); got != "second" {
		t.Fatalf("got %q", got)
	}
}

func TestRunRequiresModel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("RURUSHU_MODEL", "")
	t.Setenv("OPENAI_MODEL", "")
	err := run([]string{"--cwd", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestContinueLastWithoutSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := run([]string{"--provider", "cos", "--cwd", t.TempDir(), "-c"})
	if err == nil || !strings.Contains(err.Error(), "no previous session") {
		t.Fatalf("-c without sessions: %v", err)
	}
}

func TestContinueLastRejectsResumeConflict(t *testing.T) {
	err := run([]string{"-c", "--resume", "last"})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("-c with --resume: %v", err)
	}
}

func TestRunListModelsWithoutModel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"test-model"}]}`)
	}))
	defer server.Close()

	t.Setenv("RURUSHU_MODEL", "")
	t.Setenv("OPENAI_MODEL", "")
	if err := run([]string{"--base-url", server.URL, "--list-models", "--cwd", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

func TestReloadArgsPreservesSessionAndProvider(t *testing.T) {
	got := reloadArgs([]string{"-provider", "cos", "-c", "--activity", "debug"}, "ses_target")
	want := []string{"-provider", "cos", "--activity", "debug", "--resume", "ses_target"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reload args = %q, want %q", got, want)
	}
	got = reloadArgs([]string{"--resume", "ses_other", "--model", "test"}, "ses_target")
	want = []string{"--model", "test", "--resume", "ses_target"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reload args = %q, want %q", got, want)
	}
}

func TestPprofRejectsNonLoopback(t *testing.T) {
	for _, address := range []string{"0.0.0.0:6060", ":6060", "example.com:6060", "not-an-address"} {
		if err := startLocalPprof(address); err == nil {
			t.Fatalf("accepted unsafe address: %s", address)
		}
	}
}
