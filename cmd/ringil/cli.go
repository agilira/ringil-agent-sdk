// cli.go: commands, flags and the agent they build
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	stderrors "errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/delegate"
	"github.com/agilira/ringil-agent-sdk/memory"
	"github.com/agilira/ringil-agent-sdk/notes"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/provider/retry"
	"github.com/agilira/ringil-agent-sdk/rules"
	"github.com/agilira/ringil-agent-sdk/sandbox"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/serve"
	"github.com/agilira/ringil-agent-sdk/skills"
	"github.com/agilira/ringil-agent-sdk/todo"
	"github.com/agilira/ringil-agent-sdk/tools/files"
	"github.com/agilira/ringil-agent-sdk/workflow"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// cli holds everything a command touches outside the process, so tests can
// replace it.
type cli struct {
	in          io.Reader
	out, errOut io.Writer
	getenv      func(string) string
	// newProvider builds the model client; tests swap in a fake.
	newProvider func(openai.Config) (agent.Provider, error)
	// turnContext scopes one user turn, so Ctrl-C stops the answer, not the
	// chat.
	turnContext func(context.Context) (context.Context, context.CancelFunc)
	// configDir is where sessions live by default.
	configDir func() (string, error)
	// newEmbedder builds the embedding model; tests may replace it.
	newEmbedder func(openai.Config) (agent.Embedder, error)
	// closers stop the MCP servers started for the current command.
	closers []func() error
	// notes are the session's notebooks, when there are any.
	notes *notes.Notes
	// crew runs the agent's sub-agents, background ones included.
	crew *delegate.Crew
	// projectNotes is the project notes folder, for workers to read.
	projectNotes string
	// readTools are the file reading tools, for read-only workers.
	readTools []agent.Tool
	// askMu keeps one question on screen at a time: workers run in
	// parallel and may all need the user.
	askMu sync.Mutex
	// outMu keeps lines whole when workers print at once.
	outMu sync.Mutex
	// workerIn and workerOut add up the workers' tokens of a turn.
	workerIn, workerOut atomic.Int64
	// allowed are tools the user allowed for the rest of the session.
	allowed map[string]bool
	// newSandbox returns the platform sandbox; tests may replace it.
	newSandbox func() (sandbox.Sandbox, error)
	// cwd is where the CLI was started, home the user's home; tests
	// replace them.
	cwd  func() (string, error)
	home func() (string, error)
	// lines reads the terminal; see readLine.
	lines *bufio.Scanner
	// server, under `ringil serve`, decides the calls the chat would ask
	// about, through its API; there is no terminal.
	server *serve.Server
	// remote are the MCP and remote agents' tools, connected once when
	// shared (under serve) for every run.
	remote []agent.Tool
	shared bool
	// events observe every run (serve logs their metadata).
	events []agent.Observer
	// acceptEdits lets file writes and edits go without asking
	// (--accept-edits); protected files are still confirmed.
	acceptEdits bool
	// role is the profile in use (--profile, or a profile served); nil
	// keeps the flags and environment.
	role *profile
}

func newCLI(in io.Reader, out, errOut io.Writer) *cli {
	return &cli{
		in: in, out: out, errOut: errOut, getenv: os.Getenv, newEmbedder: func(cfg openai.Config) (agent.Embedder, error) { return openai.NewEmbedder(cfg) },
		newProvider: openai.New,
		turnContext: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(ctx, os.Interrupt)
		},
		configDir:  os.UserConfigDir,
		newSandbox: sandbox.New,
		cwd:        os.Getwd,
		home:       os.UserHomeDir,
	}
}

