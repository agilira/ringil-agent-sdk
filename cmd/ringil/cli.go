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
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"
	"golang.org/x/term"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/channel"
	"github.com/agilira/ringil-agent-sdk/channel/telegram"
	"github.com/agilira/ringil-agent-sdk/delegate"
	"github.com/agilira/ringil-agent-sdk/later"
	"github.com/agilira/ringil-agent-sdk/memory"
	"github.com/agilira/ringil-agent-sdk/metrics"
	"github.com/agilira/ringil-agent-sdk/notes"
	"github.com/agilira/ringil-agent-sdk/price"
	"github.com/agilira/ringil-agent-sdk/provider/anthropic"
	"github.com/agilira/ringil-agent-sdk/provider/chatgpt"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
	"github.com/agilira/ringil-agent-sdk/provider/retry"
	"github.com/agilira/ringil-agent-sdk/rules"
	"github.com/agilira/ringil-agent-sdk/sandbox"
	"github.com/agilira/ringil-agent-sdk/secrets"
	"github.com/agilira/ringil-agent-sdk/secrets/store"
	"github.com/agilira/ringil-agent-sdk/serve"
	"github.com/agilira/ringil-agent-sdk/skills"
	"github.com/agilira/ringil-agent-sdk/todo"
	"github.com/agilira/ringil-agent-sdk/tools/files"
	"github.com/agilira/ringil-agent-sdk/trigger"
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
	// signIns holds the ChatGPT sign-in, made once and shared with the
	// server's runs; chatgptIssuer, chatgptPort and chatgptBase point it
	// elsewhere (tests).
	signIns       *signIns
	chatgptIssuer string
	chatgptPort   int
	chatgptBase   string
	// newAnthropic builds a Claude client; tests swap in a fake.
	newAnthropic func(anthropic.Config) (agent.Provider, error)
	// turnContext scopes one user turn, so Ctrl-C stops the answer, not the
	// chat.
	turnContext func(context.Context) (context.Context, context.CancelFunc)
	// configDir is where sessions live by default; dataDir ringil's data
	// (dirs.For("ringil").Data): the documents sent in the chats, which
	// the agent's tools may read, unlike its configuration.
	configDir func() (string, error)
	dataDir   func() (string, error)
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
	// ws is the file tools' workspace, once made: what the agent may read,
	// for the tools that take a file it made (save_file_artifact).
	ws files.Workspace
	// imports is the import the app asked for, applied when serve ends;
	// restart starts the process again after it (a test's does not).
	imports *importing
	restart func()
	inImage func() bool        // the process runs in a container
	openURL func(string) error // opens a link in the user's browser
	// noKeyring keeps the store set up for the app's first key off the
	// system's keyring (tests).
	noKeyring bool
	// askMu keeps one question on screen at a time: workers run in
	// parallel and may all need the user.
	askMu sync.Mutex
	// outMu keeps lines whole when workers print at once.
	outMu sync.Mutex
	// workerIn and workerOut add up the workers' tokens of a turn, and
	// workerCost their cost in billionths of a dollar.
	workerIn, workerOut, workerCost atomic.Int64
	// maxCost is the dollars a prompt, and a worker's task, may cost.
	maxCost float64
	// unpriced is the run's model when it has no price.
	unpriced string
	// prices is the price list (priceList; tests swap it), priceWarned
	// whether its failure was said.
	prices      func(context.Context) (price.Table, error)
	priceWarned atomic.Bool
	// learned are the rules the user's "always" added, by asker (the
	// parent "", a worker by name).
	learned   map[string][]agent.Rule
	learnedMu sync.Mutex
	keepMu    sync.Mutex // one "p" answer writes ringil.yaml at a time
	// newSandbox returns the platform sandbox; tests may replace it.
	newSandbox func() (sandbox.Sandbox, error)
	// cwd is where the CLI was started, home the user's home; tests
	// replace them.
	cwd  func() (string, error)
	home func() (string, error)
	// scanner reads the terminal; see readLine.
	scanner *bufio.Scanner
	// server, under `ringil serve`, decides the calls the chat would ask
	// about, through its API; there is no terminal.
	server *serve.Server
	// inboxRoot, under serve with a chat app, is where the documents sent
	// in the chats are kept, a directory per conversation; inbox, in a
	// run, is its own conversation's, granted to the file tools.
	inboxRoot, inbox string
	// runConversation, in a run of serve, is the one it continues.
	runConversation string
	// remote are the MCP and remote agents' tools, connected once when
	// shared (under serve) for every run.
	remote []agent.Tool
	shared bool
	// events observe every run (serve logs their metadata).
	events []agent.Observer
	// traces is the process's tracer, shared by serve's runs; traceWarned
	// whether its failure was said.
	traces      *tracing
	traceWarned *atomic.Bool
	// metrics counts the served runs, for GET /metrics.
	metrics *metrics.Metrics
	// acceptEdits lets file writes and edits go without asking
	// (--accept-edits); protected files are still confirmed.
	acceptEdits bool
	// noApprovals allows every call without asking (--no-approvals).
	noApprovals bool
	// inTrigger: a run a trigger started; triggerAllow are the tools it
	// was allowed, the only calls that would ask it may make.
	inTrigger    bool
	triggerAllow []string
	// automating: a run started with /automate or `ringil automate`, the
	// only kind that may make triggers.
	automating bool
	// roleName is the profile a run of serve talks with; finalTools the
	// run's tools once made; triggers the runner serve runs (shared).
	roleName   string
	finalTools agent.Registry
	// live are the agent's tools, where add_mcp_server adds a server's;
	// catalog the skills install_skill reloads.
	live     *agent.LiveRegistry
	catalog  *skills.Catalog
	triggers *atomic.Pointer[trigger.Runner]
	// bridge is serve's bridge automations deliver through: Telegram's,
	// else Slack's; slackBridge is Slack's, for its people.
	bridge      *atomic.Pointer[channel.Bridge]
	slackBridge *atomic.Pointer[channel.Bridge]
	// board holds the checks set with check_later (shared under serve);
	// due, in the terminal, the checks come due waiting for the prompt.
	board       *later.Board
	due         chan later.Check
	laterLimits later.Limits // zero: the package's (tests shorten them)
	// input carries the terminal's lines, read by one goroutine (see
	// readLine).
	input    chan string
	readOnce sync.Once
	// role is the profile in use (--profile, or a profile served); nil
	// keeps the flags and environment.
	role *profile
	// settings is ringil.yaml in force (shared by serve's runs);
	// configEvery how soon an edit is seen (zero: 2s; tests shorten it).
	settings *settings
	secrets  *store.Cache     // the configured store, shared with serve's runs
	asker    orpheus.Prompter // the setup's questions; see prompter
	shadowed []string         // keys the setup kept that the environment has too
	// interactive says a person is at the terminal (tests set it).
	interactive func() bool
	// checkComposio tries a Composio key in the setup; tests replace it.
	checkComposio func(ctx context.Context, key string) (int, error)
	// setupBases replaces built-in providers' addresses in the setup
	// (tests: no network, no Ollama of this machine).
	setupBases map[string]string
	// newTelegram makes the setup's bot; tests replace it.
	newTelegram func(ctx context.Context, cfg telegram.Config) (telegramBot, error)
	configEvery time.Duration
	// log is serve's log; each run's lines name its configuration.
	log *slog.Logger
}

