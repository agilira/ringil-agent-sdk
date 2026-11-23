# Security

ringil-agent-sdk is built for agents that act on real systems. Security is
part of the SDK, not left to the application: what the model does is
confined, approved and recorded by default. This page states where the
line is.

**A finding is a vulnerability when it breaks a line of "The SDK
guarantees".** What the other two sections name is the host's to decide,
or outside the model; reports on them are welcome as improvements.

## The SDK guarantees

- **Commands run confined.** bash and skill scripts run in the sandbox, and
  stdio MCP servers do when the host gives them one: bubblewrap with
  namespaces and seccomp on Linux, Seatbelt on macOS, with resource
  limits. They see only the directories granted, read-only unless granted
  writable, and none of the host's environment; on Linux, no host
  processes. A bash command leaves the sandbox only on a person's yes
  (`Confirm`); with no one to ask it is refused.
- **The sandbox's network is the egress proxy.** A sandboxed program has
  no network, or reaches the allowed domains and ports alone, through the
  proxy; never loopback, the local network or cloud metadata, a name that
  resolves there included.
- **Approval by default.** With no `Approve`, a call that changes files, is
  destructive, reaches out, or is unmarked is refused; reading and the
  agent's own bookkeeping run, and bash and skill scripts, which confine
  themselves, run and ask the person for what leaves the sandbox.
  `agent.Policy` asks a person, applies the host's rules (deny wins over
  ask, ask over allow) and its mode; plan mode refuses every change
  whatever allows it. Rules read only the argument the tool's schema
  declares.
- **Protected paths need a yes of their own.** Writing a protected path (by
  default the agent's rules, `.git`, editor and CI configuration) runs only
  on the host's `ConfirmProtected` yes; without one it is refused, whatever
  the mode.
- **Files stay inside their grants.** File tools resolve under `os.Root`:
  no symlink or `..` escape. Secrets files (`.env`, keys, credentials,
  keyrings, browser profiles) are never read unless the host replaces the
  list: what a tool reads reaches the provider.
- **MCP tools are destructive unless trusted.** A server's annotations count
  only when the host trusts them. Remote MCP is HTTPS (plain http to a
  loopback address, or where the host allows it); redirects to another
  origin are never followed; calls and results are bounded.
- **Skills are pinned.** The user installs them; the model can only write
  skills of instructions alone, through `create_skill`, which asks, and
  change only its own. Every file is pinned by SHA-256, a changed skill is
  refused, and scripts run in the sandbox from the verified copy.
- **Secrets stay secret.** The model never gets a resolver. Errors never
  carry a secret value or an upstream body. A failing secret source stops
  the lookup, with no fallback. A provider's key goes only to its
  configured endpoint; redirects never leave its origin. With a secrets
  store, a key in the store and in the environment is an error; with
  `store.StoreOnly`, a key comes from the store alone.
- **Logs carry no content.** The `eventlog` and `metrics` packages record
  metadata only, never prompts, arguments or results.

## The host decides

- The `Approve` policy: its mode, its rules, who is asked, and
  `ModeBypass`; bash's `Confirm`, the yes that lets a command out of the
  sandbox; the `ConfirmProtected` answer.
- What it grants: directories, writable folders, allowed domains,
  `egress.Private`, trusted MCP annotations, the local MCP servers it runs
  without a sandbox (the default, as coding agents run the servers their
  user configured), the remote MCP and A2A endpoints it connects.
- The lists it may replace: secrets files, protected paths.
- Its secrets: the store, `store.Resolver` or `store.StoreOnly`, and which
  key goes to which provider's endpoint.
- Whom it trusts for context. Memory, rules, notes, history and tool
  results go to the model as they are: the SDK limits what the model can
  do with them, it does not filter them. Raw `agent.Event`s carry content:
  an observer that logs them logs it.
- The process: its OS user, file permissions, environment, and the
  integrity of the binary and its configuration.
- Who reaches `serve` beyond its token and the channels' owners.

## Outside the model

- **Prompt injection as such.** The SDK bounds what an injected
  instruction can do (sandbox, grants, egress, approval); it does not
  detect or filter it.
- **Rules on shell text are best effort.** A deny rule on bash finds a
  command however it is quoted, prefixed or launched; what a command
  decodes and runs (`base64 -d | sh`) no rule sees. bash's boundary is the
  sandbox.
- **A home granted whole hides its secrets where they lie.** A project
  directory is walked at every command, so a secret file (`*.pem`,
  `id_rsa`) is hidden from bash at any depth. A home, as the working
  directory or a read grant, is not: walking one takes seconds per
  command, so bash hides the secrets at its top and in their known places
  (`.ssh`, `.aws`, `.gnupg`...), and a key file left deep inside it stays
  readable to a command, though `read_file` refuses it. For the full
  guarantee, work in a project directory.
- **What the provider receives and keeps.** Whatever the model is shown
  goes to its provider.
- **Content safety and moderation.**
- **A compromised host**: control of the process (`LD_PRELOAD`, root,
  ptrace) or of the kernel, including sandbox escapes through kernel bugs.
- **What MCP servers, A2A agents and skills do inside the grants given to
  them.**

## Reporting a Vulnerability

We greatly value the efforts of the community in identifying and responsibly disclosing security vulnerabilities. Your contributions help us ensure the safety and reliability of our software.

If you have discovered a vulnerability in one of our products or have security concerns regarding AGILira software, please contact us at **security@agilira.com**.

To help us address your report effectively, please include the following details:

- **Steps to Reproduce**: A clear and concise description of how to reproduce the issue or a proof-of-concept.
- **Relevant Tools**: Any tools used during your investigation, including their versions.
- **Tool Output**: Logs, screenshots, or any other output that supports your findings.

For more information about AGILira's security practices, please visit our [Security Page](https://agilira.one/security).

Thank you for helping us maintain a secure and trustworthy environment for all users.

---

ringil-agent-sdk • an AGILira library