// bash.go: the bash tool
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package bash is the agent's shell: one tool, bash, that runs a command
// line in the working directory and returns its output and exit status.
//
// Commands run in the sandbox and need no one's yes there. The sandbox
// lets a command read the system, the working directory and what the host
// grants (a toolchain, the user's git settings), and write only the
// working directory and a private temporary directory and home, both gone
// when the command ends. Inside the working directory the file tools'
// secrets stay hidden and the files they protect (agents' rules, editor
// settings, a repository's hooks and configuration) stay read-only:
// changing those goes through edit_file, which asks. The network is off,
// or, with allowed domains or a way to ask, reaches only those domains
// through an egress proxy (package egress).
//
// Two things need the user's yes, through Config.Confirm (none refuses):
//   - a command run outside the sandbox, which the model asks for when a
//     command cannot work inside it, and every command where there is no
//     sandbox;
//   - a dangerous command, sandbox or not: deleting a tree, discarding
//     uncommitted work, rewriting history, pushing (see Dangerous).
//
// Git has no tool of its own: it is a command like any other. Reading the
// repository and committing work in the sandbox; pushing asks.
package bash

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/egress"
	"github.com/agilira/ringil-agent-sdk/sandbox"
)

// Error codes.
const (
	// ErrConfig: the configuration is invalid.
	ErrConfig errors.ErrorCode = "BASH_CONFIG"
	// ErrRefused: the user, or the absence of a way to ask, refused the
	// command.
	ErrRefused errors.ErrorCode = "BASH_REFUSED"
	// ErrInput: the model's arguments are unusable.
	ErrInput errors.ErrorCode = "BASH_INPUT"
	// ErrRun: the command could not be started.
	ErrRun errors.ErrorCode = "BASH_RUN"
)

// Defaults.
const (
	DefaultTimeout = 2 * time.Minute
	MaxTimeout     = 10 * time.Minute
	// DefaultMaxOutput bounds the output returned to the model, in bytes:
	// the start and the end are kept.
	DefaultMaxOutput = 30000
)

// Reason says why a command needs the user's yes.
type Reason string

const (
	// Unsandboxed: the command runs outside the sandbox, with the user's
	// rights.
	Unsandboxed Reason = "unsandboxed"
	// Destructive: the command is dangerous (Request.Why says how).
	Destructive Reason = "dangerous"
)

// Request is what the user is asked to allow.
type Request struct {
	Command string
	Dir     string
	Reason  Reason
	// Why lists what makes the command dangerous, when it is.
	Why []string
	// Explanation is the model's reason for running outside the sandbox.
	Explanation string
}

// Network is the sandboxed commands' network. With no domains and no Ask,
// there is none.
type Network struct {
	// Allow are the domains reachable without asking ("pypi.org",
	// "*.githubusercontent.com").
	Allow []string
	// Ask asks the user about any other domain, once per tool.
	Ask func(ctx context.Context, host string) (bool, error)
	// Ports are the ports reachable; default 80 and 443.
	Ports []int
}

// Config configures the tool.
type Config struct {
	// Dir is the working directory: commands start there, and it is the
	// only host directory a sandboxed command can write.
	Dir string
	// Sandbox runs sandboxed commands; nil uses sandbox.New. Where there
	// is none, every command needs the user's yes.
	Sandbox sandbox.Sandbox
	// Exclude are the secrets hidden in Dir and the Read directories:
	// files.DefaultExclude when nil.
	Exclude []string
	// Protected are the paths kept read-only in Dir: files.DefaultProtected
	// when nil. A repository stays usable: of .git only the hooks, the
	// configuration and info/ are read-only, so committing works.
	Protected []string
	// Read are host files and directories a sandboxed command may read,
	// at their own paths: toolchains, ~/.gitconfig. Secrets in them are
	// hidden as in Dir. ReadDefaults suggests the usual ones.
	Read []string
	// Home is HOME inside the sandbox, a private empty directory where Read
	// paths below it appear; default the user's home path. It is not used
	// when Dir is the home or above it.
	Home string
	// Path is PATH inside the sandbox; default the host's PATH entries the
	// sandbox can see.
	Path []string
	// Env are extra variables for sandboxed commands.
	Env     map[string]string
	Network Network
	// Confirm asks the user to allow a command; nil refuses every command
	// that needs it.
	Confirm func(ctx context.Context, req Request) (bool, error)
	// Never are host paths no sandbox may be granted: the agent's own
	// settings and sessions.
	Never []string
	// Limits are the sandbox's per-process limits; zero values take the
	// tool's defaults (more processes and CPU time than the sandbox's own,
	// for builds and test runs).
	Limits    sandbox.Limits
	MaxOutput int
}

