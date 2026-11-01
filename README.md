<p align="center">
  <img src="./assets/rurushu-go-cover.webp" alt="RURUSHU GO" width="100%" />
</p>

<h1 align="center">RURUSHU GO</h1>

<p align="center">
  <strong>A lightweight terminal coding agent and reusable LLM harness, built in Go.</strong>
</p>

<p align="center">
  <img alt="Go 1.26" src="https://img.shields.io/badge/Go-1.26-7B5CFF?style=flat-square&logo=go&logoColor=white" />
  <img alt="OpenAI compatible" src="https://img.shields.io/badge/API-OpenAI--compatible-7B5CFF?style=flat-square" />
  <img alt="Bubble Tea TUI" src="https://img.shields.io/badge/TUI-Bubble%20Tea-7B5CFF?style=flat-square" />
  <img alt="Vision capable" src="https://img.shields.io/badge/VLM-Vision%20capable-7B5CFF?style=flat-square" />
</p>

`rurushu-go` is a lightweight LLM harness and terminal client for Go. The reusable runtime stays independent of GEASS domain logic; the repository also ships an optional Bubble Tea TUI and an OpenAI-compatible HTTP adapter so it can run by itself.

## Install

Install the latest release with Go:

```bash
go install github.com/masato25/rurushu-go/cmd/rurushu@latest
```

Then run:

```bash
rurushu setup
rurushu
```

## Run it standalone

The recommended first run is the interactive setup wizard:

```bash
go run ./cmd/rurushu setup
```

It stores the OpenAI-compatible base URL, API key, model, system prompt, and harness budgets in `~/.config/rurushu/config.json` with file mode `0600`. After that, start Rurushu with:

```bash
go run ./cmd/rurushu
```

CLI flags override environment variables, which override the saved config.

With OpenAI:

```bash
export OPENAI_API_KEY=...
go run ./cmd/rurushu --model <model-name>
```

With an OpenAI-compatible gateway:

```bash
go run ./cmd/rurushu \
  --base-url http://localhost:8081/v1 \
  --model <model-name>
```

You can also use `RURUSHU_API_KEY`, `RURUSHU_BASE_URL`, and `RURUSHU_MODEL`. Rurushu accepts a positional draft so the TUI opens with text already in the composer:

```bash
go run ./cmd/rurushu --model <model-name> "summarize what I should work on"
```

### Chat On Steroids

### Interactive terminal workspace (F4)

### Project file browser (F5)

Press **F5** in the regular chat TUI to open a read-only project file browser. The right pane navigates folders and files; the left pane previews UTF-8 text, code, and rendered Markdown (`.md`/`.markdown`). Press **Tab** to switch panes, **↑/↓** to select files or scroll preview, **Enter** to enter a folder, **Backspace** for its parent, **r** to refresh, and **F5/Esc** to return to chat. Symlinks, binary files, and files larger than 512 KiB are not previewed.

The preview includes line numbers. For text and source code these correspond to file lines; for rendered Markdown they number the displayed rows rather than the original Markdown source lines.

To select and copy in F5, press **Enter** on a file to focus the preview. Move with **h/j/k/l** or arrow keys, press **v** for Vim-style character selection or **Ctrl+V** for rectangular block selection, then **y** to copy to the system clipboard. **Esc** cancels selection and **Tab** returns to the file list. Markdown selections copy the *rendered text*, not the source Markdown. F5 is read-only.


From the regular chat TUI press **F4** to open an embedded PTY shell on the left with a session panel on the right. Press **Enter** to focus the shell, **Ctrl+]** to release keyboard focus, and **F4** or **Esc** while unfocused to return to chat. **Ctrl+C** while focused goes to the subprocess, not to Rurushu. Terminal output and scrolling remain in the TUI alternate screen.

There is **one shared interactive shell** for this Rurushu process. F4 hides or reopens the same PTY without stopping running programs. The PTY closes when Rurushu itself exits; it is not a persistent daemon. Existing managed background jobs continue to use the separate job manager.

The agent may call the permission-gated `terminal_send` tool to submit one command to this shell even while F4 is hidden. Opening F4 shows the shell's output. Submission does **not** establish completion, exit status, or that an already-busy interactive program accepted the command. Commands with embedded newlines or control characters are rejected. For completion-aware foreground execution, use the existing `bash` tool instead.


Initialize a per-project COS configuration template in the current directory (existing files are never overwritten):