func (c *cli) app() *orpheus.App {
	app := orpheus.New("ringil").
		SetDescription("Run an AI agent from the terminal").
		SetVersion(version).
		AddGlobalFlag("base", "b", "", "API base URL (default: $RINGIL_BASE_URL)").
		AddGlobalFlag("model", "m", "", "model name (default: $RINGIL_MODEL)").
		AddGlobalFlag("key-env", "", "RINGIL_API_KEY", "name of the secret holding the API key").
		AddGlobalIntFlag("workflow-calls", "", 0, "most successful model calls one workflow may make, across resumes; 0 is no limit").
		AddGlobalIntFlag("workflow-tokens", "", 0, "most tokens one workflow may use, across resumes; 0 is no limit").
		AddGlobalFlag("embed-model", "", "", "embedding model for memory by meaning (or RINGIL_EMBED_MODEL); same endpoint and key as the chat model").
		AddGlobalFlag("agents-config", "", "", "remote A2A agents file ({\"agents\": ...}); default <config>/ringil/agents.json").
		AddGlobalFlag("mcp-config", "", "", "MCP servers file ({\"mcpServers\": ...}); default <config>/ringil/mcp.json").
		AddGlobalFlag("env-file", "e", "", "private env file (mode 600) read for settings and secrets; the environment wins").
		AddGlobalFlag("system", "s", "", "system prompt (default: the SDK's)").
		AddGlobalFlag("profile", "", "", "a role from the profiles file: its model, endpoint, key and prompt (serve: the one --a2a offers)").
		AddGlobalFlag("profiles-config", "", "", "profiles file (default: <config>/ringil/profiles.json)").
		AddGlobalIntFlag("max-turns", "", 0, "stop after this many model calls per prompt (0: no limit)").
		AddGlobalBoolFlag("allow-http", "", false, "allow plain HTTP to a non-loopback host").
		AddGlobalBoolFlag("no-tools", "", false, "do not offer the built-in tools").
		AddGlobalBoolFlag("accept-edits", "", false, "write and edit files without asking (files holding your rules and settings are still confirmed)").
		AddGlobalFlag("allow-domains", "", "", "domains bash commands reach without asking, comma-separated (pypi.org,*.github.com); others are asked about").
		AddGlobalBoolFlag("no-memory", "", false, "do not recall or keep memories across conversations").
		AddGlobalIntFlag("context-window", "", 0, "model context size in tokens; compaction starts near it (0: only when the model reports it is full)").
		AddGlobalBoolFlag("summarize", "", false, "summarize left-out turns with the model instead of dropping them (costs a call)").
		AddGlobalFlag("session", "S", "", "save the conversation under this name and resume it next time").
		AddGlobalFlag("sessions-dir", "", "", "where sessions are stored (default: the user config directory)")
	app.AddCommand(withWorkspace(orpheus.NewCommand("chat", "Talk to the agent; /reset clears the history, /exit quits").SetHandler(c.cmdChat)))
	app.AddCommand(withWorkspace(orpheus.NewCommand("serve", "Answer a local HTTP API (loopback only, bearer token) until Ctrl-C; each run gets its own agent").SetHandler(c.cmdServe).
		AddFlag("listen", "", "127.0.0.1:8765", "loopback address and port to listen on").
		AddIntFlag("max-runs", "", 4, "runs going at once").
		AddBoolFlag("a2a", "", false, "also answer as an A2A agent at /a2a, with the same token").
		AddBoolFlag("no-schedule", "", false, "do not run the scheduled jobs (ringil schedule)")))
	app.AddCommand(withWorkspace(orpheus.NewCommand("run", "Send one prompt and print the answer").SetHandler(c.cmdRun).
		SetUsage("run <prompt...>")))
	app.AddCommand(orpheus.NewCommand("sessions", "List saved sessions").SetHandler(c.cmdSessions).
		AddSubcommand(orpheus.NewCommand("rm", "Delete a saved session").SetHandler(c.cmdSessionRemove).
			SetUsage("sessions rm <name...>")))
	app.AddCommand(orpheus.NewCommand("memory", "List what the agent remembers").SetHandler(c.cmdMemory).
		AddSubcommand(orpheus.NewCommand("search", "Search what the agent remembers").SetHandler(c.cmdMemory).SetUsage("memory search <words...>")).
		AddSubcommand(orpheus.NewCommand("rm", "Delete memories by ID").SetHandler(c.cmdMemoryRemove).SetUsage("memory rm <id...>")))
	app.AddCommand(orpheus.NewCommand("workflow", "List the workflows the agent ran").SetHandler(c.cmdWorkflows).
		AddSubcommand(orpheus.NewCommand("resume", "Continue an interrupted workflow").SetHandler(c.cmdWorkflowResume).SetUsage("workflow resume <id>")).
		AddSubcommand(orpheus.NewCommand("resolve", "Record the checked outcome of an uncertain step, from standard input").SetHandler(c.cmdWorkflowResolve).SetUsage("workflow resolve <id> <step>")).
		AddSubcommand(orpheus.NewCommand("retry", "Let resume run an uncertain step again").SetHandler(c.cmdWorkflowRetry).SetUsage("workflow retry <id> <step>")).
		AddSubcommand(orpheus.NewCommand("rm", "Delete workflows' journals").SetHandler(c.cmdWorkflowRemove).SetUsage("workflow rm <id...>")))
	app.AddCommand(orpheus.NewCommand("artifact", "List this project's artifacts").SetHandler(c.cmdArtifacts).
		AddSubcommand(orpheus.NewCommand("show", "Print an artifact (control characters removed; --raw for the exact bytes)").SetHandler(c.cmdArtifactShow).SetUsage("artifact show <name>").
			AddBoolFlag("raw", "", false, "write the exact bytes, for redirecting to a file")).
		AddSubcommand(orpheus.NewCommand("rm", "Delete artifacts").SetHandler(c.cmdArtifactRemove).SetUsage("artifact rm <name...>")))
	app.AddCommand(orpheus.NewCommand("state", "Back up, check and restore the agent's state").SetHandler(c.cmdStateHelp).
		AddSubcommand(orpheus.NewCommand("backup", "Save the agent's state to a new archive").SetHandler(c.cmdStateBackup).SetUsage("state backup <file.tar.gz>")).
		AddSubcommand(orpheus.NewCommand("verify", "Check an archive without extracting it").SetHandler(c.cmdStateVerify).SetUsage("state verify <file.tar.gz>")).
		AddSubcommand(orpheus.NewCommand("restore", "Restore an archive into a new directory, in quarantine").SetHandler(c.cmdStateRestore).SetUsage("state restore <file.tar.gz> <dir>")))
	app.AddCommand(orpheus.NewCommand("schedule", "List the jobs `ringil serve` runs on a timetable").SetHandler(c.cmdSchedule).
		AddSubcommand(orpheus.NewCommand("add", "Add or replace a job: cron (\"0 8 * * 1-5\"), @daily, @hourly or @every 10m").SetHandler(c.cmdScheduleAdd).
			SetUsage("schedule add <name> --when <spec> <prompt...>").
			AddFlag("when", "", "", "the timetable: a cron expression, @daily, @hourly, @weekly or @every DURATION").
			AddFlag("profile", "", "", "the role that runs it (default: the flags and environment of serve)").
			AddFlag("conversation", "", "", "carry on this conversation across runs (default: a fresh one each time)").
			AddFlag("time-zone", "", "", "IANA time zone for the timetable, e.g. Europe/Rome (default: local)").
			AddBoolFlag("catch-up", "", false, "run once at start-up if a run was missed while serve was down")).
		AddSubcommand(orpheus.NewCommand("rm", "Delete jobs").SetHandler(c.cmdScheduleRemove).SetUsage("schedule rm <name...>")).
		AddSubcommand(orpheus.NewCommand("pause", "Stop jobs running, keeping them").SetHandler(c.cmdSchedulePause).SetUsage("schedule pause <name...>")).
		AddSubcommand(orpheus.NewCommand("resume", "Let paused jobs run again").SetHandler(c.cmdScheduleResume).SetUsage("schedule resume <name...>")))
	app.AddCommand(orpheus.NewCommand("rules", "List the project rules the agent follows (AGENTS.md, CLAUDE.md, ...)").SetHandler(c.cmdRules))
	app.AddCommand(orpheus.NewCommand("skill", "List installed skills").SetHandler(c.cmdSkills).
		AddSubcommand(orpheus.NewCommand("install", "Install a skill from a directory with a SKILL.md").SetHandler(c.cmdSkillInstall).SetUsage("skill install <dir>")).
		AddSubcommand(orpheus.NewCommand("rm", "Remove installed skills").SetHandler(c.cmdSkillRemove).SetUsage("skill rm <name...>")))
	app.AddCommand(orpheus.NewCommand("sandbox", "Run a command isolated, as scripts and tools will be").SetHandler(c.cmdSandbox).
		SetUsage("sandbox [--mount DIR[:/dest][:rw]]... [--net] [--dir /path] -- command [args...]").
		AddStringSliceFlag("mount", "", nil, "grant a host directory, at its own path unless :/dest is given; read-only unless :rw").
		AddBoolFlag("net", "", false, "give the command an isolated network (no access to the host's services)").
		AddFlag("dir", "", "", "working directory inside the sandbox"))
	app.SetDefaultCommand("chat")
	return app
}

