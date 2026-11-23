# How Microsoft Agent Framework structures its docs — and what we take

## 1. Information architecture

`toc.json` has 12 top-level sections, ordered from "first contact" to "maintenance":

1. **Overview**: one page.
2. **Get Started**: 7 numbered steps (Your First Agent, Add Tools, Multi-Turn, Memory, Workflows, Harness, Host).
3. **Concepts**: Agents and Workflows. This is the mental model: pipeline, session, middleware, safety.
4. **Agent Capabilities**: one page per feature (structured outputs, RAG, skills, observability, and Tools with 9 subpages).
5. **Workflow Capabilities**: the same pattern for workflows.
6. **Integrations**: indexed twice, *by provider* and *by component* (model providers, context providers, tools, UI).
7. **Hosting**: the ways to expose an agent (A2A, MCP, Telegram, OpenAI-compatible endpoints).
8. **Journey**: a narrative path from LLM to agent to workflow.
9. **Migration Guide**: from AutoGen and Semantic Kernel.
10. **API Reference**: links out to the generated reference and is never hand-written.
11. **Support**: FAQ, Troubleshooting and Upgrade Guides.

Depth is 2–4 levels, and every group opens with an `Overview` page. The rule is **one page = one feature**, and the page is never split by language. Languages are handled with **zone pivots**: one URL, a C# / Python / Go switch, and per-language blocks (`data-pivot="programming-language-go"`). Shared prose is written once, and only the code and API names change. Go gets its own table where the surface differs (the Tools page lists the Go `Package` column).

## 2. Page template

- **H1 = the feature name.** The step pages add a number ("Step 2: Add Tools").
- **One lead sentence that says what it is or what you get.** It is not a history.
  - "Create an agent and get a response — in just a few lines of code." (Step 1)
  - "Tools let your agent call custom functions — like fetching weather data, querying a database, or calling an API." (Step 2)
  - "An Agent Framework agent combines an agent abstraction, a model or remote-agent connection, instructions, tools, middleware, context providers, and session state behind a consistent run interface." (Concepts/Agents)
- **Code first on Get Started and capability pages.** The pattern is a sentence, a snippet, another sentence, another snippet: "Create the agent:" then code, then "Run it:" then code, then "Or stream the response:" then code. Prose links the snippets and does not explain them.
- **Concept pages are prose and an index.** They have short sections, each one or two sentences plus links to child pages, and almost no code.
- **"When to use" is a table, not paragraphs.** Example: "Use an agent when… / Use a workflow when…". It ends on one line: "If you can write a function to handle the task, do that instead of using an AI agent."
- **A Tip box links to the full runnable sample** ("See the full sample for the complete runnable file."). The page shows only the essential lines.
- **Every page ends with `## Next steps`**: one primary link (the next step), plus a "Go deeper:" list of two or three links.
- **Length**: Get Started pages run 300–650 words including code, concept overviews about 600, and capability pages 2,000–3,000 (but split three ways by language).

## 3. How they stay concise

- **They link instead of repeating.** Prerequisites appear once, in Step 1. Later pages say "For prerequisites… see the Create and run a simple agent step".
- **Tables replace prose** for tool types, the provider support matrix, the step list ("Step / What you'll learn") and role trust levels.
- **The sample is the single source of complete code.** Pages show fragments, and the repo holds the runnable `main.go`.
- **Samples are numbered and progressive.** `examples/01-get-started/01_hello_agent … 05_first_workflow` mirrors the Get Started steps one to one. Then come `02-agents/<topic>/stepNN_*` (step01_running … step24_filememory), `03-workflows/01-start-here/01..08` plus topic folders, `05-end-to-end/` and `demos/`. Each sample is one `main.go` whose header logger states a title and a one-line purpose.
- **README ≠ docs.** The README covers: what it is, who it fits, a feature list where each item is one line plus links (example and package), install, one quickstart, links to examples, troubleshooting as a Problem/Cause/Fix table, and contributor files. It teaches nothing beyond the quickstart. The docs site is where you learn.
- **Godoc is the reference.** Each package has a `doc.go` of 3–10 lines: what the package is, its main types, and where the implementations live. The `tool` package's doc says "Concrete implementations live in the subpackages functool, agenttool, …".

Things we do **not** copy: preview/"not implemented yet" banners, legal disclaimers repeated on every page, the long Safety page written as advice, the old tutorial-style tool-approval page ("This tutorial step shows you how…"), and a stale FAQ (it still says only .NET and Python).

## 4. Proposed structure for ringil-agent-sdk

Today the README is 569 lines and holds every feature, `docs/` holds internal plans, and `examples/` has three unnumbered folders. The target is below.

**README.md** (about 120 lines) covers what it is in one sentence, install, one complete agent (the current `examples/agent`), the CLI in five commands, a feature list (one line each, with links to the docs page and the package), and links. Nothing else.

**docs/** is a site tree; the user-facing version can be mkdocs or plain Markdown. Internal plans move to `docs/dev/`.

```
docs/
  index.md                 overview: what it is, agent vs workflow table, 20-line example
  get-started/
    01-first-agent.md      provider, run, stream
    02-tools.md            NewTool, typed input
    03-sessions.md         conversation on disk, resume
    04-memory.md
    05-workflow.md
    06-delegate.md         sub-agents
    07-serve.md            expose it (serve, A2A, Telegram)
  concepts/
    loop.md                provider ↔ tools ↔ history, events
    session.md             history, compaction
    safety.md              what is on by default and why (assertive, no advice list)
    errors.md              error codes
  capabilities/
    tools/ (index, files, bash, sandbox, approval, discovery, mcp, composio)
    memory.md  skills.md  rules.md  todo-notes.md  artifact.md
    workflow.md  delegate.md  later-trigger.md  backup.md  metrics.md
  providers/               index (table: name, env var, streaming, tools) + one page each
  ringil/                  the app: cli.md, config.md (ringil.yaml), tui.md, channels.md, serve.md
  reference.md             link to pkg.go.dev, nothing hand-written
  troubleshooting.md       Problem | Cause | Fix table
```

**Page template** (every page):

```
# <Feature>
<One sentence: what you get.>

<The smallest snippet that works.>
<One-sentence bridges: "Run it:", "Add a session:">

## <Option or variant>   (only if needed: table > prose)

Full sample: examples/NN-…/main.go

## Next
- <next step>  ·  <one or two deeper links>
```

Rules: result first, imperative voice, no "may/might/consider", no limitations section, a feature that is not shipped is not mentioned, and no prerequisite or concept is explained twice (link to it). Target about 400 words for Get Started pages and at most 800 for capability pages.

**Examples**: number them and mirror Get Started one to one.

```
examples/
  01-get-started/01_first_agent 02_tools 03_session 04_memory 05_workflow 06_delegate 07_serve
  02-agent/   tools_files, bash_sandbox, approval, mcp, skills, rules, compaction, providers/<name>
  03-workflow/  01_plan, 02_parallel, 03_triggers
  04-ringil/  config (ringil.yaml), channel_telegram, serve_http
  05-end-to-end/  review (the current skills/review), chat (the current chat)
```

Each `main.go` header states a title, one purpose line and the run command. This is already done in `examples/agent`.

**Godoc** gives each package a `doc.go` with the same lead style (most already have one): one sentence on what it is, the key types, and pointers to subpackages. Root `doc.go` stays the map of the SDK. API details live only there. Docs pages link to `pkg.go.dev`, and no reference material is hand-written.