func newCLI(in io.Reader, out, errOut io.Writer) *cli {
	return &cli{
		in: in, out: out, errOut: errOut, getenv: os.Getenv, newEmbedder: func(cfg openai.Config) (agent.Embedder, error) { return openai.NewEmbedder(cfg) },
		newProvider:  openai.New,
		newAnthropic: anthropic.New,
		signIns:      &signIns{},
		imports:      &importing{},
		restart:      restart,
		inImage:      inImage,
		openURL:      openBrowser,
		turnContext: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(ctx, os.Interrupt)
		},
		configDir:     os.UserConfigDir,
		dataDir:       ringilData,
		secrets:       &store.Cache{},
		checkComposio: connectComposio,
		interactive: func() bool {
			f, ok := in.(*os.File)
			return ok && term.IsTerminal(int(f.Fd())) // #nosec G115 -- a file descriptor
		},
		newTelegram: func(ctx context.Context, cfg telegram.Config) (telegramBot, error) {
			return telegram.New(ctx, cfg)
		},
		triggers:    &atomic.Pointer[trigger.Runner]{},
		bridge:      &atomic.Pointer[channel.Bridge]{},
		slackBridge: &atomic.Pointer[channel.Bridge]{},
		newSandbox:  sandbox.New,
		cwd:         os.Getwd,
		home:        os.UserHomeDir,
		traces:      &tracing{},
		traceWarned: &atomic.Bool{},
	}
}

