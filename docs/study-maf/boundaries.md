# Security boundaries: Microsoft Agent Framework (Go) vs ringil-agent-sdk

## 1. MAF's model, as written

**Framework guarantees**
- Local shell asks a human before every command by default; `AcknowledgeUnsafe` turns that off (`tool/shelltool/localshell.go:93-98,223`).
- Skill tools (`load_skill`, `read_skill_resource`, `run_skill_script`) are approval-required by default (`agent/skills/provider.go:472-480`).
- An approval response counts only if it matches a request the middleware surfaced, and it is re-bound to that request's tool call (`agent/harness/toolapproval/toolapproval.go:435-456`). An unknown tool is treated as needing approval (`:549-566`).
- File skills refuse symlinks and paths outside the skill dir (`agent/skills/fsskills/path_scope.go:59-90`). MCP skill archives have size limits, stay confined to their directory, and "extracted scripts are never executable" (`mcpskills/doc.go:12-17`).
- File memory stays inside its root, with dirs 0700 and files 0600 (`harness/filememory/filememory.go:124-127`).
- The telemetry mask goes only to allow-listed Azure hosts and carries no user data (`docs/feature-usage-telemetry.md:40-60`). The SDK's HTTPS and credential-origin guards stay in place (`provider/foundryprovider/agent.go:85-86`).
- Sensitive span data is off unless you opt in (`provider/otelprovider/embedding.go:43-49`).

**Developer is responsible for**
- "Developers must ensure that all providers attached to an agent are trusted. Agent Framework does not validate or filter the data returned by providers" (`agent/context.go:31-38`). The same applies to history (`agent/history.go:38-45`) and retrieval: "retrieved documents are untrusted… may require sanitization" (`textsearch/provider.go:162-164`).
- Isolation: "Consider using containerization or sandboxing for code execution" (`TRANSPARENCY_FAQ.md:83`). The regex policy "is a UX guardrail, NOT a security boundary" (`shelltool/shelltool.go:14,112`).
- "Only connect to trusted servers… Digests verify consistency… not authenticity" (`mcpskills/doc.go:28-30`). Evaluating third-party tools and MCP servers is yours (`TRANSPARENCY_FAQ.md:69,119`).
- Data leaving the boundary, minimisation, and API-key handling (`TRANSPARENCY_FAQ.md:120-122,130,137`; `README.md:204`).
- RAI mitigations such as metaprompts, content filters and human oversight (`README.md:206`; `TRANSPARENCY_FAQ.md:50,58`).

**Out of scope**
- Third-party systems are used "at your own risk" (`README.md:202`).
- Model behaviour (bias, ungrounded output, misuse) is inherited from the LLM (`TRANSPARENCY_FAQ.md:34-41`).
- Script execution belongs to the caller: `ScriptRunner` "is responsible for decoding and validating" (`agent/skills/skills.go:131-134`).

## 2. Enforced vs only documented in MAF

| Area | Enforced in code | Only documented |
|---|---|---|
| Shell | Approval flag on by default; 64 KiB output cap; cd-prefix confinement; `CleanEnvironment` exists but is **off** (full env inherited); no default timeout | No sandbox, network or filesystem isolation. "Use Docker" is advice |
| MCP (client) | Nothing. You pass your own `mcp.Transport`/`exec.Cmd`; tools carry **no** approval flag (`mcptool` has none) | "Careful selection and validation" |
| Hosted MCP | `ApprovalMode`; the default is whatever the provider does (OpenAI: always) | — |
| Approval | Request-ID binding; conservative unknown-tool default; 40-iteration auto-approve cap. A plain `functool` tool needs **no** approval unless wrapped | "Keep a human in the loop" |
| Credentials | Provider SDK same-origin and redirect guards; telemetry host allow-list | Key storage and rotation |
| Data to provider | No filtering or redaction; sensitive spans off by default | Minimisation and data-boundary advice |
| Skills | Symlink and path confinement; archive limits; approval by default | Script isolation (runner is yours) |

In short, MAF's only real boundary is **a human saying yes**. Everything else is listed as the developer's job.

## 3. Proposed boundary statement for ringil-agent-sdk

**The SDK guarantees**
- Bash, skill scripts and stdio MCP servers run in the sandbox (bubblewrap, namespaces, seccomp, rlimits). They see only granted directories, read-only unless granted writable, plus an empty environment, no host processes and no network. If the platform has no sandbox the result is `ErrUnavailable`, never an unconfined run. **[beyond MAF]**
- Sandboxed network goes only through the egress proxy: allowed domains and ports, and never loopback, LAN or metadata, even after DNS rebinding. **[beyond MAF]**
- Outside the sandbox, a bash command needs `Confirm` and so does a dangerous command. Writes to protected paths (agent rules, `.git`, editor/CI config) always confirm. **[beyond MAF: MAF has no command classes or protected paths]**
- File tools stay inside their grants (`os.Root`, no symlink escape). Secrets files (`DefaultExclude`) are never read, because what a tool reads reaches the provider. **[beyond MAF]**
- MCP tools count as destructive unless the host sets `TrustAnnotations`. Remote MCP is HTTPS only, cross-origin redirects are never followed, and calls and results are capped. **[beyond MAF: MAF's MCP tools need no approval]**
- Skills are installed by the user, never by the model. Every file is SHA-256-pinned, a modified skill is refused, and scripts run on the verified copy. **[beyond MAF]**
- The model never gets a secret resolver. Errors never carry a secret value or upstream body. A failing secret source stops the lookup with no fallback. A key goes only to its provider's origin. **[beyond MAF]**
- Logs and metrics carry metadata only, never prompts, arguments or results. The audit log is hash-chained. **[beyond MAF]**

**The host (developer) is responsible for**
- The `Approve` policy. The core default is *allow*, and which calls ask is the host's decision. **[MAF covers this better: shell and skills ask by default in the core, and the approval binding is built in. Ours is a synchronous callback, so no binding is needed, but the default is weaker]**
- What it grants: mounts, writable dirs, allowed domains, `TrustAnnotations`, unsandboxed MCP servers, remote MCP/A2A endpoints it chooses to trust.
- Trust in context sources: memory, rules, notes, history stores, RAG results and tool output are passed to the model as they are. **[MAF states this explicitly per interface. We must add the same sentence to `Config.Context`, memory and history docs]**
- Running the process: OS user, file permissions, the process environment, and the integrity of the binary and config (Cerberus is outside the SDK).
- Authenticating anyone who reaches `serve` beyond the token and channel owners.

**Out of scope**
- Prompt injection *as such*. We bound its consequences (sandbox, grants, egress, approval). We do not detect or filter it.
- Content safety and RAI moderation (MAF documents it, we do not ship it). **[MAF covers, as docs only]**
- What data the provider receives, and what it keeps. Whatever the model is shown goes to the provider.
- Anyone who controls the host process (`LD_PRELOAD`, root, ptrace) or the kernel. Sandbox escapes via kernel bugs are out too.
- Behaviour of MCP servers, A2A agents and skills *inside* the grants we gave them.
- Server-side ("hosted") tools run by the provider. **[MAF supports these with ApprovalMode; we have none]**

Use: a finding counts as a vulnerability only if it breaks a "guarantees" line. Anything in the other two columns is documentation, not hardening.
