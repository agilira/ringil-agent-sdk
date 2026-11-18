// cli.go: commands, flags and the agent they build
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bufio"
	"context"
	stderrors "errors"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/memory"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/provider/retry"
	"github.com/agilira/ringil-agent-sdk/sandbox"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/skills"
	"github.com/agilira/ringil-agent-sdk/tools/files"
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
}

func newCLI(in io.Reader, out, errOut io.Writer) *cli {
	return &cli{
		in: in, out: out, errOut: errOut, getenv: os.Getenv, newEmbedder: openai.NewEmbedder,
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
		AddGlobalFlag("embed-model", "", "", "embedding model for memory by meaning (or RINGIL_EMBED_MODEL); same endpoint and key as the chat model").
		AddGlobalFlag("agents-config", "", "", "remote A2A agents file ({\"agents\": ...}); default <config>/ringil/agents.json").
		AddGlobalFlag("mcp-config", "", "", "MCP servers file ({\"mcpServers\": ...}); default <config>/ringil/mcp.json").
		AddGlobalFlag("env-file", "e", "", "private env file (mode 600) read for settings and secrets; the environment wins").
		AddGlobalFlag("system", "s", "", "system prompt (default: the SDK's)").
		AddGlobalIntFlag("max-turns", "", 0, "stop after this many model calls per prompt (0: no limit)").
		AddGlobalBoolFlag("allow-http", "", false, "allow plain HTTP to a non-loopback host").
		AddGlobalBoolFlag("no-tools", "", false, "do not offer the built-in tools").
		AddGlobalBoolFlag("no-memory", "", false, "do not recall or keep memories across conversations").
		AddGlobalIntFlag("context-window", "", 0, "model context size in tokens; compaction starts near it (0: only when the model reports it is full)").
		AddGlobalBoolFlag("summarize", "", false, "summarize left-out turns with the model instead of dropping them (costs a call)").
		AddGlobalFlag("session", "S", "", "save the conversation under this name and resume it next time").
		AddGlobalFlag("sessions-dir", "", "", "where sessions are stored (default: the user config directory)")
	app.AddCommand(withWorkspace(orpheus.NewCommand("chat", "Talk to the agent; /reset clears the history, /exit quits").SetHandler(c.cmdChat)))
	app.AddCommand(withWorkspace(orpheus.NewCommand("run", "Send one prompt and print the answer").SetHandler(c.cmdRun).
		SetUsage("run <prompt...>")))
	app.AddCommand(orpheus.NewCommand("sessions", "List saved sessions").SetHandler(c.cmdSessions).
		AddSubcommand(orpheus.NewCommand("rm", "Delete a saved session").SetHandler(c.cmdSessionRemove).
			SetUsage("sessions rm <name...>")))
	app.AddCommand(orpheus.NewCommand("memory", "List what the agent remembers").SetHandler(c.cmdMemory).
		AddSubcommand(orpheus.NewCommand("search", "Search what the agent remembers").SetHandler(c.cmdMemory).SetUsage("memory search <words...>")).
		AddSubcommand(orpheus.NewCommand("rm", "Delete memories by ID").SetHandler(c.cmdMemoryRemove).SetUsage("memory rm <id...>")))
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
	tools, err := c.tools(ctx, mem, cat)
	if err != nil {
		return nil, err
	}
	system := ctx.GetGlobalFlagString("system")
	if cat != nil {
		system += cat.Instructions()
	}
	return agent.New(agent.Config{
		Provider:      retry.Wrap(p, retry.Config{}),
		Tools:         tools,
		Memory:        mem,
		Approve:       c.approve,
		System:        system,
		MaxTurns:      ctx.GetGlobalFlagInt("max-turns"),
		ContextWindow: ctx.GetGlobalFlagInt("context-window"),
		Compaction:    agent.Compaction{Summarize: ctx.GetGlobalFlagBool("summarize")},
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
	ws, err := files.New(files.Config{Dirs: dirs, Ask: c.askAccess})
	if err != nil {
		return nil, err
	}
	return reg.With(ws.ReadTools()...)
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
	mcpTools, err := c.connectMCP(ctx)
	if err != nil {
		return nil, err
	}
	agents, err := c.connectAgents(ctx)
	return append(mcpTools, agents...), err
}

// extraTools: memory and skill tools, when there is something to use.
func (c *cli) extraTools(mem agent.Memory, cat *skills.Catalog) []agent.Tool {
	var extra []agent.Tool
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
func (c *cli) askAccess(_ context.Context, dir string) (bool, error) {
	return c.confirm("\n  ? The agent wants to access " + clean(dir) + " — allow? [y/N] ")
}

// approve asks before destructive tools (forget, MCP tools from servers
// whose hints are not trusted, file writes later); everything else runs
// without interrupting the user. "a" allows that tool for the rest of
// the session.
func (c *cli) approve(_ context.Context, call agent.ToolCall, spec agent.ToolSpec) (bool, error) {
	if !spec.Annotations.Destructive || c.allowed[call.Name] {
		return true, nil
	}
	answer, err := c.ask("\n  ? The agent wants to run " + call.Name + " " + short(string(call.Input)) + " — allow? [y/N/a=always this session] ")
	if answer == "a" || answer == "always" || answer == "sempre" {
		if c.allowed == nil {
			c.allowed = map[string]bool{}
		}
		c.allowed[call.Name] = true
		return true, err
	}
	return yes(answer), err
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
	base, err := c.setting(ctx, res, "base", "RINGIL_BASE_URL")
	if err != nil {
		return nil, err
	}
	model, err := c.setting(ctx, res, "model", "RINGIL_MODEL")
	if err != nil {
		return nil, err
	}
	if base == "" || model == "" {
		return nil, errors.New(ErrUsage, "set --base and --model, or RINGIL_BASE_URL and RINGIL_MODEL")
	}
	keyName := ctx.GetGlobalFlagString("key-env")
	if err := secrets.ValidateName(keyName); err != nil {
		return nil, err
	}
	return c.newProvider(openai.Config{
		BaseURL:   base,
		Model:     model,
		KeyFunc:   secrets.OptionalKeyFunc(res, keyName),
		AllowHTTP: ctx.GetGlobalFlagBool("allow-http"),
	})
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
