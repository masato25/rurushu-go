# rurushu-go

`rurushu-go` is a lightweight LLM harness and terminal client for Go. The reusable runtime stays independent of GEASS domain logic; the repository also ships an optional Bubble Tea TUI and an OpenAI-compatible HTTP adapter so it can run by itself.

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
  "version": 1,
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

TUI controls: `Enter` sends, `Shift+Enter` inserts a newline, `Esc` cancels the active model request, `PgUp/PgDn` scroll, and `Ctrl+C` exits. Permission prompts are approved with `y` and denied with `n`, `Enter`, or `Esc`.

Local slash commands are handled by the TUI before a prompt reaches the model. The standalone client currently includes:

```text
/help   show available local commands
/clear  clear the transcript and model conversation history
/exit   exit the TUI
/jobs [id]  list managed jobs or inspect one job's recent output
/stop <id>  stop a Rurushu-managed background job
```

Unknown slash commands stay local and show an error. Prefix a prompt with `//` when you really want to send leading-slash text to the model; for example, `//help` sends `/help` as a normal prompt.

Slash commands autocomplete as you type. Enter `/` or a prefix such as `/he`, use `↑/↓` to choose a match, then press `Tab` or `Enter` to complete it. Press `Enter` again when the command is complete to run it. Commands registered by embedding applications automatically appear in the same autocomplete list.

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