// ErrUsage marks a mistake in how the command was invoked.
const ErrUsage errors.ErrorCode = "USAGE"

func (c *cli) agent(ctx *orpheus.Context) (agent.Agent, error) {
	c.acceptEdits = ctx.GetGlobalFlagBool("accept-edits")
	if err := c.chooseProfile(ctx); err != nil {
		return nil, err
	}
	p, err := c.provider(ctx)
	if err != nil {
		return nil, err
	}
	mem, err := c.memory(ctx)
	if err != nil {
		return nil, err
	}
	cat, err := c.skillCatalog(ctx)
	if err != nil {
		return nil, err
	}
	nb, err := c.notebooks(ctx)
	if err != nil {
		return nil, err
	}
	c.notes = nb
	tools, err := c.tools(ctx, mem, cat)
	if err != nil {
		return nil, err
	}
	model := retry.Wrap(p, retry.Config{})
	if tools, err = c.withDelegate(ctx, model, tools); err != nil {
		return nil, err
	}
	system := c.system(ctx)
	if cat != nil {
		system += cat.Instructions()
	}
	return agent.New(agent.Config{
		Provider:      model,
		Tools:         tools,
		Memory:        mem,
		Approve:       c.approve,
		System:        system,
		Context:       c.runContext(nb),
		MaxTurns:      c.maxTurns(ctx),
		ContextWindow: ctx.GetGlobalFlagInt("context-window"),
		Compaction:    agent.Compaction{Summarize: ctx.GetGlobalFlagBool("summarize")},
		Observers:     c.events,
	})
}