func (c *cli) app() *orpheus.App {
	app := orpheus.New("ringil").
		SetDescription("Run an AI agent from the terminal").
		SetVersion(version).
		AddGlobalFlag("model", "m", "", "model name (default: $RINGIL_MODEL)").
		AddGlobalFlag("provider", "", "", "the model's provider, by name: a built-in one (anthropic, chatgpt, openai, google, openrouter, nvidia, deepseek, requesty, zai, moonshot, xai, mistral, ollama) or one of ringil.yaml's providers (or RINGIL_PROVIDER)").
		AddGlobalIntFlag("workflow-calls", "", 0, "most successful model calls one workflow may make, across resumes; 0 is no limit").
		AddGlobalIntFlag("workflow-tokens", "", 0, "most tokens one workflow may use, across resumes; 0 is no limit").
		AddGlobalFlag("workflow-cost", "", "", "most US dollars one workflow may spend, across resumes (5.00); empty is no limit").
		AddGlobalFlag("models", "", "", "the models, in order, instead of ringil.yaml's: provider:model, comma-separated; the first answers, the others when it does not (anthropic:claude-opus-5-5,ollama:qwen3:8b)").
		AddGlobalFlag("max-cost", "", "", "stop a prompt once its model calls have cost this many US dollars (0.50), and each worker's task the same; empty is no limit").
		AddGlobalFlag("embed-model", "", "", "embedding model for memory by meaning (or RINGIL_EMBED_MODEL); same endpoint and key as the chat model").
		AddGlobalFlag("config", "c", "", "the configuration (default: <config>/ringil/ringil.yaml): settings, profiles, MCP servers, remote agents, serve; the flags win over it").
		AddGlobalFlag("env-file", "e", "", "private env file (mode 600) read for settings and secrets; the environment wins over it, and a key in the secrets store must be in neither").
		AddGlobalFlag("instructions", "", "", "a file of instructions added to the agent's prompt (tone, procedures, purpose): after the base prompt, which keeps its rules").
		AddGlobalFlag("profile", "", "", "a role from the profiles of ringil.yaml: its model, endpoint, key and prompt (serve: the one serve.a2a offers)").
		AddGlobalIntFlag("max-turns", "", 0, "stop after this many model calls per prompt (0: no limit)").
		AddGlobalBoolFlag("allow-http", "", false, "allow plain HTTP to a non-loopback host").
		AddGlobalBoolFlag("no-tools", "", false, "do not offer the built-in tools").
		AddGlobalBoolFlag("no-approvals", "", false, "never ask before a tool call: in the terminal, through the API and, for the runs an owner starts, in the chats (the others' calls are still refused)").
		AddGlobalBoolFlag("accept-edits", "", false, "write and edit files without asking (files holding your rules and settings are still confirmed)").
		AddGlobalFlag("allow-domains", "", "", "domains bash commands reach without asking, comma-separated (pypi.org,*.github.com); others are asked about").
		AddGlobalBoolFlag("no-memory", "", false, "do not recall or keep memories across conversations").
		AddGlobalIntFlag("context-window", "", 0, "model context size in tokens; compaction starts near it (0: only when the model reports it is full)").
		AddGlobalBoolFlag("summarize", "", false, "summarize left-out turns with the model instead of dropping them (costs a call)").
		AddGlobalFlag("session", "S", "", "save the conversation under this name and resume it next time").
		AddGlobalFlag("sessions-dir", "", "", "where sessions are stored (default: the user config directory)")
	app.AddCommand(withWorkspace(orpheus.NewCommand("chat", "Talk to the agent; /reset clears the history, /exit quits").SetHandler(c.firstRun(c.withConfig(c.cmdChat)))))
	app.AddCommand(withWorkspace(orpheus.NewCommand("serve", "Answer a local HTTP API (loopback only, bearer token) until Ctrl-C; each run gets its own agent; set up in the serve section of ringil.yaml").SetHandler(c.withConfig(c.cmdServe)).
		AddBoolFlag("open", "", false, "open the app in the browser even without a terminal (an installer that starts serve)").
		AddBoolFlag("no-browser", "", false, "never open the browser: only print how to sign in").
		AddSubcommand(orpheus.NewCommand("link", "Print a sign-in link of the app of the serve running here").SetHandler(c.withConfig(c.cmdServeLink)))))
	app.AddCommand(withWorkspace(orpheus.NewCommand("run", "Send one prompt and print the answer").SetHandler(c.withConfig(c.cmdRun)).
		SetUsage("run <prompt...>")))
	app.AddCommand(orpheus.NewCommand("sessions", "List saved sessions").SetHandler(c.withConfig(c.cmdSessions)).
		AddSubcommand(orpheus.NewCommand("rm", "Delete a saved session").SetHandler(c.withConfig(c.cmdSessionRemove)).
			SetUsage("sessions rm <name...>")))
	app.AddCommand(orpheus.NewCommand("login", "Sign in with a ChatGPT subscription (Plus, Pro, Business): ringil login chatgpt").SetHandler(c.withConfig(c.cmdLogin)).
		SetUsage("login chatgpt").AddBoolFlag("device", "", false, "sign in by a code typed on any device (no browser here: a server, SSH)"))
	app.AddCommand(orpheus.NewCommand("logout", "Sign out of ChatGPT: revoke the sign-in and forget it").SetHandler(c.withConfig(c.cmdLogout)).SetUsage("logout chatgpt"))
	app.AddCommand(orpheus.NewCommand("setup", "Set ringil up, with a few questions: where keys are kept, the model, your system prompt, the memory, a Telegram bot").SetHandler(c.holdingState(c.cmdSetup)))
	app.AddCommand(orpheus.NewCommand("secrets", "Keep keys in the store of ringil.yaml's secrets section (keyring, vault or file)").
		AddSubcommand(orpheus.NewCommand("set", "Save a secret; its value is read from standard input, without echo at a terminal").SetHandler(c.withConfig(c.cmdSecretsSet)).SetUsage("secrets set <NAME>")).
		AddSubcommand(orpheus.NewCommand("rm", "Delete a secret").SetHandler(c.withConfig(c.cmdSecretsRemove)).SetUsage("secrets rm <NAME>")).
		AddSubcommand(orpheus.NewCommand("move", "Move the keys to another store (keyring, file, vault) or to a new env file for a container (env --to-file PATH): each is copied and read back, ringil.yaml switched, and only then the old ones deleted, if you say so").
			SetHandler(c.withConfig(c.cmdSecretsMove)).SetUsage("secrets move keyring|file|vault|env").
			AddFlag("to-file", "", "", "the new env file of a move to env (mode 600; never an existing one)").
			AddFlag("vault-url", "", "", "Vault's address, when ringil.yaml does not have it").
			AddFlag("vault-path", "", "", "the path of ringil's keys in Vault's KV engine, when ringil.yaml does not have it")))
	app.AddCommand(orpheus.NewCommand("memory", "List what the agent remembers").SetHandler(c.withConfig(c.cmdMemory)).
		AddSubcommand(orpheus.NewCommand("search", "Search what the agent remembers").SetHandler(c.withConfig(c.cmdMemory)).SetUsage("memory search <words...>")).
		AddSubcommand(orpheus.NewCommand("rm", "Delete memories by ID").SetHandler(c.withConfig(c.cmdMemoryRemove)).SetUsage("memory rm <id...>")))
	app.AddCommand(orpheus.NewCommand("workflow", "List the workflows the agent ran").SetHandler(c.withConfig(c.cmdWorkflows)).
		AddSubcommand(orpheus.NewCommand("resume", "Continue an interrupted workflow").SetHandler(c.withConfig(c.cmdWorkflowResume)).SetUsage("workflow resume <id>")).
		AddSubcommand(orpheus.NewCommand("resolve", "Record the checked outcome of an uncertain step, from standard input").SetHandler(c.withConfig(c.cmdWorkflowResolve)).SetUsage("workflow resolve <id> <step>")).
		AddSubcommand(orpheus.NewCommand("retry", "Let resume run an uncertain step again").SetHandler(c.withConfig(c.cmdWorkflowRetry)).SetUsage("workflow retry <id> <step>")).
		AddSubcommand(orpheus.NewCommand("rm", "Delete workflows' journals").SetHandler(c.withConfig(c.cmdWorkflowRemove)).SetUsage("workflow rm <id...>")))
	app.AddCommand(orpheus.NewCommand("artifact", "List this project's artifacts").SetHandler(c.withConfig(c.cmdArtifacts)).
		AddSubcommand(orpheus.NewCommand("show", "Print an artifact (control characters removed; --raw for the exact bytes)").SetHandler(c.withConfig(c.cmdArtifactShow)).SetUsage("artifact show <name>").
			AddBoolFlag("raw", "", false, "write the exact bytes, for redirecting to a file")).
		AddSubcommand(orpheus.NewCommand("rm", "Delete artifacts").SetHandler(c.withConfig(c.cmdArtifactRemove)).SetUsage("artifact rm <name...>")))
	app.AddCommand(orpheus.NewCommand("state", "Export the agent's state, check an archive, import one").SetHandler(c.cmdStateHelp).
		AddSubcommand(orpheus.NewCommand("export", "Save the agent's state to a new archive").SetHandler(c.withConfig(c.cmdStateExport)).SetUsage("state export <file.tar.gz>")).
		AddSubcommand(orpheus.NewCommand("verify", "Check an archive without extracting it").SetHandler(c.cmdStateVerify).SetUsage("state verify <file.tar.gz>")).
		AddSubcommand(orpheus.NewCommand("import", "Replace the state with an archive's, keeping a copy of it before").SetHandler(c.cmdStateImport).SetUsage("state import <file.tar.gz>")))
	app.AddCommand(withWorkspace(orpheus.NewCommand("automate", "Make something happen by itself from now on: a trigger, shown before it is made").SetHandler(c.withConfig(c.cmdAutomate)).
		SetUsage("automate <what, e.g. every weekday at 8 summarize my mail>")))
	app.AddCommand(orpheus.NewCommand("trigger", "List the triggers `ringil serve` runs: on a time, a change, a message, a webhook").SetHandler(c.withConfig(c.cmdTrigger)).
		AddSubcommand(orpheus.NewCommand("add", "Add or replace a trigger").SetHandler(c.withConfig(c.cmdTriggerAdd)).
			SetUsage("trigger add <name> [--kind time|tool|file|message|webhook] [--skill <skill>] <prompt...>").
			AddFlag("kind", "", "", "what sets it off: time, tool (its answer changes), file (files change), message (in a chat), webhook; default from --tool, --path, --chat, else time").
			AddFlag("when", "", "", "time: the timetable (cron \"0 8 * * 1-5\", @daily, @hourly, @weekly, @every 10m); tool and file: how often to look").
			AddFlag("time-zone", "", "", "IANA time zone for the timetable, e.g. Europe/Rome (default: local)").
			AddBoolFlag("catch-up", "", false, "time: run once at start-up if a run was missed while serve was down").
			AddFlag("tool", "", "", "tool: the tool to ask (one that changes nothing)").
			AddFlag("args", "", "", "tool: its arguments, as JSON").
			AddFlag("path", "", "", "file: the file or directory to look at").
			AddFlag("chat", "", "", "message: the chat to listen to, as telegram-CHATID (a group's ID is negative); only owners' and members' messages are heard").
			AddFlag("match", "", "", "message: words the message must have (default: any message)").
			AddFlag("skill", "", "", "the skill its runs follow").
			AddFlag("allow", "", "", "tools its runs may use without asking, comma-separated; any other that would ask is refused").
			AddFlag("deliver", "", "", "where to send what its runs answer (a chat)").
			AddFlag("profile", "", "", "the role that runs it (default: the flags and environment of serve)").
			AddFlag("timeout", "", "", "stop a run that takes longer (default 10m, at most 2h)").
			AddFlag("from", "", "", "start working on this date: 2026-12-01 or 2026-12-01 09:00 (default now)").
			AddFlag("until", "", "", "stop working and be removed after this date: 2026-12-31 (the whole day) or 2026-12-31 18:00").
			AddBoolFlag("continue", "", false, "carry on one conversation across its runs (default: a fresh one each time)")).
		AddSubcommand(orpheus.NewCommand("rm", "Delete triggers").SetHandler(c.withConfig(c.cmdTriggerRemove)).SetUsage("trigger rm <name...>")).
		AddSubcommand(orpheus.NewCommand("pause", "Stop triggers, keeping them").SetHandler(c.withConfig(c.cmdTriggerPause)).SetUsage("trigger pause <name...>")).
		AddSubcommand(orpheus.NewCommand("resume", "Let paused triggers run again").SetHandler(c.withConfig(c.cmdTriggerResume)).SetUsage("trigger resume <name...>")))
	app.AddCommand(orpheus.NewCommand("rules", "List the project rules the agent follows (AGENTS.md, CLAUDE.md, ...)").SetHandler(c.withConfig(c.cmdRules)))
	app.AddCommand(orpheus.NewCommand("skill", "List installed skills").SetHandler(c.withConfig(c.cmdSkills)).
		AddSubcommand(orpheus.NewCommand("install", "Install a skill from a directory with a SKILL.md").SetHandler(c.withConfig(c.cmdSkillInstall)).SetUsage("skill install <dir>")).
		AddSubcommand(orpheus.NewCommand("rm", "Remove installed skills").SetHandler(c.withConfig(c.cmdSkillRemove)).SetUsage("skill rm <name...>")))
	app.AddCommand(orpheus.NewCommand("sandbox", "Run a command isolated, as scripts and tools will be").SetHandler(c.withConfig(c.cmdSandbox)).
		SetUsage("sandbox [--mount DIR[:/dest][:rw]]... [--allow-domain D]... [--dir /path] -- command [args...]").
		AddStringSliceFlag("mount", "", nil, "grant a host directory, at its own path unless :/dest is given; read-only unless :rw").
		AddStringSliceFlag("allow-domain", "", nil, "let the command reach this domain (*.example.com for its subdomains), through the egress proxy; none, no network").
		AddFlag("dir", "", "", "working directory inside the sandbox"))
	app.SetDefaultCommand("chat")
	return app
}

