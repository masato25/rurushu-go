package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	cosPollInterval             = 600 * time.Millisecond
	cosSessionDiscoveryInterval = 1200 * time.Millisecond
	cosHandoffPollInterval      = 2 * time.Second
	cosIdleTimeout              = 108 * time.Second
)

// COSProvider connects Rurushu to the Chat On Steroids local Control API.
type COSProvider struct {
	CWD        string
	HTTPClient *http.Client
	HomeDir    string
	Stderr     io.Writer
	AutoSubmit func(ctx context.Context, projectURL, prompt, projectName string) (bool, error)
	OpenURL    func(string) error

	// Test hooks. Zero keeps production defaults. Keeping timing on the
	// provider instance avoids package-global mutation/races in parallel tests.
	pollInterval             time.Duration
	sessionDiscoveryInterval time.Duration
	handoffPollInterval      time.Duration

	mu       sync.Mutex
	bindings map[string]string
}

func (p *COSProvider) pollDelay() time.Duration {
	if p.pollInterval > 0 {
		return p.pollInterval
	}
	return cosPollInterval
}

func (p *COSProvider) discoveryDelay() time.Duration {
	if p.sessionDiscoveryInterval > 0 {
		return p.sessionDiscoveryInterval
	}
	return cosSessionDiscoveryInterval
}

func (p *COSProvider) handoffDelay() time.Duration {
	if p.handoffPollInterval > 0 {
		return p.handoffPollInterval
	}
	return cosHandoffPollInterval
}

func NewCOSProvider(cwd string) *COSProvider {
	return &COSProvider{CWD: cwd, HTTPClient: &http.Client{}, Stderr: os.Stderr}
}

func (p *COSProvider) ID() string { return "cos" }

func (p *COSProvider) ListModels(context.Context) ([]string, error) {
	return []string{"chatgpt-web", "cos"}, nil
}

// CompactResume requests COS's native in-place conversation handoff for an
// existing Rurushu session. It never creates a new remote session.
func (p *COSProvider) CompactResume(ctx context.Context, localID string) error {
	if strings.TrimSpace(localID) == "" {
		return errors.New("local session id is required")
	}
	remote := p.storedSession(localID)
	if remote == "" {
		return fmt.Errorf("session %s has no COS mapping; refusing to create a session", localID)
	}
	baseURL, token, err := p.credentials()
	if err != nil {
		return err
	}
	status, body, err := p.request(ctx, http.MethodPost, baseURL+"/v1/sessions/"+url.PathEscape(remote)+"/compact-resume", token, map[string]any{})
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("COS compact-resume refused (%d): %s", status, strings.TrimSpace(body))
	}
	return nil
}

type cosProjectConfig struct {
	Project    string `json:"project"`
	ProjectURL string `json:"projectUrl"`
}

type cosSessionMap struct {
	Sessions map[string]string `json:"sessions"`
	ByCWD    map[string]string `json:"byCwd"`
}

type cosSession struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	ConversationID *string `json:"conversationId"`
	EndedAt        *int64  `json:"endedAt"`
	Events         int     `json:"events"`
}

type cosChromeResult struct {
	OK        bool   `json:"ok"`
	Submitted bool   `json:"submitted"`
	Reason    string `json:"reason"`
	Error     string `json:"error"`
}

type cosEvent struct {
	Seq       int    `json:"seq"`
	Position  *int   `json:"position"`
	Kind      string `json:"kind"`
	TurnID    string `json:"turnId"`
	MessageID string `json:"messageId"`
	InputID   string `json:"inputId"`
	Final     bool   `json:"final"`
	State     string `json:"state"`
	Outcome   string `json:"outcome"`
	Message   *struct {
		Text string `json:"text"`
	} `json:"message"`
	Tool *struct {
		CallID  string `json:"callId"`
		Name    string `json:"name"`
		Summary *struct {
			Title string `json:"title"`
		} `json:"summary"`
	} `json:"tool"`
}

