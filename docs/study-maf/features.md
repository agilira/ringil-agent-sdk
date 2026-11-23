# MAF Go vs ringil-agent-sdk — capability table

MAF root: `agent-framework-go/` (study copy). Ours root: `~/Desktop/GitHub/ringil-agent-sdk/`.
Verdicts are from reading package docs/signatures, not from running either SDK.

| Capability | MAF (package — one line) | Ours (package) | Verdict |
|---|---|---|---|
| Agent loop | `agent/agent.go` + `agent/harness/toolautocall/autocall.go` — tool loop lives in provider middleware (default 40 iterations, consecutive-error limit 3, optional concurrent calls) | root `agent.go`, `loop.go` — single loop, MaxTurns, parallel read-only/`Parallel()` tools, loop detector (`Config.Loop`), empty-reply retry | ours equal/better (loop detector, crash-safe per-message persistence, empty-reply retry) |
| Streaming | `agent/response.go` `ResponseStream` (iter.Seq2 of updates), `agent.Stream(bool)` | root `Stream` iterator of typed events (`event.go`) | equal |
| Function tools | `tool/functool/func.go`, `tool.SchemaTool` declaration-only tools returned to app | root `tool_func.go` `agent.NewTool` — schema from struct tags, strict arg validation, error filtering, panic recovery | ours better (strict decode, redacted errors); theirs adds declaration-only/client-executed tools |
| MCP client | `tool/mcptool/mcp.go` + `tasks.go` (MCP long-running tasks, input requests) | `mcp/` (stdio in sandbox, Streamable HTTP, OAuth login, timeouts, caps, annotation distrust), `mcp/composio` | ours better on security/OAuth; theirs better: MCP **tasks** lifecycle (`ListToolsWithTasks`) |
| Agent exposed as MCP server | `mcptool.AddTool` + `examples/02-agents/agents/step10_as_mcp_tool` | not found (`mcp/server.go` only describes servers to connect to) | missing in ours |
| Hosted tools | `tool/hostedtool/hostedtool.go` — WebSearch, FileSearch, CodeInterpreter markers | none | missing in ours (only meaningful for OpenAI/Foundry hosted APIs) |
| Shell tool | `tool/shelltool/` — persistent shell session, regex allow/deny Policy, approval; isolation left to Docker | `tools/bash` (jobs, danger classifier, audit) run via `sandbox/` (bwrap+seccomp on Linux, macOS profile) + `egress/` proxy | ours better (real sandbox + egress allowlist) |
| File tools | not found as a tool package (only `harness/filememory`) | `tools/files` — multi-dir workspace, no escape, secret exclusion, protected paths | missing in theirs |
| Agent-as-tool | `tool/agenttool/agenttool.go` | `delegate/` — workers with own tools, clean context, parallel, background + `continue_task`, scratchpad | ours better |
| Middleware / hooks | `agent/middleware.go` — full run-wrapping middleware + function-invocation middleware (`Terminate`), `ContextProvider` | `Config.Approve`, `AfterTool`, `Context`, `Observers` (`agent.go`) | theirs better: composable middleware chain over the whole run/model call |
| Tool approval | `agent/harness/toolapproval` — standing approvals, auto-approval rules, approval persisted in session across runs | `Config.Approve` (run/modify/refuse/stop), `serve` approval over API, MCP/A2A forced destructive | theirs better: approval request/response as first-class messages that survive a session restore; ours better on defaults |
| Message injection mid-run | `agent/messageinjection.go` `MessageInjector` | `later/` (scheduled re-check turns), `delegate.Notice` | theirs better: queue messages into a running loop between model calls |
| Sessions / history | `agent/session.go` (JSON-serializable state bag), `agent/history.go` `HistoryProvider`, Cosmos DB history (`provider/azureprovider/cosmosdb_history.go`) | `session/file` — append-only JSONL with hash chain, crash-safe resume, one-writer lock | ours better on integrity; theirs better: pluggable history provider + per-provider state bag |
| Continuation / background responses | `agent/continuation.go` `WithContinuationToken`, OpenAI Responses background | resumable SSE in `serve`; no provider-side background | theirs has it (provider-level); ours resumable at the serve layer |
| Compaction | `agent/compaction/` — pipeline of strategies (sliding window, truncation, summarization, tool-result eviction, context-window) + triggers | root `compact.go`, `compact_summary.go` — threshold, keep-N, model summary, `read_history` to fetch originals | theirs better: composable strategy pipeline; ours better: originals recoverable via `read_history` |
| Memory | `agent/memory/provider.go` (vector-store chat memory, scoped app/agent/user/session), `harness/filememory`, Foundry memory | `memory/` local BM25, sqlite FTS5+vectors (RRF), chroma, mem0; `memory.Tools` remember/recall/forget; `notes/` | ours equal/better (more backends, hybrid search, no cloud lock-in) |
| RAG / vector stores | `agent/textsearch` (pre-run or on-demand search, citations), `vectordata/` generic typed collections, filters, hybrid search, in-memory + Azure AI Search | only via memory backends (`memory/sqlite`, `memory/chroma`) | theirs better: generic vector-store abstraction + document RAG with citations |
| Embeddings | `embedding/` generic generator, OTel instrumented | root `embed.go` `Embedder`, `provider/openai/embed.go`, `provider/ollama` | equal |
| Structured output | `agent/structuredoutput.go` `WithStructuredOutput(v)`, `WithResponseFormat`, `openaiprovider/strict_schema.go` | not found (`Request` has no response format) | missing in ours |
| Multimodal | images, audio, files content (`message/datacontent.go`) | images (`KeepImages`), speech/transcribe (`provider/openai/speech.go`, `transcribe.go`) | equal-ish |
| Sequential orchestration | `workflow/agentworkflow/sequential.go` | `workflow/` plan with `needs` chain | equal |
| Concurrent | `agentworkflow/concurrent.go` (custom aggregator, map-reduce) | `workflow/` ready steps run together; parallel `delegate` | equal (theirs has explicit aggregator) |
| Handoff | not implemented in Go (README) | not found | missing in both |
| Group chat | `agentworkflow/groupchat.go` (manager, round-robin) | not found | missing in ours |
| Graph workflows | `workflow/` typed executors, edges, conditional/switch/multi-select, loops, subworkflows, shared state | `workflow/` DAG of delegate steps, acyclic, no conditions | theirs better: real graph engine (conditions, loops, subworkflows, typed messages) |
| Checkpoint / time travel | `workflow/checkpoint` (in-memory + JSON store, parent index), `inproc.Run.RestoreCheckpoint` | `workflow.NewFileJournal` — resume skips done steps, uncertain step blocks (`WORKFLOW_UNCERTAIN`), `Resolve` | theirs better: checkpoint per superstep + restore to any; ours better: never re-runs side effects silently |
| Human-in-the-loop in workflows | `workflow/requestport.go` external request/response ports | plan approval before `run_workflow`; `Resolve` for uncertain steps | theirs better: typed request ports mid-graph |
| Workflow as agent | `examples/03-workflows/agents/workflow_as_an_agent` | `Runner.Tool()` (workflow as tool) | equal-ish |
| Workflow budget | not found | `workflow.Runner.Budget` (calls/tokens, survives resume) | missing in theirs |
| A2A | `provider/a2aprovider` — client + server, task store, stream reconnection, polling | `a2a/` — remote as tool (`tool.go`), serve profile as A2A (`server.go`), origin-pinned creds | equal (theirs more task modes; ours stricter origin) |
| AG-UI | `provider/aguiprovider` — server, frontend tools, HITL, shared state | not found | missing in ours |
| Local HTTP API | AG-UI/A2A hosting only | `serve/` — loopback, bearer, SSE resumable, approvals, cancel, health, metrics | missing in theirs (as a generic one) |
| Providers | OpenAI (chat+responses), Azure OpenAI, Foundry, Anthropic, Gemini, GitHub Copilot, A2A | openai-compatible (many named), anthropic, chatgpt subscription, ollama, fake | equal; theirs has Gemini native/Copilot/Foundry; ours has ChatGPT subscription + any OpenAI-compatible |
| Provider resilience | not found (relies on vendor SDK retries) | `provider/retry`, `circuit`, `ratelimit`, `budget` | missing in theirs |
| Observability | `provider/otelprovider` (agent/model/tool spans, GenAI semconv metrics), `workflow/observability/opentelemetry` | `metrics/` (Prometheus text, stdlib), `eventlog/` (slog, metadata only); no OTel | theirs better: OpenTelemetry traces + GenAI semconv |
| Durable execution | checkpoints only; no durable runtime found | file sessions + workflow journal, `trigger/` runner, `backup/` | neither has a durable engine; ours closer for single-host |
| Skills | `agent/skills` (fsskills, mcpskills, inline, script runner via app) | `skills/` (install w/ SHA-256 pinning, sandboxed scripts, output delivery) | ours better on safety; theirs adds MCP-sourced skills |
| Harness | `agent/harness` — todo, plan/execute mode, file memory, skills, approvals, web search, tracing preset; `loop` evaluators re-invoke until done | pieces exist (`todo/`, `notes/`, `rules/`, `skills/`), assembled in `cmd/ringil` not SDK | theirs better: one-call preset + `agentmode` + `loop` evaluator |
| Declarative agents | not implemented (README) | `ringil.yaml` but in `cmd/ringil` (app), not SDK | missing in both (SDK level) |
| Evaluation / testing | `internal/agenttest` (internal), Foundry evaluators only in generated client | `provider/fake` (scripted public provider), fuzz/security tests | ours better for SDK users (public fake); no eval framework in either |
| Triggers / scheduling | not found | `trigger/` (cron-like, file watch, webhook, chat), `later/` | missing in theirs |
| Chat channels | not found | `channel/`, `channel/telegram` | missing in theirs |
| Secrets | not found (Azure credential only) | `secrets/` env file, keyring, encfile, vault | missing in theirs |
| Artifacts | not found | `artifact/` versioned, SHA-256 checked | missing in theirs |
| Project rules (AGENTS.md…) | not found | `rules/` | missing in theirs |
| Tool discovery (deferred schemas) | not found | root `discovery.go` `tool_search` | missing in theirs |