// withWorkspace adds the flag granting directories to the file tools.
func withWorkspace(cmd *orpheus.Command) *orpheus.Command {
	return cmd.AddStringSliceFlag("workspace", "w", nil,
		"grant a directory to the file tools (read_file, list_directory, glob, grep); repeatable, the first is the base for relative paths")
}

// tools grants the built-in tools and, with --workspace, the file tools.
func (c *cli) tools(ctx *orpheus.Context, mem agent.Memory, cat *skills.Catalog) (agent.Registry, error) {
	if ctx.GetGlobalFlagBool("no-tools") {
		return nil, nil
	}
	reg, err := builtinTools()
	if err != nil {
		return nil, err
	}
	if reg, err = reg.With(c.extraTools(mem, cat)...); err != nil {
		return nil, err
	}
	remote, err := c.remoteTools(ctx)
	if err != nil {
		return nil, err
	}
	reg = c.addEach(reg, remote)
	dirs := ctx.GetFlagStringSlice("workspace")
	if len(dirs) == 0 {
		var err error
		if dirs, err = c.defaultDirs(); err != nil {
			return nil, err
		}
	}
	ws, err := files.New(files.Config{Dirs: dirs, Ask: c.askAccess, ConfirmProtected: c.confirmProtected})
	if err != nil {
		return nil, err
	}
	c.readTools = ws.ReadTools()
	sh, err := c.bashTool(ctx, dirs)
	if err != nil {
		return nil, err
	}
	return reg.With(append(append(c.readTools, ws.WriteTools()...), sh)...)
}

// addEach adds remote tools one by one: a name that clashes with another
// tool is reported and left out, the rest still work.
func (c *cli) addEach(reg agent.Registry, tools []agent.Tool) agent.Registry {
	for _, t := range tools {
		next, err := reg.With(t)
		if err != nil {
			c.note("tool " + t.Spec().Name + " left out: its name is already taken")
			continue
		}
		reg = next
	}
	return reg
}