func (p *COSProvider) Stream(ctx context.Context, req CompletionRequest) (<-chan StreamEvent, error) {
	if strings.TrimSpace(req.SessionID) == "" && strings.TrimSpace(os.Getenv("COS_SESSION_ID")) == "" {
		return nil, errors.New("Chat On Steroids requires a Rurushu session id")
	}
	baseURL, token, err := p.credentials()
	if err != nil {
		return nil, err
	}
	// COS owns its remote conversation history. Request its native in-place
	// handoff at coarse, deterministic milestones rather than attempting to
	// compact local messages or creating a replacement remote session.
	// Failures are nonfatal: a follow-up must never be lost to maintenance.
	if shouldAutoCompactCOS(req.Messages) && p.storedSession(req.SessionID) != "" {
		compactCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		if err := p.CompactResume(compactCtx, req.SessionID); err != nil {
			p.logf("COS auto compact skipped: %v\n", err)
		}
		cancel()
	}
	prompt := formatCOSPrompt(req, p.CWD)
	sessionID, alreadySubmitted, err := p.resolveSession(ctx, baseURL, token, req.SessionID, prompt)
	if err != nil {
		return nil, err
	}

	out := make(chan StreamEvent)
	go p.runStreamSelected(ctx, out, baseURL, token, sessionID, prompt, alreadySubmitted, req.Model, req.ReasoningEffort)
	return out, nil
}

func shouldAutoCompactCOS(messages []Message) bool {
	turns := 0
	for _, message := range messages {
		if message.Role == RoleUser {
			turns++
		}
	}
	return turns >= 100 && turns%100 == 0
}

func (p *COSProvider) runStream(ctx context.Context, out chan<- StreamEvent, baseURL, token, sessionID, prompt string, alreadySubmitted bool) {
	p.runStreamSelected(ctx, out, baseURL, token, sessionID, prompt, alreadySubmitted, "", "")
}