type tool struct {
	cfg   Config
	box   sandbox.Sandbox
	noBox error // why there is no sandbox
	proxy *egress.Proxy
	home  string
	path  []string

	mu     sync.Mutex
	hidden map[string][]string // Read directory → its hidden paths, walked once
	runMu  sync.Mutex          // one sandboxed command at a time
}

type input struct {
	Command     string `json:"command" desc:"The command line, run by bash in the working directory."`
	Timeout     int    `json:"timeout,omitempty" desc:"Seconds before the command is stopped; default 120, at most 600." min:"1" max:"600"`
	Unsandboxed bool   `json:"unsandboxed,omitempty" desc:"Run outside the sandbox, with the user's rights. Only when the command failed because of the sandbox (no network to a domain, a path it cannot write); the user must approve."`
	Reason      string `json:"reason,omitempty" desc:"With unsandboxed: why the sandbox is not enough, shown to the user."`
}

// New returns the bash tool.
func New(cfg Config) (agent.Tool, error) {
	t, err := newTool(cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewTool("bash", t.description(), t.call, agent.Destructive(), agent.OpenWorld()), nil
}

func newTool(cfg Config) (*tool, error) {
	dir, err := workDir(cfg.Dir)
	if err != nil {
		return nil, err
	}
	cfg.Dir = dir
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = DefaultMaxOutput
	}
	t := &tool{cfg: cfg, box: cfg.Sandbox, hidden: map[string][]string{}}
	if t.box == nil {
		t.box, t.noBox = sandbox.New()
	}
	if len(cfg.Network.Allow) > 0 || cfg.Network.Ask != nil {
		t.proxy, err = egress.New(egress.Config{Allow: cfg.Network.Allow, Ask: cfg.Network.Ask, Ports: cfg.Network.Ports})
		if err != nil {
			return nil, err
		}
	}
	if err := t.settle(); err != nil {
		return nil, err
	}
	return t, nil
}