```bash
./bin/rurushu --init
# Or create the config in another workspace:
./bin/rurushu --init --cwd /path/to/project
```

Edit `.rurushu/cos.json` and fill in `project` and `projectUrl` before starting the COS provider.

Rurushu can connect directly to the local Chat On Steroids Control API (no OpenAI API key required). Enable `controlApi` in Chat On Steroids, start the app, and configure the ChatGPT Project URL:

```json
// .rurushu/cos.json (project) or ~/.rurushu/cos.json (global)
{
  "project": "my-project",
  "projectUrl": "https://chatgpt.com/g/g-p-.../project"
}
```

Then run:

```bash
go run ./cmd/rurushu --provider cos
# equivalent environment setting:
RURUSHU_PROVIDER=cos go run ./cmd/rurushu
```

To continue the last session in the current project (including its conversation history), use `-c`:

```bash
go run ./cmd/rurushu --provider cos -c
# equivalent to: --resume last
```

If there is no previous session, `-c` reports an error rather than creating a new conversation. Use `/sessions` in the TUI to select another session.

`COS_PROJECT` and `COS_PROJECT_URL` override the config files. Each Rurushu session is mapped one-to-one to a COS session in `~/.rurushu/cos-sessions-map.json`; resumed Rurushu sessions reuse their matching ChatGPT conversation. Set `COS_NEW=1` to force creation of a new mapping, or `COS_SESSION_ID` to bind an explicit COS session.

To inspect models exposed by the endpoint:

```bash
go run ./cmd/rurushu --base-url http://localhost:8081/v1 --list-models
```

Build a local binary with:

```bash
go build -o bin/rurushu ./cmd/rurushu
./bin/rurushu --model <model-name>
```

For workflow orchestrators and other non-interactive callers, Rurushu also
provides a versioned JSON execution boundary. The caller and Rurushu remain
separate processes and do not need to share Go packages:

```bash
printf '%s\n' '{
  "version": 2,
  "task": "inspect this workspace and summarize the requested change",
  "cwd": "/path/to/workspace",
  "model": "model-name",
  "tool_profile": "readonly",
  "permission_mode": "deny"
}' | rurushu run
```

When JSON is piped to `rurushu run` (or `--input`/`--output` is used), the
command writes exactly one JSON response to stdout/output. Plain positional
text such as `rurushu run tests` remains a TUI initial prompt for compatibility.
`tool_profile` is
`none`, `readonly` (default), or `execution`; execution tools still require an
explicit `permission_mode: "allow"`. Provider credentials and endpoint remain
Rurushu configuration concerns (`RURUSHU_*`, `OPENAI_*`, or its saved config).

Protocol v2 also accepts an optional `external_tools` array. Each entry supplies a
function `id`, description, JSON `input_schema`, subprocess `command`/`args`, optional
environment, a `read_only` flag, and optional `preauthorized`. Rurushu forwards the
model's JSON arguments to the subprocess on stdin and returns bounded stdout/stderr as
the tool result. Read-only tools skip interactive permission. Mutable tools normally
pass through the configured permission handler; `preauthorized` is for orchestrators
whose subprocess enforces its own narrower policy (for example workspace confinement
or a command allowlist). Version 1 requests remain accepted but cannot include
`external_tools`.

TUI controls: `Enter` sends, `Shift+Enter` inserts a newline, `Esc` cancels the active model request, `PgUp/PgDn` scroll, and `Ctrl+C` exits. Permission prompts are approved with `y` and denied with `n`, `Enter`, or `Esc`.

For trusted tasks only, start the TUI with `rurushu --auto-approve` to approve all tool permission requests without prompting, including shell execution and job termination. This is disabled by default and allows potentially destructive commands to run without confirmation. To persist it, set `"auto_approve": true` in `~/.config/rurushu/config.json`; override that setting for a session with `--auto-approve=false`. Headless runs keep their separate explicit `permission_mode` policy and do not inherit this setting.

Local slash commands are handled by the TUI before a prompt reaches the model. The standalone client currently includes:

```text
/help   show available local commands
/clear  clear the transcript and model conversation history
/exit   exit the TUI
/attach <image-path>  attach an image to the next model message
/attachments  show images queued for the next model message
/detach [index|all]  remove a queued image attachment
/jobs [id]  list managed jobs or inspect one job's recent output
/stop <id>  stop a Rurushu-managed background job
/watch [<interval> <instruction>|stop]  run a recurring instruction in this single chat session
/reload  rebuild Rurushu and restart into exactly the current session
```