func (p *COSProvider) runStreamSelected(ctx context.Context, out chan<- StreamEvent, baseURL, token, sessionID, prompt string, alreadySubmitted bool, model, reasoningEffort string) {
	defer close(out)
	// COS retains the same session ID across Compact & Resume. Never submit a
	// follow-up into its source chat while the rebind transaction is in flight.
	if !alreadySubmitted {
		if err := p.waitForHandoff(ctx, baseURL, token, sessionID); err != nil {
			p.emitError(out, err)
			return
		}
	}
	nextFrom := 0
	inputID := randomID()
	if !alreadySubmitted {
		for {
			cursor, err := p.eventCursor(ctx, baseURL, token, sessionID)
			if err == nil {
				nextFrom = cursor
			}
			payload := cosInputPayload(inputID, sessionID, prompt, model, reasoningEffort)
			status, body, err := p.request(ctx, http.MethodPost, baseURL+"/v1/inputs", token, payload)
			if err == nil && status >= 200 && status < 300 {
				break
			}
			if status == http.StatusTooManyRequests {
				busy, stage, stateErr := p.handoffBusy(ctx, baseURL, token, sessionID)
				if stateErr == nil && busy {
					p.logf("COS input paused while Compact & Resume is %s; waiting for handoff completion\n", stage)
					if waitErr := p.waitForHandoff(ctx, baseURL, token, sessionID); waitErr != nil {
						p.emitError(out, waitErr)
						return
					}
					inputID = randomID()
					continue
				}
			}
			if status != http.StatusConflict || !strings.Contains(body, "would_interrupt") {
				p.emitError(out, fmt.Errorf("Chat On Steroids input error (%d): %s", status, strings.TrimSpace(body)))
				return
			}
			if !sleepContext(ctx, p.pollDelay()) {
				p.emitError(out, ctx.Err())
				return
			}
			inputID = randomID()
		}
	} else {
		nextFrom = 1
	}

	// COS sends complete message snapshots, which can be revised (not just
	// extended). Keep the ordered parts so revisions replace stale content.
	parts := []string{}
	messageParts := map[string]int{}
	emitSnapshot := func() {
		out <- StreamEvent{Type: EventReplace, Text: strings.Join(parts, "")}
	}
	seenTools := map[string]bool{}
	var correlation []cosEvent
	remoteTurnID := ""
	turnEnded := false
	turnOutcome := ""
	quietAfterEnd := 0
	lastMatchedSeq := -1
	lastActivity := time.Now()
	startedWaiting := lastActivity
	for {
		if !sleepContext(ctx, p.pollDelay()) {
			p.cancelInput(baseURL, token, inputID)
			p.emitError(out, ctx.Err())
			return
		}
		var payload struct {
			Events   []cosEvent `json:"events"`
			NextFrom int        `json:"nextFrom"`
		}
		path := fmt.Sprintf("%s/v1/sessions/%s/events?from=%d&limit=50", baseURL, url.PathEscape(sessionID), nextFrom)
		if err := p.getJSON(ctx, path, token, &payload); err != nil {
			if time.Since(lastActivity) > cosIdleTimeout {
				if !p.cosTurnStillActive(ctx, baseURL, token, sessionID, remoteTurnID, startedWaiting) {
					p.emitError(out, fmt.Errorf("timeout reading Chat On Steroids events (turn=%q): %w", remoteTurnID, err))
					return
				}
				lastActivity = time.Now()
			}
			continue
		}
		if payload.NextFrom > 0 {
			nextFrom = payload.NextFrom
		}
		// A terminal turn_end may be journaled before a final assistant
		// snapshot becomes visible. Do not close the stream until the journal
		// has been polled again without matching updates.
		matchingUpdate := false
		for _, event := range payload.Events {
			if event.Kind == "user_message" || event.Kind == "turn_start" || event.Kind == "turn_end" {
				correlation = append(correlation, event)
				if remoteTurnID == "" {
					remoteTurnID = correlateCOSTurn(correlation, inputID, alreadySubmitted)
				}
			}
			if remoteTurnID == "" || event.TurnID != remoteTurnID {
				continue
			}
			fresh := event.Seq > lastMatchedSeq
			if fresh {
				lastMatchedSeq = event.Seq
				// Any fresh event owned by the correlated native turn proves the
				// turn is still settling. In particular, COS may journal tool/page
				// activity around a turn_end boundary; counting only assistant text
				// as activity can close the Rurushu stream while ChatGPT still works.
				matchingUpdate = true
			}
			lastActivity = time.Now()
			switch event.Kind {
			case "turn_end":
				turnEnded = true
				turnOutcome = event.Outcome
			case "tool_call":
				if event.Tool != nil && !seenTools[event.Tool.CallID] {
					seenTools[event.Tool.CallID] = true
					title := event.Tool.Name
					if event.Tool.Summary != nil && event.Tool.Summary.Title != "" {
						title = event.Tool.Summary.Title
					}
					parts = append(parts, fmt.Sprintf("\n> ⚙️ `[Tool: %s]` %s\n", event.Tool.Name, title))
					emitSnapshot()
				}
			case "assistant_message":
				if event.Message == nil {
					continue
				}
				mid := event.MessageID
				if mid == "" {
					mid = "latest"
				}
				if index, ok := messageParts[mid]; ok {
					if parts[index] != event.Message.Text {
						parts[index] = event.Message.Text
						emitSnapshot()
					}
				} else {
					messageParts[mid] = len(parts)
					parts = append(parts, event.Message.Text)
					emitSnapshot()
				}
				// A final assistant message closes only that message, not the
				// entire round. More assistant messages can follow tool calls.
			}
		}
		if turnEnded {
			if matchingUpdate {
				quietAfterEnd = 0
			} else {
				quietAfterEnd++
			}
			if quietAfterEnd >= 2 {
				// The append-only journal and live browser projection can settle at
				// slightly different times. A recorded turn_end is not sufficient to
				// stop the single-chat UI while COS still reports this exact native
				// turn as active. Keep the provider channel open so the TUI spinner and
				// subsequent response/tool snapshots continue to flow.
				active, liveErr := p.cosTurnActive(ctx, baseURL, token, sessionID, remoteTurnID, startedWaiting)
				if liveErr != nil {
					if time.Since(lastActivity) > cosIdleTimeout {
						p.emitError(out, fmt.Errorf("cannot verify Chat On Steroids turn completion after turn_end (turn=%q): %w", remoteTurnID, liveErr))
						return
					}
					continue
				}
				if active {
					continue
				}
				if turnOutcome != "" && turnOutcome != "completed" {
					p.emitError(out, fmt.Errorf("Chat On Steroids turn ended with outcome %q (turn=%q); refusing automatic replay", turnOutcome, remoteTurnID))
					return
				}
				out <- StreamEvent{Type: EventDone}
				return
			}
		}
		if time.Since(lastActivity) > cosIdleTimeout {
			if !p.cosTurnStillActive(ctx, baseURL, token, sessionID, remoteTurnID, startedWaiting) {
				p.emitError(out, fmt.Errorf("timeout waiting for Chat On Steroids generation (no matching event for %s, turn=%q)", cosIdleTimeout, remoteTurnID))
				return
			}
			lastActivity = time.Now()
		}
	}
}