// ErrUsage marks a mistake in how the command was invoked.
const ErrUsage errors.ErrorCode = "USAGE"

func (c *cli) agent(ctx *orpheus.Context) (agent.Agent, error) {
	c.acceptEdits = c.flagBool(ctx, "accept-edits")
	if c.roleName == "" {
		c.roleName = a2aProfile(ctx) // --profile, else default: what triggers made here run with
	}
	c.noApprovals = c.flagBool(ctx, "no-approvals")
	if err := c.chooseProfile(ctx); err != nil {
		return nil, err
	}
	p, err := c.runModel(ctx)
	if err != nil {
		return nil, err
	}
	mem, err := c.memory(ctx)
	if err != nil {
		return nil, err
	}
	cat, nb, err := c.ownerState(ctx)
	if err != nil {
		return nil, err
	}
	c.notes = nb
	tools, err := c.tools(ctx, mem, cat)
	if err != nil {
		return nil, err
	}
	model := retry.Wrap(p, retry.Config{})
	if tools, err = c.finishTools(ctx, model, tools); err != nil {
		return nil, err
	}
	c.finalTools, c.live, c.catalog = tools, agent.NewLiveRegistry(tools), cat
	system, instructions, err := c.prompts()
	if err != nil {
		return nil, err
	}
	return agent.New(agent.Config{
		Provider:      model,
		Tools:         c.live,
		Memory:        mem,
		Approve:       c.decide,
		System:        system,
		Instructions:  instructions,
		Context:       c.runContext(nb, !c.restricted()),
		MaxTurns:      c.maxTurns(ctx),
		MaxCost:       c.maxCost,
		ContextWindow: c.flagInt(ctx, "context-window"),
		Compaction:    agent.Compaction{Summarize: c.flagBool(ctx, "summarize")},
		KeepImages:    c.keepImages(ctx),
		// The model is told when it goes round in circles: small models
		// need it, and the CLI runs them.
		Loop:      agent.Loop{Action: agent.LoopWarn},
		Observers: c.observers(ctx.Context()),
	})
}