When developing Rurushu from its source checkout, type `/reload` in an idle single-chat TUI to save the current session, exit the UI, run `build.sh --skip-tests`, and relaunch the rebuilt binary with `--resume <current-session-id>`. The selected provider and command-line settings are preserved; the session does not silently switch to the most recently used session. A failed build leaves the session saved and prints a manual resume command instead of launching a broken binary. Run tests separately before production use; reload deliberately skips tests for speed.

On Unix systems the successful reload replaces the running Rurushu process in place, keeping its PID instead of nesting extra parent processes. Existing nested processes from older releases are not automatically terminated; close that legacy terminal chain once before validating subsequent reloads.

Performance diagnostics: start the interactive client with `--pprof 127.0.0.1:6060` to enable optional Go runtime CPU/heap/goroutine profiles on loopback only. For example, inspect `http://127.0.0.1:6060/debug/pprof/` locally or use `go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=15`. Do not expose profiling to other machines; profiles may contain sensitive runtime data. The UI throttles token-driven transcript redraws, bounds the Markdown cache and resets it when restoring a session. Watch scheduling checks the file timestamp/size before parsing. Session snapshot writes encode JSON once; append-only history is documented below.

Session history now uses an immutable `.rurushu/sessions/<session>.json` snapshot and append-only `<session>.jsonl` delta records for new messages. Older single-file JSON sessions remain readable. An incomplete final journal line is ignored after an interrupted write, while edits to earlier messages automatically create a fresh JSON snapshot; stale records from the previous snapshot are ignored. Session loading still reconstructs full history, and the journal has a 16 MiB size limit; this is an incremental write optimization, not unlimited history retention.

The store now retains a bounded, single-session in-memory save cache and validates snapshot/journal file timestamps before using it. A journal reaching 1 MiB is folded into an atomic snapshot and removed after replacement. Session saves use a per-session advisory file lock across processes to serialize writes. Interactive sessions also acquire a nonblocking ownership lease: another Rurushu process attempting to open the same session receives an error instead of editing it concurrently. The lease follows `/new`, `/resume`, and `/rewind` session changes and is released on process exit (including `/reload` process replacement). Other non-interactive writers do not yet enforce this lease; total-history capacity is still bounded.

Use `rurushu -provider cos -kc` to force-continue the most recent session despite an active ownership lease. This intentionally allows multiple interactive processes on the same session and can overwrite history if both save divergent conversations. Prefer normal `-c` unless deliberately recovering or overriding a session; `-kc` retains per-save file locking but does not merge concurrent edits.

For long sessions, the TUI renders only the most recent 120 transcript entries by default. When scrolled to the top, press PageUp to reveal 120 more entries; all history remains stored in the original session. COS additionally requests a native Compact & Resume handoff at every 100th user turn when an existing remote session mapping is available. Handoff is requested, not guaranteed complete immediately; the COS provider waits for ongoing handoffs before submitting subsequent input. Failed compact requests do not block a normal follow-up or create a new remote session. Local conversation history is retained for display and recovery.

For long-running monitor/maintenance work, `/watch` keeps the normal single-chat workflow and periodically sends the configured instruction back into the same session. For example:

```text
/watch 10m inspect the current monitor and process health; summarize changes since the previous check; investigate anomalies; apply safe fixes or restarts when justified; otherwise report that no action is needed
```

Use `/watch` with no arguments to inspect the schedule and `/watch stop` to disable it. The schedule is stored per session under `.rurushu/` and is restored when that session is resumed. A tick is deliberately skipped when a model/tool run is still active, a permission prompt is waiting, another TUI workspace is open, or the composer contains an unsent draft; recurring work never overwrites user input or accumulates a backlog. The TUI process must remain running for timers to fire. If scheduled work needs permission-gated execution without a person present, the existing `--auto-approve` mode can be used for a trusted workspace; otherwise Rurushu pauses at the normal permission prompt.

The interactive harness also exposes `watch_create`, `watch_status`, and `watch_stop` tools to models supporting tool calling. Therefore a natural-language request such as "Schedule a check of my system every 10 minutes; diagnose anomalies and repair issues when safe" can create the same recurring watch without memorizing `/watch` syntax. The result is applied to the currently active session by the TUI, usually within two seconds. Successful tool execution confirms schedule creation, not successful future checks or repairs. Commands that alter the system continue to use normal permissions, and an open TUI is still required.