func cosInputPayload(inputID, sessionID, prompt, model, reasoningEffort string) map[string]any {
	payload := map[string]any{"id": inputID, "sessionId": sessionID, "text": prompt}
	model = strings.TrimSpace(model)
	reasoningEffort = strings.TrimSpace(reasoningEffort)
	if model != "" && model != "chatgpt-web" && reasoningEffort != "" {
		payload["model"] = model
		payload["reasoningEffort"] = reasoningEffort
	}
	return payload
}

// A live native turn may legitimately have no journal events during extended
// reasoning or tool execution. Keep waiting only if COS confirms the exact
// correlated turn is active, with an absolute bound against stuck generations.
func (p *COSProvider) cosTurnStillActive(ctx context.Context, baseURL, token, sessionID, turnID string, started time.Time) bool {
	active, err := p.cosTurnActive(ctx, baseURL, token, sessionID, turnID, started)
	return err == nil && active
}

func (p *COSProvider) cosTurnActive(ctx context.Context, baseURL, token, sessionID, turnID string, started time.Time) (bool, error) {
	if turnID == "" || time.Since(started) >= 20*time.Minute {
		return false, nil
	}
	var detail struct {
		Live *struct {
			ActiveTurnID string `json:"activeTurnId"`
		} `json:"live"`
	}
	if err := p.getJSON(ctx, baseURL+"/v1/sessions/"+url.PathEscape(sessionID)+"?live=1", token, &detail); err != nil {
		return false, err
	}
	return detail.Live != nil && detail.Live.ActiveTurnID == turnID, nil
}

func (p *COSProvider) waitForHandoff(ctx context.Context, baseURL, token, sessionID string) error {
	deadline := time.NewTimer(10 * time.Minute)
	defer deadline.Stop()
	for {
		busy, stage, err := p.handoffBusy(ctx, baseURL, token, sessionID)
		if err != nil {
			return fmt.Errorf("cannot verify COS handoff state; refusing unverified send: %w", err)
		}
		if !busy {
			if stage == "failed" {
				return errors.New("COS Compact & Resume failed; refusing automatic replay")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("COS Compact & Resume remains in progress after 10 minutes; follow-up not sent")
		case <-time.After(p.handoffDelay()):
		}
	}
}

func (p *COSProvider) handoffBusy(ctx context.Context, baseURL, token, sessionID string) (bool, string, error) {
	var state struct {
		Live *struct {
			Job *struct {
				Stage string `json:"stage"`
				Busy  bool   `json:"busy"`
			} `json:"job"`
		} `json:"live"`
	}
	err := p.getJSON(ctx, baseURL+"/v1/sessions/"+url.PathEscape(sessionID)+"?live=1", token, &state)
	if err != nil {
		return false, "", err
	}
	if state.Live == nil || state.Live.Job == nil {
		return false, "", nil
	}
	return state.Live.Job.Busy, state.Live.Job.Stage, nil
}

func (p *COSProvider) emitError(out chan<- StreamEvent, err error) {
	if err != nil {
		out <- StreamEvent{Type: EventError, Error: err}
	}
}

func (p *COSProvider) credentials() (string, string, error) {
	home := p.HomeDir
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
	}
	dir := filepath.Join(home, "Library", "Application Support", "chat-on-steroids", "control-api")
	endpointData, err := os.ReadFile(filepath.Join(dir, "endpoint.json"))
	if err != nil {
		return "", "", errors.New("Chat On Steroids Control API is unavailable; start Chat On Steroids and enable controlApi")
	}
	var endpoint struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(endpointData, &endpoint); err != nil || endpoint.Port <= 0 {
		return "", "", errors.New("invalid Chat On Steroids Control API endpoint")
	}
	token, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil || strings.TrimSpace(string(token)) == "" {
		return "", "", errors.New("Chat On Steroids Control API token is unavailable")
	}
	return fmt.Sprintf("http://127.0.0.1:%d", endpoint.Port), strings.TrimSpace(string(token)), nil
}