// prompts are the run's system prompt (the base, with what a chat and an
// /automate run add) and the operator's instructions. The skills come as
// a context block, so one installed during a session is listed.
func (c *cli) prompts() (system, instructions string, err error) {
	system = c.automateSystem(c.chatSystem(agent.DefaultSystem))
	instructions, err = c.instructions()
	return system, instructions, err
}

// withWorkspace adds the flag granting directories to the file tools.
func withWorkspace(cmd *orpheus.Command) *orpheus.Command {
	return cmd.AddStringSliceFlag("workspace", "w", nil,
		"grant a directory to the file tools (read_file, list_directory, glob, grep); repeatable, the first is the base for relative paths")
}

// tools grants the built-in tools and, with --workspace, the file tools.
func (c *cli) tools(ctx *orpheus.Context, mem agent.Memory, cat *skills.Catalog) (agent.Registry, error) {
	if c.flagBool(ctx, "no-tools") {
		return nil, nil
	}
	reg, err := builtinTools()
	if err != nil {
		return nil, err
	}
	if reg, err = reg.With(append(c.extraTools(mem, cat), c.installTools(ctx, cat)...)...); err != nil {
		return nil, err
	}
	remote, err := c.remoteTools(ctx)
	if err != nil {
		return nil, err
	}
	reg = c.addEach(reg, remote)
	dirs, err := c.workspaceDirs(ctx)
	if err != nil {
		return nil, err
	}
	ws, err := files.New(files.Config{Dirs: dirs, Ask: c.askAccess, ConfirmProtected: c.confirmProtected, Exclude: ownStateExcluded,
		Protected: protectedPatterns, ProtectedFiles: c.protectedFiles(ctx)})
	if err != nil {
		return nil, err
	}
	c.readTools, c.ws = ws.ReadTools(), ws
	sh, err := c.bashTool(ctx, dirs)
	if err != nil {
		return nil, err
	}
	return reg.With(append(append(c.readTools, ws.WriteTools()...), sh...)...)
}

// protectedPatterns are the SDK's protected paths and ringil's own
// configuration, wherever a project keeps one.
var protectedPatterns = append(append([]string(nil), files.DefaultProtected...), "ringil.yaml")

