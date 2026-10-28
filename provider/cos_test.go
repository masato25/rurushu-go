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
	"sync/atomic"
	"testing"
	"time"
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

func TestCOSWaitForHandoff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		status    int
		wantError string
	}{
		{name: "no job", body: `{"live":{"job":null}}`, status: 200},
		{name: "completed handoff", body: `{"live":{"job":{"stage":"done","busy":false}}}`, status: 200},
		{name: "failed handoff", body: `{"live":{"job":{"stage":"failed","busy":false}}}`, status: 200, wantError: "failed"},
		{name: "api failure", body: `{"error":"busy"}`, status: 503, wantError: "refusing unverified send"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/sessions/cos-left" || r.URL.Query().Get("live") != "1" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			p := &COSProvider{HTTPClient: server.Client()}
			err := p.waitForHandoff(context.Background(), server.URL, "token", "cos-left")
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("error=%v want %s", err, tc.wantError)
			}
		})
	}
}

func TestCOSWaitForHandoffRespectsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"live":{"job":{"stage":"opening","busy":true}}}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	p := &COSProvider{HTTPClient: server.Client()}
	if err := p.waitForHandoff(ctx, server.URL, "token", "cos-left"); err == nil {
		t.Fatal("in-progress handoff should not silently submit after cancellation")
	}
}

func TestCOSTurnStillActiveRequiresMatchingLiveTurn(t *testing.T) {
	active := "turn-left"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/cos-left" || r.URL.Query().Get("live") != "1" {
			t.Errorf("unexpected live request %s", r.URL)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"live": map[string]any{"activeTurnId": active}})
	}))
	defer server.Close()
	p := &COSProvider{HTTPClient: server.Client()}
	ctx := context.Background()
	start := time.Now()
	if !p.cosTurnStillActive(ctx, server.URL, "token", "cos-left", "turn-left", start) {
		t.Fatal("active correlated turn should be allowed to continue")
	}
	if p.cosTurnStillActive(ctx, server.URL, "token", "cos-left", "turn-right", start) {
		t.Fatal("must not extend timeout for another turn")
	}
	if p.cosTurnStillActive(ctx, server.URL, "token", "cos-left", "", start) {
		t.Fatal("must not extend timeout before correlating a turn")
	}
	if p.cosTurnStillActive(ctx, server.URL, "token", "cos-left", "turn-left", start.Add(-21*time.Minute)) {
		t.Fatal("must not extend an indefinitely stuck turn")
	}
}