func (p *COSProvider) resolveSession(ctx context.Context, baseURL, token, localID, prompt string) (string, bool, error) {
	if forced := strings.TrimSpace(os.Getenv("COS_SESSION_ID")); forced != "" {
		return forced, false, nil
	}
	if bound := p.boundSession(localID); bound != "" {
		return bound, false, nil
	}
	if stored := p.storedSession(localID); stored != "" {
		var data struct {
			Session cosSession `json:"session"`
		}
		if err := p.getJSON(ctx, baseURL+"/v1/sessions/"+url.PathEscape(stored), token, &data); err != nil {
			return "", false, fmt.Errorf("mapped COS session %s for local session %s is unavailable; refusing to create a replacement automatically: %w", stored, localID, err)
		}
		if data.Session.ID == "" {
			return "", false, fmt.Errorf("mapped COS session %s for local session %s returned no session id; refusing to create a replacement automatically", stored, localID)
		}
		// A local Rurushu session owns exactly one remote COS session. Do not
		// silently replace it when COS marks the remote session ended; doing so
		// causes resume/error retries to accumulate remote sessions. Reuse the
		// mapped id and let the input API report whether it can accept another
		// turn. Starting a replacement requires an explicit new local session.
		p.bindSession(localID, stored)
		return stored, false, nil
	}
	// COS_NEW only controls creation for an unbound local Rurushu session.
	// A resumed local session with an existing mapping must keep using the
	// previously associated remote COS session.
	id, submitted, err := p.createSession(ctx, baseURL, token, prompt)
	if err != nil {
		return "", false, err
	}
	p.bindSession(localID, id)
	if err := p.saveSession(localID, id); err != nil {
		return "", false, fmt.Errorf("persist COS session mapping: %w", err)
	}
	return id, submitted, nil
}

func (p *COSProvider) createSession(ctx context.Context, baseURL, token, prompt string) (string, bool, error) {
	var initial struct {
		ActiveID string       `json:"activeId"`
		Sessions []cosSession `json:"sessions"`
	}
	_ = p.getJSON(ctx, baseURL+"/v1/sessions?limit=50", token, &initial)
	known := map[string]bool{}
	for _, session := range initial.Sessions {
		known[session.ID] = true
	}
	cfg := p.projectConfig()
	projectURL, err := p.projectURL(cfg)
	if err != nil {
		return "", false, err
	}
	projectName := cfg.Project
	if projectName == "" {
		projectName = "ChatGPT Project"
	}
	p.logf("[cos] Opening ChatGPT Project and submitting the opening prompt: %s\n", projectURL)
	autoSubmit := p.AutoSubmit
	if autoSubmit == nil {
		autoSubmit = autoSubmitCOSPrompt
	}
	autoSubmitted, autoErr := autoSubmit(ctx, projectURL, prompt, projectName)
	if autoErr != nil {
		p.logf("[cos] Chrome auto-submit unavailable: %v\n", autoErr)
	}
	if !autoSubmitted {
		openProjectURL := p.OpenURL
		if openProjectURL == nil {
			openProjectURL = openURL
		}
		if err := openProjectURL(projectURL); err != nil {
			return "", false, fmt.Errorf("open ChatGPT Project: %w", err)
		}
		p.logf("[cos] Auto-submit failed. Send the opening prompt manually in ChatGPT; Rurushu will continue automatically.\n")
	} else {
		p.logf("[cos] Opening prompt submitted in Chrome.\n")
	}

	ticker := time.NewTicker(p.discoveryDelay())
	defer ticker.Stop()
	timeout := time.NewTimer(90 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-timeout.C:
			return "", false, errors.New("timed out waiting for a new ChatGPT session")
		case <-ticker.C:
			var current struct {
				ActiveID string       `json:"activeId"`
				Sessions []cosSession `json:"sessions"`
			}
			if p.getJSON(ctx, baseURL+"/v1/sessions?limit=10", token, &current) != nil {
				continue
			}
			for _, session := range current.Sessions {
				if !known[session.ID] && session.Events > 0 {
					return session.ID, true, nil
				}
			}
			if current.ActiveID != "" && current.ActiveID != initial.ActiveID && !known[current.ActiveID] {
				return current.ActiveID, true, nil
			}
		}
	}
}

func (p *COSProvider) boundSession(localID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bindings == nil {
		return ""
	}
	return p.bindings[localID]
}