To manage the current session's schedule visually, open **F2 Setup**, then press **Ctrl+W** to switch to **Scheduled tasks**. The panel shows whether the watch is running, its next and last run, interval, and instruction. Edit the interval and instruction, then press **Enter** on the instruction field to save and activate the schedule. Press **Ctrl+D** in this panel to stop it immediately. **Ctrl+W** returns to normal settings, and **Esc/F2** closes Setup. These changes persist to the same session watch file and are independent of the regular model configuration Save button.

Watch execution tracking records `queued`, `running`, `completed`, `failed`, `skipped`, and `interrupted` in the per-session schedule file. F2 shows the latest result, last completion time, skipped tick count, and any execution error. `last_run_at` indicates when a run was queued, not a successful completion. A watch still in progress at process startup is marked interrupted. A `completed` status means the model stream finished normally, not that the monitored system is healthy or a repair succeeded; validate those claims from tool output and logs.

Unknown slash commands stay local and show an error. Prefix a prompt with `//` when you really want to send leading-slash text to the model; for example, `//help` sends `/help` as a normal prompt.

Slash commands autocomplete as you type. Enter `/` or a prefix such as `/he`, use `↑/↓` to choose a match, then press `Tab` or `Enter` to complete it. Press `Enter` again when the command is complete to run it. Commands registered by embedding applications automatically appear in the same autocomplete list.

Vision-capable OpenAI-compatible models can receive local images through `/attach`. Up to four images may be queued for the next message. Rurushu keeps small JPEG/PNG files unchanged, but automatically downsizes images whose longest edge exceeds 1568 pixels and re-encodes oversized payloads to stay near 768 KiB per image. This bounds transfer/session growth and reduces image-token usage on providers whose vision cost scales with image dimensions. GIF/WebP input is normalized to a broadly compatible static JPEG/PNG payload. To avoid paying for the same vision inputs on every later turn, API requests carry at most the four most recent historical images; the full optimized image history remains in the local session for resume/rewind.

Tool activity is persistent in the transcript. `--activity normal` shows compact tool progress (default), `--activity verbose` adds compact args/result details, and `--activity debug` includes full tool results plus usage/context-compaction events. Raw model chain-of-thought is never rendered; reasoning streams only produce a generic `thinking` progress marker.

The standalone binary includes read-only repository tools (`read`, `glob`, `grep`) plus permission-gated shell execution (`bash`). Foreground shell commands have bounded output and a timeout. Long-running servers/watchers should use `background=true`; they become managed jobs that survive `/exit` and can be inspected with `job_list`, `job_output`, `/jobs`, and stopped with `job_stop` or `/stop`.

## Project state and managed jobs

Starting Rurushu in a project creates private local state under `.rurushu/`:

```text
.rurushu/
├── state.json
├── sessions/
│   └── ses_....json
└── jobs/
    ├── job_000001.json
    └── job_000001.log
```

Directories use mode `0700` and metadata/log files use `0600`. `.rurushu/.gitignore` ignores the entire state directory without modifying the repository's root `.gitignore`, and the built-in repository discovery tools skip `.rurushu`.

Each TUI launch records lightweight session metadata (session ID, model, provider, timestamps). Managed jobs record the command, project CWD, runner PID, process identity, lifecycle status, and a relative log path; environment variables are not serialized. Job logs are rolling files capped at approximately 2 MiB, while `/jobs <id>` and `job_output` read only a bounded recent tail.

On startup Rurushu reconciles jobs left by previous sessions. A job is considered live only when its recorded PID still matches the recorded process identity; a reused or missing PID becomes `stale` rather than being treated as a managed process. The status bar shows the current number of running managed jobs.

## Packages

- `provider`: model/message/stream contracts, streaming tool-call reconstruction, and an OpenAI-compatible HTTP provider.
- `tool`: typed Go tool execution plus a thread-safe registry and function schemas.
- `permission`: approval contracts plus an event-driven TUI permission bridge.
- `projectstate`: project-local `.rurushu` state and session metadata.
- `jobs`: persistent managed-job metadata, detached runner lifecycle, process reconciliation, stopping, and bounded logs.
- `harness`: prompt/context composition, provider → tool → provider control loop, cancellation, step limits, context compaction, result aggregation, and optional result validators.
- `tui`: reusable Bubble Tea conversation UI for any `Streamer` compatible with the harness.
- `cmd/rurushu`: standalone terminal entrypoint.