func TestCOSCompactResumeUsesExistingMapping(t *testing.T) {
	requested := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sessions/cos-existing/compact-resume" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		requested++
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"live":{"job":{"stage":"handoff-pending"}}}`))
	}))
	defer server.Close()
	home := t.TempDir()
	dir := filepath.Join(home, "Library", "Application Support", "chat-on-steroids", "control-api")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	if err := os.WriteFile(filepath.Join(dir, "endpoint.json"), []byte(`{"port":`+port+`}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &COSProvider{HomeDir: home, CWD: t.TempDir(), HTTPClient: server.Client()}
	if err := p.saveSession("local-existing", "cos-existing"); err != nil {
		t.Fatal(err)
	}
	if err := p.CompactResume(context.Background(), "unmapped"); err == nil || !strings.Contains(err.Error(), "no COS mapping") {
		t.Fatalf("unmapped session error: %v", err)
	}
	if err := p.CompactResume(context.Background(), "local-existing"); err != nil {
		t.Fatal(err)
	}
	if requested != 1 {
		t.Fatalf("requests=%d, want 1", requested)
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
	writeJSON(filepath.Join(home, ".rurushugo", "cos.json"), `{"project":"global","projectUrl":"https://global"}`)
	writeJSON(filepath.Join(cwd, ".rurushugo", "cos.json"), `{"project":"local"}`)
	writeJSON(filepath.Join(cwd, ".openzerocode", "cos.json"), `{"project":"legacy-must-not-win"}`)
	t.Setenv("COS_PROJECT_URL", "https://env")
	p := &COSProvider{HomeDir: home, CWD: cwd}
	cfg := p.projectConfig()
	if cfg.Project != "local" || cfg.ProjectURL != "https://env" {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestCOSSessionMappingIsPerRurushuSession(t *testing.T) {
	p := &COSProvider{HomeDir: t.TempDir(), CWD: "/workspace"}
	if err := p.saveSession("local-a", "cos-a"); err != nil {
		t.Fatal(err)
	}
	if err := p.saveSession("local-b", "cos-b"); err != nil {
		t.Fatal(err)
	}
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
	if err := p.saveSession("local-existing", "cos-existing"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COS_NEW", "1")

	id, submitted, err := p.resolveSession(context.Background(), server.URL, "token", "local-existing", "follow-up")
	if err != nil {
		t.Fatal(err)
	}
	if id != "cos-existing" || submitted {
		t.Fatalf("resolveSession = (%q, %t), want (cos-existing, false)", id, submitted)
	}
}

func TestResolveSessionReusesStoredMappingEvenWhenRemoteIsEnded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/cos-ended" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		ended := int64(123)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session": map[string]any{"id": "cos-ended", "endedAt": ended},
		})
	}))
	defer server.Close()

	p := &COSProvider{HomeDir: t.TempDir(), CWD: "/workspace", HTTPClient: server.Client()}
	if err := p.saveSession("local-ended", "cos-ended"); err != nil {
		t.Fatal(err)
	}
	id, submitted, err := p.resolveSession(context.Background(), server.URL, "token", "local-ended", "follow-up")
	if err != nil {
		t.Fatal(err)
	}
	if id != "cos-ended" || submitted {
		t.Fatalf("resolveSession = (%q, %t), want (cos-ended, false)", id, submitted)
	}
}

func TestResolveSessionDoesNotReplaceUnavailableStoredMapping(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer server.Close()

	p := &COSProvider{
		HomeDir:    t.TempDir(),
		CWD:        "/workspace",
		HTTPClient: server.Client(),
		AutoSubmit: func(context.Context, string, string, string) (bool, error) {
			t.Fatal("unavailable stored mapping must not create a replacement session")
			return false, nil
		},
	}
	if err := p.saveSession("local-missing", "cos-missing"); err != nil {
		t.Fatal(err)
	}
	_, _, err := p.resolveSession(context.Background(), server.URL, "token", "local-missing", "follow-up")
	if err == nil || !strings.Contains(err.Error(), "refusing to create a replacement") {
		t.Fatalf("expected replacement refusal, got %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want exactly one lookup", requests)
	}
}

func TestCOSSessionMappingPersistsAtomically(t *testing.T) {
	home := t.TempDir()
	p := &COSProvider{HomeDir: home, CWD: "/workspace"}
	if err := p.saveSession("local-a", "cos-a"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".rurushu", "cos-sessions-map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got cosSessionMap
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Sessions["local-a"] != "cos-a" {
		t.Fatalf("mapping = %+v", got.Sessions)
	}
	info, err := os.Stat(filepath.Join(home, ".rurushu", "cos-sessions-map.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mapping permissions = %v", info.Mode().Perm())
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

func TestCOSStreamWaitsForTurnEndAfterFinalAssistantMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/events") {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nextFrom": 7,
			"events": []map[string]any{
				{"seq": 1, "kind": "user_message", "inputId": "opening"},
				{"seq": 2, "kind": "turn_start", "turnId": "turn-1"},
				{"seq": 3, "kind": "assistant_message", "turnId": "turn-1", "messageId": "first", "message": map[string]any{"text": "first part"}, "final": true},
				{"seq": 4, "kind": "tool_call", "turnId": "turn-1", "tool": map[string]any{"callId": "tool-1", "name": "read"}},
				{"seq": 5, "kind": "assistant_message", "turnId": "turn-1", "messageId": "second", "message": map[string]any{"text": "second part"}, "state": "final"},
				{"seq": 6, "kind": "turn_end", "turnId": "turn-1"},
			},
		})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := &COSProvider{HTTPClient: server.Client()}
	out := make(chan StreamEvent)
	go p.runStream(ctx, out, server.URL, "token", "session-1", "prompt", true)
	var response strings.Builder
	done := false
	for event := range out {
		switch event.Type {
		case EventReplace:
			response.Reset()
			response.WriteString(event.Text)
		case EventDone:
			done = true
		case EventError:
			t.Fatalf("stream error: %v", event.Error)
		}
	}
	if !done || !strings.Contains(response.String(), "first part") || !strings.Contains(response.String(), "second part") {
		t.Fatalf("incomplete turn: done=%v response=%q", done, response.String())
	}
}