func (p *COSProvider) bindSession(localID, remoteID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bindings == nil {
		p.bindings = map[string]string{}
	}
	p.bindings[localID] = remoteID
}

func (p *COSProvider) projectConfig() cosProjectConfig {
	home := p.HomeDir
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	merged := readCOSConfigWithLegacyFallback(
		filepath.Join(home, ".rurushu", "cos.json"),
		filepath.Join(home, ".rurushugo", "cos.json"),
	)
	local := readCOSConfigWithLegacyFallback(
		filepath.Join(p.CWD, ".rurushu", "cos.json"),
		filepath.Join(p.CWD, ".rurushugo", "cos.json"),
	)
	if local.Project != "" {
		merged.Project = local.Project
	}
	if local.ProjectURL != "" {
		merged.ProjectURL = local.ProjectURL
	}
	if value := strings.TrimSpace(os.Getenv("COS_PROJECT")); value != "" {
		merged.Project = value
	}
	if value := strings.TrimSpace(os.Getenv("COS_PROJECT_URL")); value != "" {
		merged.ProjectURL = value
	}
	return merged
}

func readCOSConfigWithLegacyFallback(path, legacyPath string) cosProjectConfig {
	if _, err := os.Stat(path); err == nil {
		return readCOSConfig(path)
	}
	return readCOSConfig(legacyPath)
}

func readCOSConfig(path string) cosProjectConfig {
	var cfg cosProjectConfig
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	cfg.Project = strings.TrimSpace(cfg.Project)
	cfg.ProjectURL = strings.TrimSpace(cfg.ProjectURL)
	return cfg
}

func (p *COSProvider) projectURL(cfg cosProjectConfig) (string, error) {
	if cfg.ProjectURL != "" {
		return cfg.ProjectURL, nil
	}
	if runtime.GOOS != "darwin" || cfg.Project == "" {
		return "", errors.New("COS projectUrl is required in .rurushu/cos.json or COS_PROJECT_URL")
	}
	script := fmt.Sprintf(`tell application "Google Chrome" to get URL of every tab of every window`)
	output, err := exec.Command("osascript", "-e", script).Output()
	if err == nil {
		for _, candidate := range strings.Split(string(output), ", ") {
			candidate = strings.TrimSpace(candidate)
			if strings.Contains(candidate, cfg.Project) && strings.Contains(candidate, "chatgpt.com/g/g-p-") {
				if index := strings.Index(candidate, "/project"); index >= 0 {
					return candidate[:index+8], nil
				}
			}
		}
	}
	return "", fmt.Errorf("unable to locate ChatGPT Project %q; set COS_PROJECT_URL", cfg.Project)
}

func (p *COSProvider) mapPath() string {
	home := p.HomeDir
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".rurushu", "cos-sessions-map.json")
}

func (p *COSProvider) storedSession(localID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.readMap()
	return m.Sessions[localID]
}

func (p *COSProvider) saveSession(localID, remoteID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.readMap()
	m.Sessions[localID] = remoteID
	m.ByCWD[p.CWD] = remoteID
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := p.mapPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cos-sessions-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (p *COSProvider) readMap() cosSessionMap {
	m := cosSessionMap{Sessions: map[string]string{}, ByCWD: map[string]string{}}
	data, err := os.ReadFile(p.mapPath())
	if err == nil {
		_ = json.Unmarshal(data, &m)
	}
	if m.Sessions == nil {
		m.Sessions = map[string]string{}
	}
	if m.ByCWD == nil {
		m.ByCWD = map[string]string{}
	}
	return m
}

func (p *COSProvider) eventCursor(ctx context.Context, baseURL, token, sessionID string) (int, error) {
	var payload struct {
		NextFrom int `json:"nextFrom"`
	}
	err := p.getJSON(ctx, baseURL+"/v1/sessions/"+url.PathEscape(sessionID)+"/events?limit=1", token, &payload)
	return payload.NextFrom, err
}

func (p *COSProvider) getJSON(ctx context.Context, endpoint, token string, target any) error {
	status, body, err := p.request(ctx, http.MethodGet, endpoint, token, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("COS API error (%d): %s", status, strings.TrimSpace(body))
	}
	return json.Unmarshal([]byte(body), target)
}

func (p *COSProvider) request(ctx context.Context, method, endpoint, token string, body any) (int, string, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), err
}