// remoteTools are the tools of MCP servers and remote A2A agents.
func (c *cli) remoteTools(ctx *orpheus.Context) ([]agent.Tool, error) {
	if c.shared {
		return c.remote, nil
	}
	mcpTools, err := c.connectMCP(ctx)
	if err != nil {
		return nil, err
	}
	agents, err := c.connectAgents(ctx)
	return append(mcpTools, agents...), err
}

// withDelegate adds the delegate tool over the tools the agent has.
func (c *cli) withDelegate(ctx *orpheus.Context, model agent.Provider, tools agent.Registry) (agent.Registry, error) {
	if tools == nil {
		return nil, nil
	}
	crew, err := c.delegateCrew(ctx, model, tools)
	if err != nil {
		return nil, err
	}
	wt, err := c.workflowTool(ctx)
	if err != nil {
		return nil, err
	}
	// One tool per worker (explore, general): models call workers by name.
	return tools.With(append(crew.WorkerTools(), wt)...)
}

// runContext adds to every run the project's rules (AGENTS.md and the
// other agents' files) and the notebooks' indexes.
func (c *cli) runContext(nb *notes.Notes) func(context.Context) (string, error) {
	cfg, ok := c.rulesConfig()
	return func(ctx context.Context) (string, error) {
		var parts []string
		if ok {
			if r := rules.Render(rules.Load(cfg)); r != "" {
				parts = append(parts, r)
			}
		}
		if nb != nil {
			n, err := nb.Context(ctx)
			if err != nil {
				return strings.Join(parts, "\n\n"), err
			}
			if n != "" {
				parts = append(parts, n)
			}
		}
		return strings.Join(parts, "\n\n"), nil
	}
}

// rulesConfig: the start directory up to its repository root, and the
// user's own AGENTS.md in the config directory.
func (c *cli) rulesConfig() (rules.Config, bool) {
	cwd, err := c.cwd()
	if err != nil {
		return rules.Config{}, false
	}
	cfg := rules.Config{Dir: cwd}
	if base, err := c.configDir(); err == nil {
		cfg.Global = filepath.Join(base, "ringil", "AGENTS.md")
	}
	return cfg, true
}

func (c *cli) cmdRules(*orpheus.Context) error {
	cfg, ok := c.rulesConfig()
	if !ok {
		return errors.New(ErrUsage, "cannot read the current directory")
	}
	found := rules.Load(cfg)
	if len(found) == 0 {
		return c.line("no rules files found (AGENTS.md, CLAUDE.md, GEMINI.md, ...)")
	}
	for _, f := range found {
		if err := c.line(rules.SafePath(f.Path) + "  " + strconv.Itoa(len(f.Text)) + " bytes"); err != nil {
			return err
		}
	}
	return nil
}

// extraTools: memory and skill tools, when there is something to use.
func (c *cli) extraTools(mem agent.Memory, cat *skills.Catalog) []agent.Tool {
	extra := append([]agent.Tool{todo.Tool()}, c.artifactTools(false)...)
	if c.notes != nil {
		extra = append(extra, c.notes.Tool())
	}
	if mem != nil {
		extra = append(extra, memory.Tools(mem)...)
	}
	if cat != nil && len(cat.List()) > 0 {
		extra = append(extra, skills.Tools(cat, c.skillOptions())...)
	}
	return extra
}

// defaultDirs are the start directory and the user's home: the agent moves
// freely across the user's own files, as the user would, with secrets
// excluded by the file tools' policy. Anything else is asked for.
func (c *cli) defaultDirs() ([]string, error) {
	cwd, err := c.cwd()
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "cannot read the current directory")
	}
	dirs := []string{cwd}
	if home, err := c.home(); err == nil && home != "" && home != cwd {
		dirs = append(dirs, home)
	}
	return dirs, nil
}

// askAccess asks in the terminal whether the agent may use dir. No answer
// (end of input) is a no.
func (c *cli) askAccess(ctx context.Context, dir string) (bool, error) {
	if c.server != nil {
		return false, nil // no terminal to ask: only the granted directories
	}
	c.holdTerminal(ctx)
	defer c.askMu.Unlock()
	return c.confirm("\n  ? " + asker(ctx) + " wants to access " + clean(dir) + " — allow? [y/N] ")
}