func TestCOSStreamHandlesRevisedMessageSnapshots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"nextFrom": 6, "events": []map[string]any{
			{"seq": 1, "kind": "user_message", "inputId": "opening"},
			{"seq": 2, "kind": "turn_start", "turnId": "turn-1"},
			{"seq": 3, "kind": "assistant_message", "turnId": "turn-1", "messageId": "one", "message": map[string]any{"text": "func main() { incomplete"}},
			{"seq": 4, "kind": "assistant_message", "turnId": "turn-1", "messageId": "one", "message": map[string]any{"text": "func main() { complete }"}},
			{"seq": 5, "kind": "turn_end", "turnId": "turn-1"},
		}})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := &COSProvider{HTTPClient: server.Client()}
	out := make(chan StreamEvent)
	go p.runStream(ctx, out, server.URL, "token", "session-1", "prompt", true)
	var last string
	for event := range out {
		if event.Type == EventReplace {
			last = event.Text
		}
		if event.Type == EventError {
			t.Fatal(event.Error)
		}
	}
	if last != "func main() { complete }" {
		t.Fatalf("snapshot was not replaced: %q", last)
	}
}

func TestCOSStreamReceivesFinalSnapshotAfterTurnEnd(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		poll := polls.Add(1)
		events := []map[string]any{
			{"seq": 1, "kind": "user_message", "inputId": "opening"},
			{"seq": 2, "kind": "turn_start", "turnId": "turn-1"},
			{"seq": 3, "kind": "assistant_message", "turnId": "turn-1", "messageId": "msg-1", "message": map[string]any{"text": "partial"}},
			{"seq": 4, "kind": "turn_end", "turnId": "turn-1"},
		}
		if poll >= 2 {
			events = append(events, map[string]any{"seq": 5, "kind": "assistant_message", "turnId": "turn-1", "messageId": "msg-1", "message": map[string]any{"text": "complete response after turn end"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"nextFrom": 6, "events": events})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := &COSProvider{HTTPClient: server.Client()}
	out := make(chan StreamEvent)
	go p.runStream(ctx, out, server.URL, "token", "session-1", "prompt", true)
	var last string
	done := false
	for ev := range out {
		if ev.Type == EventReplace {
			last = ev.Text
		}
		if ev.Type == EventDone {
			done = true
		}
		if ev.Type == EventError {
			t.Fatal(ev.Error)
		}
	}
	if !done || last != "complete response after turn end" {
		t.Fatalf("truncated COS reply, done=%t response=%q", done, last)
	}
}

func TestCOSStreamRejectsInterruptedTurnEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"nextFrom": 5, "events": []map[string]any{
			{"seq": 1, "kind": "user_message", "inputId": "opening"},
			{"seq": 2, "kind": "turn_start", "turnId": "turn-1"},
			{"seq": 3, "kind": "assistant_message", "turnId": "turn-1", "messageId": "first", "message": map[string]any{"text": "incomplete"}},
			{"seq": 4, "kind": "turn_end", "turnId": "turn-1", "outcome": "interrupted"},
		}})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	p := &COSProvider{HTTPClient: server.Client()}
	out := make(chan StreamEvent)
	go p.runStream(ctx, out, server.URL, "token", "session-1", "prompt", true)
	var gotError bool
	for ev := range out {
		if ev.Type == EventDone {
			t.Fatal("interrupted generation was marked successful")
		}
		if ev.Type == EventError && ev.Error != nil && strings.Contains(ev.Error.Error(), "interrupted") {
			gotError = true
		}
	}
	if !gotError {
		t.Fatal("expected interrupted turn error")
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