func (p *COSProvider) cancelInput(baseURL, token, inputID string) {
	if inputID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, _ = p.request(ctx, http.MethodPost, baseURL+"/v1/inputs/"+url.PathEscape(inputID)+"/cancel", token, nil)
}

func (p *COSProvider) logf(format string, args ...any) {
	if p.Stderr != nil {
		_, _ = fmt.Fprintf(p.Stderr, format, args...)
	}
}

func formatCOSPrompt(req CompletionRequest, cwd string) string {
	userCount := 0
	userText := ""
	compaction := ""
	for _, message := range req.Messages {
		switch message.Role {
		case RoleUser:
			userCount++
			userText = message.Content
		case RoleSystem:
			if strings.Contains(message.Content, "[Compaction Summary]") || strings.Contains(message.Content, "[Session Summary]") {
				compaction = message.Content
			}
		}
	}
	parts := []string{}
	if userCount <= 1 || compaction != "" {
		leaf, parent := filepath.Base(filepath.Clean(cwd)), filepath.Base(filepath.Dir(filepath.Clean(cwd)))
		label := leaf
		if parent != "." && parent != string(filepath.Separator) && parent != "" {
			label = parent + "/" + leaf
		}
		parts = append(parts, "[Project Workspace: "+label+"]", "")
	}
	if compaction != "" {
		parts = append(parts, compaction, "")
	}
	return strings.Join(append(parts, userText), "\n")
}

func correlateCOSTurn(events []cosEvent, inputID string, alreadySubmitted bool) string {
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i].Seq, events[j].Seq
		if events[i].Position != nil {
			a = *events[i].Position
		}
		if events[j].Position != nil {
			b = *events[j].Position
		}
		return a < b
	})
	ownsNext, openingSeen := false, false
	for _, event := range events {
		switch event.Kind {
		case "user_message":
			if alreadySubmitted {
				ownsNext = !openingSeen
				openingSeen = true
			} else {
				ownsNext = event.InputID == inputID
			}
		case "turn_start":
			if ownsNext && event.TurnID != "" {
				return event.TurnID
			}
			ownsNext = false
		case "turn_end":
			ownsNext = false
		}
	}
	return ""
}

func randomID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", time.Now().Unix(), os.Getpid()&0xffff, 0x4000, 0x8000, time.Now().UnixNano()&0xffffffffffff)
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		data[0:4],
		data[4:6],
		data[6:8],
		data[8:10],
		data[10:16],
	)
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func openURL(value string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", "-a", "Google Chrome", value)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", value)
	default:
		command = exec.Command("xdg-open", value)
	}
	return command.Start()
}

func autoSubmitCOSPrompt(ctx context.Context, projectURL, prompt, projectName string) (bool, error) {
	if runtime.GOOS != "darwin" {
		return false, nil
	}
	if strings.TrimSpace(projectURL) == "" || strings.TrimSpace(prompt) == "" {
		return false, nil
	}
	commandCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, "osascript", "-l", "JavaScript", "-e", cosChromeAutoSubmitScript,
		projectURL, prompt, projectName).CombinedOutput()
	if err != nil {
		if commandCtx.Err() != nil {
			return false, fmt.Errorf("Chrome automation timed out: %w", commandCtx.Err())
		}
		return false, fmt.Errorf("Chrome automation failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var result cosChromeResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &result); err != nil {
		return false, fmt.Errorf("decode Chrome automation result: %w", err)
	}
	if result.OK && result.Submitted {
		return true, nil
	}
	detail := result.Reason
	if result.Error != "" {
		if detail != "" {
			detail += ": "
		}
		detail += result.Error
	}
	if detail == "" {
		detail = "unknown error"
	}
	return false, errors.New(detail)
}