// approve asks before destructive tools (forget, MCP tools from servers
// whose hints are not trusted, file writes later); everything else runs
// without interrupting the user. "a" allows that tool for the rest of
// the session.
func (c *cli) approve(ctx context.Context, call agent.ToolCall, spec agent.ToolSpec) (bool, error) {
	if c.server != nil {
		return c.serveApprove(ctx, call, spec)
	}
	c.holdTerminal(ctx)
	defer c.askMu.Unlock()
	// "always" is remembered per asker: a worker's yes is not the
	// parent's, nor another worker's.
	who, key := asker(ctx), call.Name
	if info, ok := delegate.Current(ctx); ok {
		key = info.Worker + ":" + call.Name
	}
	if !c.mustAsk(ctx, call, spec) || c.allowed[key] {
		return true, nil
	}
	if call.Name == workflow.ToolName {
		c.showPlan(call)
	}
	answer, err := c.ask("\n  ? " + who + " wants to run " + call.Name + " " + short(string(call.Input)) + " — allow? [y/N/a=always this session] ")
	return c.answered(key, answer), err
}

// answered reads a yes, no or always; always is remembered for key.
func (c *cli) answered(key, answer string) bool {
	if answer == "a" || answer == "always" || answer == "sempre" {
		if c.allowed == nil {
			c.allowed = map[string]bool{}
		}
		c.allowed[key] = true
		return true
	}
	return yes(answer)
}

// holdTerminal takes the terminal for a question; when the chat prompt
// holds it, the user is told a question is waiting for their next message.
func (c *cli) holdTerminal(ctx context.Context) {
	if c.askMu.TryLock() {
		return
	}
	if _, worker := delegate.Current(ctx); worker {
		c.note(asker(ctx) + " is waiting for your approval: it will ask after your next message")
	}
	c.askMu.Lock()
}

// asker names who is asking: the agent, or one of its workers.
func asker(ctx context.Context) string {
	if info, ok := delegate.Current(ctx); ok {
		return "Worker " + info.Worker + " " + strconv.Itoa(info.Task)
	}
	return "The agent"
}

// mustAsk: destructive tools, and the notes changes their scope wants
// confirmed (project notes, deletions).
// writesFiles are the tools that change files: confirmed unless
// --accept-edits.
var writesFiles = map[string]bool{"write_file": true, "edit_file": true}

// confirmProtected asks before any write to a protected file (other
// agents' rules, editor and CI settings, .git), even with --accept-edits;
// under serve, the API's caller decides.
func (c *cli) confirmProtected(ctx context.Context, path string) (bool, error) {
	if c.server != nil {
		input, err := json.Marshal(map[string]string{"path": path})
		if err != nil {
			return false, err
		}
		return c.server.Approve(ctx, agent.ToolCall{ID: "protected-" + rand.Text()[:8], Name: "write_protected_file", Input: input}, agent.ToolSpec{})
	}
	c.holdTerminal(ctx)
	defer c.askMu.Unlock()
	return c.confirm("\n  ! " + asker(ctx) + " wants to change " + clean(path) + ", which holds your own rules or settings — allow this change? [y/N] ")
}

// serveApprove: under serve, the API's caller decides what the chat
// would ask the user.
func (c *cli) serveApprove(ctx context.Context, call agent.ToolCall, spec agent.ToolSpec) (bool, error) {
	if !c.mustAsk(ctx, call, spec) {
		return true, nil
	}
	return c.server.Approve(ctx, call, spec)
}

func (c *cli) mustAsk(ctx context.Context, call agent.ToolCall, spec agent.ToolSpec) bool {
	if call.Name == "bash" {
		return false // the tool asks itself: outside the sandbox, and dangerous commands
	}
	if writesFiles[call.Name] {
		return !c.acceptEdits // protected files are confirmed apart, always
	}
	if call.Name != notes.DefaultName {
		return spec.Annotations.Destructive
	}
	if _, worker := delegate.Current(ctx); worker {
		// A worker writes only its own scratchpad (the project is read
		// only for it): only deleting is confirmed.
		return strings.Contains(string(call.Input), `"delete"`)
	}
	return c.notes != nil && c.notes.NeedsApproval(call)
}

// confirm asks a yes/no question; no answer is a no.
func (c *cli) confirm(question string) (bool, error) {
	answer, err := c.ask(question)
	return yes(answer), err
}

