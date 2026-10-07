# rurushu-go

`rurushu-go` is a small, provider-agnostic LLM harness for Go services. It was extracted from the reusable model/tool execution core in GEASS-TI and intentionally does not depend on GEASS runtime, research, NATS, UI, or persistence packages.

The module owns four concerns:

- `provider`: model/message/stream contracts and streaming tool-call reconstruction.
- `tool`: typed Go tool execution plus a thread-safe registry and function schemas.
- `permission`: a minimal approval contract with allow-all and deny-all defaults.
- `harness`: prompt/context composition, provider → tool → provider control loop, cancellation, step limits, context compaction, result aggregation, and optional result validators.

It has no third-party dependencies.

## Minimal use

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

Implement `provider.Provider` for the model backend you want to use. Provider-specific HTTP clients deliberately live outside the core so the harness can be embedded without pulling in an SDK or a particular vendor.

`harness.ContextSource` is similarly generic: applications can inject memory, retrieval, project state, or no extra context at all. Tools receive a `tool.ExecutionContext` with the working directory, metadata, and an optional permission callback.

## Design boundary

The harness does **not** own queues, workers, retries for business jobs, checkpoints, durable memory, UI, agent planning modes, or domain workflows. Those belong to the embedding application. The harness is only responsible for one model interaction lifecycle and the tool calls that occur inside it.