const cosChromeAutoSubmitScript = `
function run(argv) {
  const projectUrl = argv[0];
  const promptText = argv[1];
  const projectName = argv[2];

  let chrome;
  try {
    chrome = Application("Google Chrome");
    if (!chrome.running()) return JSON.stringify({ ok: false, reason: "chrome_not_running" });
  } catch (e) {
    return JSON.stringify({ ok: false, reason: "chrome_unavailable", error: String(e) });
  }
  if (!chrome.windows.length) return JSON.stringify({ ok: false, reason: "no_chrome_window" });

  const win = chrome.windows[0];
  const originalTabId = String(win.activeTab().id());
  let targetTab;
  let targetTabId = null;

  function restoreOriginalTab() {
    try {
      for (let i = 0; i < win.tabs.length; i++) {
        if (String(win.tabs[i].id()) === originalTabId) {
          win.activeTabIndex = i + 1;
          return;
        }
      }
    } catch (_) {}
  }

  try {
    win.tabs.push(chrome.Tab({ url: projectUrl }));
    targetTab = win.tabs[win.tabs.length - 1];
    targetTabId = String(targetTab.id());
    win.activeTabIndex = win.tabs.length;

    let composerReady = false;
    let lastState = null;
    const loadDeadline = Date.now() + 12000;
    while (Date.now() < loadDeadline) {
      delay(0.25);
      targetTab = win.activeTab();
      try {
        lastState = targetTab.execute({ javascript: "(() => { const box = document.querySelector('#prompt-textarea, [contenteditable=\\\"true\\\"][role=\\\"textbox\\\"]'); return { href: location.href, ready: !!box, disabled: box ? (box.getAttribute('aria-disabled') === 'true' || box.getAttribute('contenteditable') === 'false') : false }; })()" });
      } catch (_) {
        lastState = null;
      }
      if (lastState && lastState.ready && !lastState.disabled) {
        composerReady = true;
        break;
      }
    }
    if (!composerReady) {
      restoreOriginalTab();
      return JSON.stringify({ ok: false, reason: "composer_not_ready", targetTabId: targetTabId, lastState: lastState });
    }

    const insertCode = "(() => {" +
      "const box=document.querySelector('#prompt-textarea, [contenteditable=\\\"true\\\"][role=\\\"textbox\\\"]');" +
      "if(!box)return {ok:false,reason:'no_box'};" +
      "box.focus();" +
      "const sel=document.getSelection(); if(!sel)return {ok:false,reason:'no_selection'};" +
      "const clearRange=document.createRange(); clearRange.selectNodeContents(box); sel.removeAllRanges(); sel.addRange(clearRange); document.execCommand('delete',false,null);" +
      "const p=box.querySelector('p')||box;" +
      "const range=document.createRange(); range.setStart(p,0); range.collapse(true); sel.removeAllRanges(); sel.addRange(range);" +
      "const value=" + JSON.stringify(promptText) + ";" +
      "const inserted=document.execCommand('insertText',false,value);" +
      "return {ok:!!inserted,text:box.innerText||'',html:box.innerHTML};" +
      "})()";
    const insertState = targetTab.execute({ javascript: insertCode });
    if (!insertState || !insertState.ok || !(insertState.text || "").trim()) {
      restoreOriginalTab();
      return JSON.stringify({ ok: false, reason: "insert_failed", targetTabId: targetTabId, insertState: insertState });
    }

    let submitted = false;
    let sendState = null;
    const sendDeadline = Date.now() + 5000;
    while (Date.now() < sendDeadline) {
      delay(0.2);
      try {
        sendState = targetTab.execute({ javascript: "(() => { const btn=document.querySelector('button[aria-label=\\\"Send\\\"], button[data-testid=\\\"send-button\\\"]'); if(!btn)return {ok:false,reason:'no_send_button'}; if(btn.disabled||btn.getAttribute('aria-disabled')==='true')return {ok:false,reason:'send_disabled'}; btn.click(); return {ok:true}; })()" });
      } catch (_) {
        sendState = null;
      }
      if (sendState && sendState.ok) {
        submitted = true;
        break;
      }
    }
    if (!submitted) {
      restoreOriginalTab();
      return JSON.stringify({ ok: false, reason: "send_failed", targetTabId: targetTabId, sendState: sendState });
    }

    let finalHref = targetTab.url();
    const navDeadline = Date.now() + 5000;
    while (Date.now() < navDeadline) {
      delay(0.2);
      finalHref = targetTab.url();
      if (String(finalHref).includes("/c/")) break;
    }
    restoreOriginalTab();
    return JSON.stringify({ ok: true, submitted: true, targetTabId: targetTabId, href: finalHref, projectName: projectName });
  } catch (e) {
    restoreOriginalTab();
    return JSON.stringify({ ok: false, reason: "automation_error", targetTabId: targetTabId, error: String(e) });
  }
}
`
