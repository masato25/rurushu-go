package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestFormatCOSPrompt(t *testing.T) {
	req := CompletionRequest{Messages: []Message{
		{Role: RoleSystem, Content: "instructions"},
		{Role: RoleUser, Content: "first task"},
	}}
	got := formatCOSPrompt(req, filepath.Join("tmp", "acme", "widget"))
	if !strings.Contains(got, "[Project Workspace: acme/widget]") || !strings.HasSuffix(got, "first task") {
		t.Fatalf("unexpected prompt: %q", got)
	}

	req.Messages = append(req.Messages, Message{Role: RoleAssistant, Content: "done"}, Message{Role: RoleUser, Content: "next"})
	if got := formatCOSPrompt(req, filepath.Join("tmp", "acme", "widget")); got != "next" {
		t.Fatalf("follow-up prompt = %q, want %q", got, "next")
	}
}

func TestCOSProjectConfigPrecedence(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	writeJSON := func(path, value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(filepath.Join(home, ".openzerocode", "cos.json"), `{"project":"global","projectUrl":"https://global"}`)
	writeJSON(filepath.Join(cwd, ".openzerocode", "cos.json"), `{"project":"local"}`)
	t.Setenv("COS_PROJECT_URL", "https://env")
	p := &COSProvider{HomeDir: home, CWD: cwd}
	cfg := p.projectConfig()
	if cfg.Project != "local" || cfg.ProjectURL != "https://env" {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestCOSSessionMappingIsPerRurushuSession(t *testing.T) {
	p := &COSProvider{HomeDir: t.TempDir(), CWD: "/workspace"}
	p.saveSession("local-a", "cos-a")
	p.saveSession("local-b", "cos-b")
	if got := p.storedSession("local-a"); got != "cos-a" {
		t.Fatalf("local-a = %q", got)
	}
	if got := p.storedSession("local-b"); got != "cos-b" {
		t.Fatalf("local-b = %q", got)
	}
	if got := p.storedSession("unknown"); got != "" {
		t.Fatalf("unknown = %q", got)
	}
}

func TestResolveSessionReusesStoredMappingWhenCOSNewIsSet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/cos-existing" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session": map[string]any{"id": "cos-existing", "endedAt": nil},
		})
	}))
	defer server.Close()

	p := &COSProvider{HomeDir: t.TempDir(), CWD: "/workspace", HTTPClient: server.Client()}
	p.saveSession("local-existing", "cos-existing")
	t.Setenv("COS_NEW", "1")

	id, submitted, err := p.resolveSession(context.Background(), server.URL, "token", "local-existing", "follow-up")
	if err != nil {
		t.Fatal(err)
	}
	if id != "cos-existing" || submitted {
		t.Fatalf("resolveSession = (%q, %t), want (cos-existing, false)", id, submitted)
	}
}

func TestCorrelateCOSTurn(t *testing.T) {
	events := []cosEvent{
		{Seq: 1, Kind: "user_message", InputID: "other"},
		{Seq: 2, Kind: "turn_start", TurnID: "wrong"},
		{Seq: 3, Kind: "user_message", InputID: "ours"},
		{Seq: 4, Kind: "turn_start", TurnID: "right"},
	}
	if got := correlateCOSTurn(events, "ours", false); got != "right" {
		t.Fatalf("turn = %q, want right", got)
	}
}

func TestRandomIDIsUUID(t *testing.T) {
	id := randomID()
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("randomID() = %q, want UUID v4", id)
	}
}

func TestCreateSessionReturnsAlreadySubmitted(t *testing.T) {
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		payload := map[string]any{"activeId": "old", "sessions": []map[string]any{{"id": "old", "events": 1}}}
		if polls > 1 {
			payload = map[string]any{"activeId": "new", "sessions": []map[string]any{{"id": "new", "events": 2}}}
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()

	p := &COSProvider{
		CWD:        t.TempDir(),
		HTTPClient: server.Client(),
		AutoSubmit: func(context.Context, string, string, string) (bool, error) { return true, nil },
		OpenURL:    func(string) error { t.Fatal("fallback browser open should not run"); return nil },
	}
	t.Setenv("COS_PROJECT_URL", "https://chatgpt.example/project")
	id, submitted, err := p.createSession(context.Background(), server.URL, "token", "opening prompt")
	if err != nil {
		t.Fatal(err)
	}
	if id != "new" || !submitted {
		t.Fatalf("createSession = (%q, %t), want (new, true)", id, submitted)
	}
}

func TestCreateSessionManualFallbackIsAlreadySubmitted(t *testing.T) {
	polls := 0
	opened := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		payload := map[string]any{"activeId": "old", "sessions": []map[string]any{{"id": "old", "events": 1}}}
		if polls > 1 {
			payload = map[string]any{"activeId": "new", "sessions": []map[string]any{{"id": "new", "events": 2}}}
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()

	p := &COSProvider{
		CWD:        t.TempDir(),
		HTTPClient: server.Client(),
		AutoSubmit: func(context.Context, string, string, string) (bool, error) { return false, nil },
		OpenURL:    func(string) error { opened = true; return nil },
	}
	t.Setenv("COS_PROJECT_URL", "https://chatgpt.example/project")
	id, submitted, err := p.createSession(context.Background(), server.URL, "token", "opening prompt")
	if err != nil {
		t.Fatal(err)
	}
	if !opened {
		t.Fatal("manual fallback did not open project URL")
	}
	if id != "new" || !submitted {
		t.Fatalf("createSession = (%q, %t), want (new, true)", id, submitted)
	}
}