// ask prints a question and returns the lower-cased answer; empty at the
// end of input.
func (c *cli) ask(question string) (string, error) {
	if err := c.line(question); err != nil {
		return "", err
	}
	answer, ok := c.readLine()
	if !ok {
		return "", nil
	}
	return strings.ToLower(strings.TrimSpace(answer)), nil
}

func yes(answer string) bool {
	switch answer {
	case "y", "yes", "s", "si", "sì":
		return true
	}
	return false
}

// readLine reads one line from the terminal, shared by the chat prompt and
// the access questions so neither steals the other's input.
func (c *cli) readLine() (string, bool) {
	if c.lines == nil {
		c.lines = bufio.NewScanner(c.in)
		c.lines.Buffer(make([]byte, 0, 4096), maxLine)
	}
	if !c.lines.Scan() {
		return "", false
	}
	return c.lines.Text(), true
}

func (c *cli) provider(ctx *orpheus.Context) (agent.Provider, error) {
	res, err := c.resolver(ctx)
	if err != nil {
		return nil, err
	}
	base, err := c.roleSetting(ctx, res, "base", "RINGIL_BASE_URL")
	if err != nil {
		return nil, err
	}
	model, err := c.roleSetting(ctx, res, "model", "RINGIL_MODEL")
	if err != nil {
		return nil, err
	}
	if base == "" || model == "" {
		return nil, errors.New(ErrUsage, "set --base and --model, or RINGIL_BASE_URL and RINGIL_MODEL")
	}
	keyName := c.keyEnv(ctx)
	if err := validKeyName(keyName); err != nil {
		return nil, err
	}
	return c.newProvider(openai.Config{
		BaseURL:   base,
		Model:     model,
		KeyFunc:   keyFunc(res, keyName),
		AllowHTTP: ctx.GetGlobalFlagBool("allow-http"),
	})
}

// keyFunc reads the key from name; nil, no key, for "".
func keyFunc(res secrets.Resolver, name string) func(context.Context) (string, error) {
	if name == "" {
		return nil
	}
	return secrets.OptionalKeyFunc(res, name)
}

// validKeyName accepts a variable name, or "" for no key.
func validKeyName(name string) error {
	if name == "" {
		return nil
	}
	return secrets.ValidateName(name)
}

// resolver is where settings and secrets come from: the environment first,
// then the --env-file if given. The key is never taken from a flag, which
// would leave it in the shell history and the process list.
func (c *cli) resolver(ctx *orpheus.Context) (secrets.Resolver, error) {
	env := secrets.Resolver(lookupEnv{c.getenv})
	path := ctx.GetGlobalFlagString("env-file")
	if path == "" {
		return env, nil
	}
	file, err := secrets.EnvFile(path)
	if err != nil {
		return nil, err
	}
	return secrets.Chain(env, file), nil
}

// setting returns a flag, or else the named value from the resolver.
func (c *cli) setting(ctx *orpheus.Context, res secrets.Resolver, flag, name string) (string, error) {
	if v := ctx.GetGlobalFlagString(flag); v != "" {
		return v, nil
	}
	v, _, err := secrets.Lookup(ctx.Context(), res, name)
	return v, err
}

// lookupEnv adapts the CLI's getenv (replaced in tests) to a Resolver. An
// empty variable counts as unset, as it does for every RINGIL_ setting.
type lookupEnv struct{ getenv func(string) string }

func (l lookupEnv) Lookup(ctx context.Context, name string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	v := l.getenv(name)
	return v, v != "", nil
}

// describe turns an error into one line for the terminal.
func describe(err error) string {
	switch {
	case stderrors.Is(err, context.Canceled):
		return "interrupted"
	case errors.HasCode(err, agent.ErrProviderAuth):
		return "the API key was refused or is missing"
	case errors.HasCode(err, secrets.ErrMissing), errors.HasCode(err, secrets.ErrSource), errors.HasCode(err, secrets.ErrValue):
		return "secrets: " + err.Error()
	case errors.HasCode(err, agent.ErrProviderConfig):
		return "provider configuration: " + err.Error()
	}
	return err.Error()
}
