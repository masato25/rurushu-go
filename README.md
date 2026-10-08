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

TUI controls: `Enter` sends, `Shift+Enter` inserts a newline, `Esc` cancels the active model request, `PgUp/PgDn` scroll, and `Ctrl+C` exits. Permission prompts are approved with `y` and denied with `n`, `Enter`, or `Esc`.

The standalone binary currently ships without built-in filesystem or shell tools. The harness supports tools, but applications must register the capabilities they actually want to expose.

## Packages

- `provider`: model/message/stream contracts, streaming tool-call reconstruction, and an OpenAI-compatible HTTP provider.
- `tool`: typed Go tool execution plus a thread-safe registry and function schemas.
- `permission`: approval contracts plus an event-driven TUI permission bridge.
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

## Design boundary

The harness does not own queues, worker scheduling, business-job retries, checkpoints, durable memory, agent planning modes, or domain workflows. Those stay in the embedding application. The optional TUI and OpenAI-compatible adapter sit on top of the same library contracts rather than being required by the harness core.