// workspaceDirs are the directories granted to the file tools: those of
// --workspace, or the default ones, and under serve the documents of the
// run's chat.
func (c *cli) workspaceDirs(ctx *orpheus.Context) ([]string, error) {
	dirs := ctx.GetFlagStringSlice("workspace")
	if len(dirs) == 0 {
		var err error
		if dirs, err = c.defaultDirs(); err != nil {
			return nil, err
		}
	}
	if c.inbox != "" {
		dirs = append(dirs, c.inbox)
	}
	return dirs, nil
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

// finishTools adds delegating over the tools, then keeps the role's. A
// restricted role has no workers: they would have every tool.
func (c *cli) finishTools(ctx *orpheus.Context, model agent.Provider, tools agent.Registry) (agent.Registry, error) {
	if c.restricted() {
		return c.onlyTools(tools)
	}
	tools, err := c.withDelegate(ctx, model, tools)
	if err != nil {
		return nil, err
	}
	return c.onlyTools(tools)
}

// ownerState is the user's skills and notebooks; none for a restricted
// role, whose runs must not read or write what is the user's.
func (c *cli) ownerState(ctx *orpheus.Context) (*skills.Catalog, *notes.Notes, error) {
	if c.restricted() {
		return nil, nil, nil
	}
	cat, err := c.skillCatalog(ctx)
	if err != nil {
		return nil, nil, err
	}
	nb, err := c.notebooks(ctx)
	return cat, nb, err
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
func (c *cli) runContext(nb *notes.Notes, withRules bool) func(context.Context) ([]agent.ContextBlock, error) {
	cfg, ok := c.rulesConfig()
	ok = ok && withRules
	return func(ctx context.Context) ([]agent.ContextBlock, error) {
		// Each block comes back to the model only when it changes: a new
		// note does not send the rules again, a new day only the date.
		blocks := []agent.ContextBlock{{Name: "environment", Text: c.environment()}}
		if ok {
			blocks = append(blocks, agent.ContextBlock{Name: "rules", Text: rules.Render(rules.Load(cfg))})
		}
		if c.catalog != nil {
			blocks = append(blocks, agent.ContextBlock{Name: "skills", Text: c.catalog.Instructions()})
		}
		if nb != nil {
			n, err := nb.Context(ctx)
			if err != nil {
				return blocks, err
			}
			blocks = append(blocks, agent.ContextBlock{Name: "notes", Text: n})
		}
		return blocks, nil
	}
}

// environment is where and when the agent works, as Claude Code tells its
// model (found 21/11: a note was dated 2025, the model had no date).
func (c *cli) environment() string {
	var b strings.Builder
	b.WriteString("<environment>\n")
	if cwd, err := c.cwd(); err == nil {
		b.WriteString("Working directory: ")
		b.WriteString(cwd)
		b.WriteByte('\n')
	}
	b.WriteString("Platform: ")
	b.WriteString(runtime.GOOS)
	b.WriteString("\nToday's date: ")
	b.WriteString(time.Now().Format("2006-01-02 (Monday)"))
	b.WriteString("\n</environment>")
	return b.String()
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
	if c.offersSkills(cat) {
		extra = append(extra, skills.Tools(cat, c.skillOptions())...)
	}
	if dir, err := c.skillsDir(); err == nil && !c.restricted() {
		extra = append(extra, skills.CreateTool(dir, nil)) // destructive: asked, showing what it writes
		if !c.inTrigger {                                  // a trigger's runs see no other automation
			extra = append(extra, trigger.ListTool(c.triggerStore), trigger.DeleteTool(c.triggerStore, func(string) { c.reloadTriggers() }))
		}
		if c.automating {
			extra = append(extra, c.createTrigger())
		}
	}
	return append(extra, c.laterTools()...)
}

// offersSkills: there are skills, or the owner may install one during
// the session.
func (c *cli) offersSkills(cat *skills.Catalog) bool {
	return cat != nil && (len(cat.List()) > 0 || !c.restricted())
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

// decide is approve as the agent's approval hook.
func (c *cli) decide(ctx context.Context, call agent.ToolCall, spec agent.ToolSpec) (agent.Decision, error) {
	p := c.policy(ctx)
	if c.notesReadInPlan(ctx, p, call, spec) {
		return agent.Allow(), nil
	}
	if p.Effect(call, spec) == agent.EffectDeny { // a deny rule, or plan mode: before anything else allows it
		return p.Approve(ctx, call, spec)
	}
	if allow, decided := c.triggerAllowed(call.Name); decided { // a trigger's run may only what it was allowed
		if (allow && !replacesSkill(call) && !p.AskedByRule(call, spec)) || p.Effect(call, spec) == agent.EffectAllow {
			return agent.Allow(), nil // allowed when the trigger was made (unless a rule wants a person: no one is here), or what runs without asking
		}
		return agent.Deny("This automation was not allowed this call when it was set up, and no one can be asked now."), nil
	}
	if c.notesFree(ctx, p, call, spec) {
		return agent.Allow(), nil
	}
	return p.Approve(ctx, call, spec)
}

// notesReadInPlan: plan mode reads, and the notes tool's calls that change
// nothing (a view, the session's scratchpad) are reading; a deny or ask
// rule on them still holds.
func (c *cli) notesReadInPlan(ctx context.Context, p *agent.Policy, call agent.ToolCall, spec agent.ToolSpec) bool {
	if call.Name != notes.DefaultName || c.mode() != agent.ModePlan || c.notesMustAsk(ctx, call) || p.AskedByRule(call, spec) {
		return false
	}
	return (&agent.Policy{Rules: p.Rules}).Effect(call, spec) != agent.EffectDeny // the rules alone, without the mode
}

func (c *cli) notesFree(ctx context.Context, p *agent.Policy, call agent.ToolCall, spec agent.ToolSpec) bool {
	return call.Name == notes.DefaultName && !p.AskedByRule(call, spec) && !c.notesMustAsk(ctx, call)
}

// askPerson asks whoever decides this run: the API's caller under serve
// (a chat's owner), else the terminal.
func (c *cli) askPerson(ctx context.Context, call agent.ToolCall, spec agent.ToolSpec) (agent.Answer, error) {
	if c.server != nil {
		ok, err := c.server.Ask(ctx, call)
		return agent.Answer{Allow: ok}, err
	}
	return c.askTerminal(ctx, call, spec)
}

// askTerminal asks in the terminal: yes, no, always for this session, or
// always, kept in ringil.yaml.
func (c *cli) askTerminal(ctx context.Context, call agent.ToolCall, spec agent.ToolSpec) (agent.Answer, error) {
	c.holdTerminal(ctx)
	defer c.askMu.Unlock()
	if checking(ctx) { // the user may be typing: a line meant for the chat must not answer it
		c.note("a check may not ask in the terminal: " + call.Name + " refused")
		return agent.Answer{}, nil
	}
	c.showCall(call)
	options := "[y/N]"
	if rule, ok := agent.RuleFor(call, spec); ok {
		options = "[y/N/a=always " + rule.String() + " this session/p=always, kept in ringil.yaml]"
	}
	answer, err := c.ask("\n  ? " + asker(ctx) + " wants to run " + call.Name + " " + short(string(call.Input)) + " — allow? " + options + " ")
	return answered(answer), err
}

// answered reads a yes, no, always (this session) or always (the project).
func answered(answer string) agent.Answer {
	switch answer {
	case "a", "always", "sempre":
		return agent.Answer{Allow: true, Remember: agent.RememberSession}
	case "p", "project", "progetto":
		return agent.Answer{Allow: true, Remember: agent.RememberProject}
	}
	return agent.Answer{Allow: yes(answer)}
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

// confirmProtected asks before any write to a protected file (other
// agents' rules, editor and CI settings, .git), even with --accept-edits;
// under serve, the API's caller decides.
func (c *cli) confirmProtected(ctx context.Context, path string) (bool, error) {
	if allow, decided := c.triggerAllowed("write_protected_file"); decided {
		return allow, nil
	}
	if c.unasked() {
		return true, nil
	}
	if c.server != nil {
		input, err := json.Marshal(map[string]string{"path": path})
		if err != nil {
			return false, err
		}
		return c.server.Ask(ctx, agent.ToolCall{ID: "protected-" + rand.Text()[:8], Name: "write_protected_file", Input: input})
	}
	c.holdTerminal(ctx)
	defer c.askMu.Unlock()
	return c.confirm("\n  ! " + asker(ctx) + " wants to change " + clean(path) + ", which holds your own rules or settings — allow this change? [y/N] ")
}

// unasked: --no-approvals, and not in a chat's run, whose Bridge decides
// by who started it (an owner's runs go unasked, the others' are refused).
func (c *cli) unasked() bool {
	return c.noApprovals && !c.chatRun()
}

// chats are the channels serve bridges: their runs' approvals are the
// Bridge's to decide, and they talk in chat style.
var chats = []string{"telegram"}

// chatRun: this run continues a chat's conversation.
func (c *cli) chatRun() bool {
	for _, name := range chats {
		if channel.Owns(name, c.runConversation) {
			return true
		}
	}
	return false
}

// notesMustAsk reports whether a notes call changes something its scope
// wants confirmed (project notes or deletions).
func (c *cli) notesMustAsk(ctx context.Context, call agent.ToolCall) bool {
	if _, worker := delegate.Current(ctx); worker {
		// A worker writes only its own scratchpad (the project is read
		// only for it): only deleting is confirmed.
		var in struct {
			Command string `json:"command"`
		}
		return json.Unmarshal(call.Input, &in) != nil || in.Command == "delete" // what cannot be read is asked
	}
	return c.notes != nil && c.notes.NeedsApproval(call)
}

// ownStateExcluded are the file tools' exclusions plus the agent's own
// state: its triggers, skills, configuration, tokens and chat records change
// only through their tools and commands, which ask.
var ownStateExcluded = append(slices.Clone(files.DefaultExclude),
	"ringil/triggers", "ringil/skills", "ringil/ringil.yaml", "ringil/serve-token",
	"ringil/telegram-phones.json", "ringil/telegram-sessions.json", "ringil/instructions.md", "ringil/secrets")

// showCall shows, before asking, what a call would do in full when a
// line cannot: a workflow's plan, a skill's instructions.
func (c *cli) showCall(call agent.ToolCall) {
	switch call.Name {
	case workflow.ToolName:
		c.showPlan(call)
	case "create_skill":
		c.showSkill(call)
	case "create_trigger":
		c.showTrigger(call)
	}
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
// The lines come from one goroutine, so that the chat's prompt can also
// wait for a check come due (lines) while it waits for the user.
func (c *cli) readLine() (string, bool) {
	line, ok := <-c.lines()
	return line, ok
}

// lines starts reading the terminal, once; the channel closes at the end
// of input.
func (c *cli) lines() <-chan string {
	c.readOnce.Do(func() {
		c.input = make(chan string)
		c.scanner = bufio.NewScanner(c.in)
		c.scanner.Buffer(make([]byte, 0, 4096), maxLine)
		go func() {
			defer close(c.input)
			for c.scanner.Scan() {
				c.input <- c.scanner.Text()
			}
		}()
	})
	return c.input
}

func (c *cli) provider(ctx *orpheus.Context) (agent.Provider, error) {
	res, err := c.resolver(ctx)
	if err != nil {
		return nil, err
	}
	list, err := c.runModels(ctx, res)
	if err != nil {
		return nil, err
	}
	return c.chain(ctx, res, list)
}

// newModel is the provider for model at ep: Anthropic's API, ChatGPT's,
// or an OpenAI-compatible one, with ep's key and no other.
func (c *cli) newModel(ctx *orpheus.Context, res secrets.Resolver, ep endpoint, model string) (agent.Provider, error) {
	switch ep.kind {
	case providerChatGPT:
		p, err := c.newChatGPT(ctx, model, ep.base)
		if err != nil {
			return nil, err
		}
		return c.priced(ctx.Context(), p, ep, model), nil
	case providerAnthropic:
		p, err := c.newAnthropic(anthropic.Config{BaseURL: ep.base, Model: model, KeyFunc: keyFunc(res, ep.keyName), AllowHTTP: c.flagBool(ctx, "allow-http")})
		if err != nil {
			return nil, err
		}
		return c.priced(ctx.Context(), p, ep, model), nil
	}
	p, err := c.newProvider(openai.Config{
		BaseURL:    ep.base,
		Model:      model,
		KeyFunc:    keyFunc(res, ep.keyName),
		AllowHTTP:  c.flagBool(ctx, "allow-http"),
		Images:     c.images(ctx.Context(), res),
		StatesCost: statesCost(ep.base),
		Documents:  readsPDF(ep.base),
	})
	if err != nil {
		return nil, err
	}
	return c.priced(ctx.Context(), p, ep, model), nil
}

// keyFunc reads a provider's key from name; nil, no key, for "". With a
// secrets store the key comes from the store alone: one planted in the
// environment for a provider the store has no key for is refused (audit
// 23/11: the conversations went to the planter's account).
func keyFunc(res secrets.Resolver, name string) func(context.Context) (string, error) {
	if name == "" {
		return nil
	}
	return secrets.OptionalKeyFunc(storeOnly(res), name)
}

// storeOnly is res reading from the store alone when there is one: for
// what decides who ringil talks to or who talks to it (a provider's key,
// serve's token, the bot's token: audit 23/11).
func storeOnly(res secrets.Resolver) secrets.Resolver {
	if k, ok := res.(keyChain); ok && k.keys != nil {
		return sourced{Resolver: store.StoreOnly(k.base, k.keys), record: k.record}
	}
	return res
}

// providerSetting is a provider's name: the flag, the environment's, the
// file's. With a secrets store the environment's is refused: a name
// planted there would send the conversations to a provider with no key to
// guard it, Ollama's port on a shared host (audit 23/11).
func (c *cli) providerSetting(ctx *orpheus.Context, res secrets.Resolver, flag, env, file string) (string, error) {
	if flag != "" && ctx.GetGlobalFlagString(flag) != "" {
		return ctx.GetGlobalFlagString(flag), nil
	}
	v, found, err := secrets.Lookup(ctx.Context(), settingsOf(res), env)
	switch {
	case err != nil:
		return "", err
	case found && c.conf().Secrets.Store != "":
		return "", errors.New(ErrUsage, env+" is in the environment (or the env file), and with a secrets store the provider is ringil.yaml's (or --provider): remove it")
	case found:
		return v, nil
	}
	return file, nil
}

// resolver is where settings and secrets come from: the environment first,
// then the --env-file if given, then the store of ringil.yaml's secrets
// section. The key is never taken from a flag, which would leave it in
// the shell history and the process list.
func (c *cli) resolver(ctx *orpheus.Context) (secrets.Resolver, error) {
	base, err := c.baseResolver(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := c.keyStore(ctx.Context(), base)
	if err != nil {
		return nil, err
	}
	chain := keyChain{Resolver: store.Resolver(base, keys), base: base, keys: keys, record: c.settings.keySource}
	if keys != nil {
		chain.Resolver = sourced{Resolver: chain.Resolver, record: chain.record}
	}
	return chain, nil
}

// sourced records where each key it gives was read from, as the store's
// resolver tells it (one read: a second could answer otherwise).
type sourced struct {
	secrets.Resolver
	record func(name, source string)
}

type sourceTeller interface {
	LookupSource(ctx context.Context, name string) (string, bool, bool, error)
}

func (s sourced) Lookup(ctx context.Context, name string) (string, bool, error) {
	teller, ok := s.Resolver.(sourceTeller)
	if !ok {
		return s.Resolver.Lookup(ctx, name)
	}
	value, found, fromStore, err := teller.LookupSource(ctx, name)
	if err == nil && found {
		s.record(name, map[bool]string{true: "store", false: "environment"}[fromStore])
	}
	return value, found, err
}

// keyChain resolves keys through the store first (a name in the store and
// in the environment is an error, never a silent choice: 23/11); settingsOf gives what
// resolves settings (the provider, the model, RINGIL_IMAGES): the
// environment and the --env-file alone, so a store that cannot answer
// stops only what needs a key, and an entry in it never changes a
// setting of ringil.yaml (audit 22/11).
type keyChain struct {
	secrets.Resolver
	base   secrets.Resolver
	keys   secrets.Store // the store, nil for none
	record func(name, source string)
}

func settingsOf(res secrets.Resolver) secrets.Resolver {
	if k, ok := res.(keyChain); ok {
		return k.base
	}
	return res
}

// baseResolver is the environment, then the --env-file.
func (c *cli) baseResolver(ctx *orpheus.Context) (secrets.Resolver, error) {
	env := secrets.Resolver(lookupEnv{c.getenv})
	path := ctx.GetGlobalFlagString("env-file")
	if path == "" {
		return c.withKeyFile(env)
	}
	file, err := secrets.EnvFile(path)
	if err != nil {
		return nil, err
	}
	return c.withKeyFile(secrets.Chain(env, file)) // the passphrase ringil made for its own store, last
}

// setting returns a flag, or else the named value from the resolver, or
// else ringil.yaml's.
func (c *cli) setting(ctx *orpheus.Context, res secrets.Resolver, flag, name string) (string, error) {
	if v := ctx.GetGlobalFlagString(flag); v != "" {
		return v, nil
	}
	v, found, err := secrets.Lookup(ctx.Context(), settingsOf(res), name)
	if err != nil || found {
		return v, err
	}
	return c.conf().stringOf(flag), nil
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
	case errors.HasCode(err, chatgpt.ErrSignedOut):
		return "not signed in to ChatGPT, or the sign-in ended: run `ringil login chatgpt`"
	case errors.HasCode(err, agent.ErrProviderAuth):
		return "the API key was refused or is missing"
	case errors.HasCode(err, secrets.ErrMissing), errors.HasCode(err, secrets.ErrSource), errors.HasCode(err, secrets.ErrValue):
		return "secrets: " + err.Error()
	case errors.HasCode(err, agent.ErrProviderConfig):
		return "provider configuration: " + err.Error()
	}
	return err.Error()
}