// workDir resolves the working directory, which must exist.
func workDir(dir string) (string, error) {
	if dir == "" {
		return "", errors.New(ErrConfig, "the working directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", errors.Wrap(err, ErrConfig, "cannot resolve "+dir)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return "", errors.New(ErrConfig, "the working directory must exist: "+abs)
	}
	return abs, nil
}

func (t *tool) description() string {
	d := "Run a bash command line in " + t.cfg.Dir + " and return its output and exit status. " +
		"Use it for builds, tests, git and other programs; read, write and edit files with the file tools instead of cat, sed or echo. "
	if t.noBox != nil {
		return d + "There is no sandbox here: the user approves every command."
	}
	d += "Commands run in a sandbox without asking: they can write only the working directory and temporary files, " +
		"protected files there (agents' rules, editor settings, .git hooks and config) are read-only, and secrets are hidden. "
	if t.proxy == nil {
		d += "There is no network. "
	} else {
		d += "The network reaches only allowed domains, through a proxy (HTTP_PROXY is set); others are refused or asked about. "
	}
	return d + "Deleting trees, discarding changes, rewriting history and pushing need the user's approval. " +
		"Set unsandboxed only when a command failed because of the sandbox."
}

func (t *tool) call(ctx context.Context, in input) (agent.ToolOutput, error) {
	command := strings.TrimSpace(in.Command)
	if command == "" || strings.ContainsRune(command, 0) {
		return agent.ToolOutput{}, agent.ToolError(ErrInput, "The command is empty.")
	}
	timeout := DefaultTimeout
	if in.Timeout > 0 {
		timeout = min(time.Duration(in.Timeout)*time.Second, MaxTimeout)
	}
	outside := in.Unsandboxed || t.noBox != nil
	if err := t.approve(ctx, command, outside, in.Reason); err != nil {
		return agent.ToolOutput{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &capture{max: t.cfg.MaxOutput}
	run := t.runSandboxed
	if outside {
		run = t.runHost
	}
	return t.result(ctx, out, run(ctx, command, out), timeout)
}

// approve asks the user about a command outside the sandbox or a
// dangerous one; others need no one's yes.
func (t *tool) approve(ctx context.Context, command string, outside bool, explanation string) error {
	why := Dangerous(command)
	if !outside && len(why) == 0 {
		return nil
	}
	req := Request{Command: command, Dir: t.cfg.Dir, Reason: Destructive, Why: why, Explanation: explanation}
	if outside {
		req.Reason = Unsandboxed
	}
	return t.confirm(ctx, req)
}

func (t *tool) confirm(ctx context.Context, req Request) error {
	if t.cfg.Confirm == nil {
		return agent.ToolError(ErrRefused, refusal(req, "there is no one to ask here"))
	}
	ok, err := t.cfg.Confirm(ctx, req)
	if err != nil {
		return err
	}
	if !ok {
		return agent.ToolError(ErrRefused, refusal(req, "the user refused"))
	}
	return nil
}

func refusal(req Request, who string) string {
	if req.Reason == Unsandboxed {
		return "Not run: running outside the sandbox needs the user's approval, and " + who + "."
	}
	return "Not run: " + strings.Join(req.Why, "; ") + ". That needs the user's approval, and " + who + "."
}

// result reports the exit status and the output, with hints when the
// sandbox explains a failure.
func (t *tool) result(ctx context.Context, out *capture, err error, timeout time.Duration) (agent.ToolOutput, error) {
	status, err := exitStatus(err)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	text := out.String()
	var b strings.Builder
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		b.WriteString("Stopped after " + timeout.String() + " (timeout).\n")
	case status != 0:
		b.WriteString("Exit status " + strconv.Itoa(status) + ".\n")
	}
	b.WriteString(text)
	b.WriteString(hints(text, status != 0, out.blocked()))
	if len(out.notes) > 0 {
		b.WriteString("\n[Sandbox: " + strings.Join(out.notes, "; ") + ".]")
	}
	if b.Len() == 0 {
		b.WriteString("(no output)")
	}
	return agent.ToolOutput{Content: b.String()}, nil
}

// hints explains failures the sandbox causes, so the model does not retry
// blindly.
func hints(out string, failed bool, blocked []string) string {
	var h []string
	if failed && (strings.Contains(out, "Read-only file system") || strings.Contains(out, "Operation not permitted")) {
		h = append(h, "a protected file or a path outside the working directory is read-only here; "+
			"change protected files with edit_file or write_file, which ask the user")
	}
	if len(blocked) > 0 {
		h = append(h, "the network refused: "+strings.Join(blocked, "; "))
	}
	if len(h) == 0 {
		return ""
	}
	return "\n[Sandbox: " + strings.Join(h, "; ") + ".]"
}

// capture keeps the start and the end of an output larger than max, and
// what the network refused meanwhile.
type capture struct {
	mu      sync.Mutex
	max     int
	head    []byte
	tail    []byte
	dropped int
	denied  []string
	notes   []string // what the repository checks undid
}

// maxDenied bounds the refusals reported for one command.
const maxDenied = 5

func (c *capture) refused(host, why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.denied {
		if d == why {
			return
		}
	}
	if len(c.denied) < maxDenied {
		c.denied = append(c.denied, why)
	}
}

func (c *capture) blocked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.denied...)
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if room := c.max/2 - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	c.tail = append(c.tail, p...)
	if extra := len(c.tail) - c.max/2; extra > 0 {
		c.dropped += extra
		c.tail = append(c.tail[:0], c.tail[extra:]...)
	}
	return n, nil
}

func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := string(c.head)
	if c.dropped > 0 {
		s += "\n[... " + strconv.Itoa(c.dropped) + " bytes of output left out ...]\n"
	}
	return strings.ToValidUTF8(s+string(c.tail), "�")
}