## Library use

```go
registry := tool.NewRegistry()
registry.Register(myTool)

runner, err := harness.New(myProvider, harness.Config{
    Tools:          registry,
    ToolMaxSteps:   8,
    Permission:     permission.AllowAll{},
    SystemPrompt:   "You are a focused service agent.",
})
if err != nil {
    return err
}

result, err := runner.Run(ctx, provider.CompletionRequest{
    Model: "my-model",
    Messages: []provider.Message{{
        Role:    provider.RoleUser,
        Content: "Inspect this job and return a decision.",
    }},
})
```

`provider.Provider` only requires streaming. Model listing is an optional capability. `harness.ContextSource` is generic, so applications can inject memory, retrieval, project state, or no extra context at all.

Embedding applications can add lightweight TUI-only slash commands without changing the harness or provider layers:

```go
uiModel := tui.NewWithStreamer(modelName, providerID, runner)
err := uiModel.RegisterSlashCommand(tui.SlashCommand{
    Name:        "project",
    Usage:       "/project <name>",
    Description: "switch the local project context",
    Run: func(args string) (tui.SlashCommandResult, error) {
        return tui.SlashCommandResult{Output: "selected " + args}, nil
    },
})
```

Slash command input and output are local UI state and are not appended to model conversation history.

## Design boundary

The harness does not own queues, worker scheduling, business-job retries, checkpoints, durable memory, agent planning modes, or domain workflows. Those stay in the embedding application. The optional TUI and OpenAI-compatible adapter sit on top of the same library contracts rather than being required by the harness core.

### LEFT-led duel (左右互搏)

The duel is LEFT-led: LEFT works first like a normal single-chat implementation/research session, then RIGHT receives LEFT's final response and returns a lightweight review/coaching reply of at most 100 Unicode characters. That reply is fed back to LEFT on the next round. RIGHT may begin its reply with `DONE:` when the work is ready for owner review. Ctrl+C interrupts safely without recording success. The current duel IDs, provider/model assignments, round count and latest summary are saved at `.rurushu/duel-state.json`.

```bash
go run ./cmd/rurushu duel --goal "Improve the project until it is ready for acceptance"
# Resume the same two sessions after an interruption:
go run ./cmd/rurushu duel --resume

# Existing duel state is reused by default; this does not create new LEFT/RIGHT sessions.
go run ./cmd/rurushu duel --goal "continue the same task"

# Start a fresh LEFT/RIGHT pair only when you intentionally want a new duel.
go run ./cmd/rurushu duel --new --goal "start a different task"

# LEFT and RIGHT may use different providers/models.
go run ./cmd/rurushu duel --new \
  --goal "continue development" \
  --left-provider cos --left-model chatgpt-web \
  --right-provider ollama \
  --right-model "hf.co/unsloth/Qwen3.8-27B-GGUF:UD-IQ2_M" \
  --ollama-url http://192.168.2.109:11434

# Convenience launcher for the same COS LEFT + 27B Ollama RIGHT setup:
# Continue the last Ollama RIGHT duel for the current project
./scripts/duel-ollama-right.sh --continue

# Or start a fresh duel with a new goal
./scripts/duel-ollama-right.sh --new --goal "continue development"
```

The Ollama launcher requires an explicit `--new`, `--continue`, or `--resume`. `--continue` is an alias for `--resume` and continues the current project's last duel. Continue/resume is accepted only when that project's persisted state already records `right_provider=ollama`.

The command opens a split-screen TUI: LEFT's work reports appear on the left, RIGHT's short feedback on the right, and an owner input area at the bottom. Enter submits additional instructions while the duel keeps working; Shift+Enter adds a line. Instructions are applied at the next role boundary, not injected into an in-flight turn. Ctrl+C exits and preserves the latest completed-round summary.

LEFT and RIGHT may use different providers. `cos`, `openai-compatible`, and `ollama` are supported by duel. Ollama uses its native `/api/chat` endpoint with thinking disabled so RIGHT's small completion budget is spent on the final feedback instead of hidden reasoning. Provider errors are surfaced instead of being interpreted as approval. This mode may make repeated model requests; use Ctrl+C to stop it manually.
