// apphooks.go: the user's hooks, commands run at fixed points
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/trigger"
	"github.com/agilira/ringil-agent-sdk/workflow"
)

// Hooks are commands the user sets in ringil.yaml (or from the app) at
// fixed points, as Claude Code's are:
//
//	hooks:
//	  before_tool:
//	    - tools: [bash, write_file]   # none: every tool
//	      command: [/home/me/bin/check-call]
//	      timeout: 10s                # default 30s, at most 10m
//	      on_error: block             # a hook that fails: continue (default) or block
//
// Each gets the event as JSON on standard input. Exit 0 goes on; what it
// prints, as JSON, may change what goes on (before_run {"text"},
// before_tool {"input"}, after_tool {"content"}, before_automation
// {"detail"}, before_step {"task"}). Exit 2 blocks, its standard error the
// reason. Any other end (a crash, the timeout) follows on_error. after_*
// hooks are told, and change nothing. A hook runs as the user, with a
// clean environment (no keys): it is the user's own program, never the
// agent's (the agent cannot write ringil.yaml).
type hooksConfig struct {
	BeforeRun        []hookEntry `argus:"before_run"`
	AfterRun         []hookEntry `argus:"after_run"`
	BeforeTool       []hookEntry `argus:"before_tool"`
	AfterTool        []hookEntry `argus:"after_tool"`
	BeforeAutomation []hookEntry `argus:"before_automation"`
	AfterAutomation  []hookEntry `argus:"after_automation"`
	BeforeWorkflow   []hookEntry `argus:"before_workflow"`
	AfterWorkflow    []hookEntry `argus:"after_workflow"`
	BeforeStep       []hookEntry `argus:"before_step"`
	AfterStep        []hookEntry `argus:"after_step"`
}

type hookEntry struct {
	// Tools, for before_tool and after_tool, are the tools it is for;
	// none, every tool.
	Tools   []string `argus:"tools" json:"tools,omitempty"`
	Command []string `argus:"command" json:"command"`
	Timeout string   `argus:"timeout" json:"timeout,omitempty"`
	OnError string   `argus:"on_error" json:"on_error,omitempty"`
}

// Bounds of the hooks.
const (
	maxHooks       = 16
	defaultHookFor = 30 * time.Second
	maxHookFor     = 10 * time.Minute
	maxHookOutput  = 64 << 10
	maxHookReason  = 1 << 10
)

var hookTool = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// events are the hooks by event name, in ringil.yaml's order.
func (h hooksConfig) events() map[string][]hookEntry {
	return map[string][]hookEntry{"before_run": h.BeforeRun, "after_run": h.AfterRun, "before_tool": h.BeforeTool, "after_tool": h.AfterTool,
		"before_automation": h.BeforeAutomation, "after_automation": h.AfterAutomation, "before_workflow": h.BeforeWorkflow, "after_workflow": h.AfterWorkflow,
		"before_step": h.BeforeStep, "after_step": h.AfterStep}
}

func (h hooksConfig) check() error {
	for event, list := range h.events() {
		if len(list) > maxHooks {
			return errors.New(ErrUsage, "hooks."+event+": at most 16 hooks")
		}
		for i, e := range list {
			if err := e.check(event); err != nil {
				return errors.New(ErrUsage, "hooks."+event+"["+strconv.Itoa(i)+"]: "+err.Error())
			}
		}
	}
	return nil
}

func (e hookEntry) check(event string) error {
	if err := e.checkCommand(); err != nil {
		return err
	}
	switch {
	case !slices.Contains([]string{"", "continue", "block"}, e.OnError):
		return errors.New(ErrUsage, "on_error is continue or block")
	case len(e.Tools) > 0 && !strings.HasSuffix(event, "_tool"):
		return errors.New(ErrUsage, "tools is for before_tool and after_tool")
	}
	for _, t := range e.Tools {
		if !hookTool.MatchString(t) {
			return errors.New(ErrUsage, "not a tool's name: "+t)
		}
	}
	_, err := e.timeout()
	return err
}

// checkCommand: the command is a program by absolute path or by name in
// PATH, its arguments without a NUL.
func (e hookEntry) checkCommand() error {
	switch {
	case len(e.Command) == 0 || strings.TrimSpace(e.Command[0]) == "":
		return errors.New(ErrUsage, "a hook needs a command")
	case !filepath.IsAbs(e.Command[0]) && strings.ContainsAny(e.Command[0], `/\`):
		return errors.New(ErrUsage, "a hook's command is an absolute path or a program's name found in PATH, not "+e.Command[0])
	}
	for _, a := range e.Command {
		if strings.ContainsRune(a, 0) {
			return errors.New(ErrUsage, "a hook's command holds a NUL")
		}
	}
	return nil
}

func (e hookEntry) timeout() (time.Duration, error) {
	if e.Timeout == "" {
		return defaultHookFor, nil
	}
	d, err := time.ParseDuration(e.Timeout)
	if err != nil || d <= 0 || d > maxHookFor {
		return 0, errors.New(ErrUsage, "a hook's timeout is from 1s to 10m, such as 30s")
	}
	return d, nil
}

func (e hookEntry) forTool(name string) bool {
	return len(e.Tools) == 0 || slices.Contains(e.Tools, name)
}

// hookEvent is what a hook reads on standard input.
type hookEvent struct {
	Event        string          `json:"event"`
	Profile      string          `json:"profile,omitempty"`
	Conversation string          `json:"conversation,omitempty"`
	Text         string          `json:"text,omitempty"`
	Tool         string          `json:"tool,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Result       *hookResult     `json:"result,omitempty"`
	Trigger      string          `json:"trigger,omitempty"`
	Kind         string          `json:"kind,omitempty"`
	Detail       string          `json:"detail,omitempty"`
	ByHand       bool            `json:"by_hand,omitempty"`
	Workflow     string          `json:"workflow_run,omitempty"`
	Step         *workflow.Step  `json:"step,omitempty"`
	Report       string          `json:"report,omitempty"`
	Stop         string          `json:"stop,omitempty"`
	Error        string          `json:"error,omitempty"`
}

type hookResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// hookReply is what a hook may print to change what goes on.
type hookReply struct {
	Text    *string         `json:"text"`
	Input   json.RawMessage `json:"input"`
	Content *string         `json:"content"`
	Detail  *string         `json:"detail"`
	Task    *string         `json:"task"`
}

// errHookBlocked: a hook exited 2, or failed with on_error: block.
const errHookBlocked errors.ErrorCode = "HOOK_BLOCKED"

// runHooks runs the hooks of an event in order, each told ev: the
// replies of those that went on, in order; the first block stops them.
func (c *cli) runHooks(ctx context.Context, list []hookEntry, ev hookEvent) ([]hookReply, error) {
	var replies []hookReply
	for _, e := range list {
		r, err := c.runHook(ctx, e, ev)
		if err != nil {
			return replies, err
		}
		replies = append(replies, r)
	}
	return replies, nil
}

// runHook runs one hook. Its failure (not a block) is told and, with
// on_error: block, blocks.
func (c *cli) runHook(ctx context.Context, e hookEntry, ev hookEvent) (hookReply, error) {
	limit, _ := e.timeout() // checked with the file
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, limit)
	defer cancel()
	in, err := json.Marshal(ev)
	if err != nil {
		return hookReply{}, err
	}
	cmd := exec.CommandContext(ctx, e.Command[0], e.Command[1:]...) // #nosec G204 -- the user's own hook, from ringil.yaml
	cmd.Stdin = bytes.NewReader(in)
	cmd.Env = hookEnv(ev.Event)
	cmd.Dir = c.hookDir()
	cmd.WaitDelay = outputWait
	hookProcess(cmd)
	out, errOut, err := runCommand(cmd)
	err = cleanWaitDelay(cmd, err)
	return c.interpretHook(parent, ctx, limit, e, ev, out, errOut, err)
}

func cleanWaitDelay(cmd *exec.Cmd, err error) error {
	if stderrors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		return nil
	}
	return err
}

func (c *cli) interpretHook(parent, ctx context.Context, limit time.Duration, e hookEntry, ev hookEvent, out, errOut []byte, err error) (hookReply, error) {
	var exit *exec.ExitError
	switch {
	case err == nil:
		var r hookReply
		if strings.TrimSpace(string(out)) != "" && json.Unmarshal(out, &r) != nil {
			return c.hookFailed(e, ev, "its output is not JSON")
		}
		return r, nil
	case stderrors.As(err, &exit) && exit.ExitCode() == 2:
		reason := strings.TrimSpace(string(errOut))
		if reason == "" {
			reason = "a hook of the user's refused it"
		}
		return hookReply{}, errors.New(errHookBlocked, reason).WithUserMessage(reason)
	case parent.Err() != nil:
		return hookReply{}, parent.Err()
	case ctx.Err() == context.DeadlineExceeded:
		return c.hookFailed(e, ev, "it took longer than "+limit.String())
	}
	return c.hookFailed(e, ev, oneLine(err.Error()+" "+string(errOut), 300))
}

// outputWait is how long the output of a hook that ended is waited for,
// past what it left running.
const outputWait = time.Second

// runCommand runs cmd to its end with its output on pipes of its own
// (Wait does not wait for them), kept within the bounds; then what it left
// running in its group is ended, so that a child holding the output open
// neither loses the hook's answer nor outlives it (audit 27/11).
func runCommand(cmd *exec.Cmd) (out, errOut []byte, err error) {
	or, ow, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	er, ew, err := os.Pipe()
	if err != nil {
		closeFiles(or, ow)
		return nil, nil, err
	}
	cmd.Stdout, cmd.Stderr = ow, ew
	if err := cmd.Start(); err != nil {
		closeFiles(or, ow, er, ew)
		return nil, nil, err
	}
	closeFiles(ow, ew) // the hook holds them now
	outc, errc := readCapped(or, maxHookOutput), readCapped(er, maxHookReason)
	err = cmd.Wait()
	endGroup(cmd)
	return collect(outc, or), collect(errc, er), err
}

// readCapped reads r to its end, keeping its first max bytes.
func readCapped(r *os.File, max int) <-chan []byte {
	got := make(chan []byte, 1)
	go func() {
		var b cappedBuffer
		b.max = max
		_, _ = io.Copy(&b, r)
		got <- b.Bytes()
	}()
	return got
}

// collect is what was read, once the writers are gone or a while has
// passed (one left that holds the pipe, on Windows): then the pipe is
// closed and what came is kept.
func collect(got <-chan []byte, r *os.File) []byte {
	defer closeFiles(r)
	select {
	case b := <-got:
		return b
	case <-time.After(outputWait):
		closeFiles(r)
		return <-got
	}
}

func closeFiles(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// hookFailed tells a hook that failed, and blocks when it is to.
func (c *cli) hookFailed(e hookEntry, ev hookEvent, why string) (hookReply, error) {
	msg := "hook " + ev.Event + " (" + clean(filepath.Base(e.Command[0])) + ") failed: " + clean(why)
	if c.log != nil {
		c.log.Warn(msg)
	}
	c.note(msg)
	if e.OnError == "block" {
		msg := "a hook of the user's failed, and blocks when it fails"
		return hookReply{}, errors.New(errHookBlocked, msg).WithUserMessage(msg)
	}
	return hookReply{}, nil
}

// hookDir is where hooks run: the configuration's directory.
func (c *cli) hookDir() string {
	if c.settings != nil && c.settings.path != "" {
		return filepath.Dir(c.settings.path)
	}
	return ""
}

// hookEnv is a hook's environment: where programs are and whose home,
// never the keys of the process.
func hookEnv(event string) []string {
	keep := []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "TZ", "USER"}
	if runtime.GOOS == "windows" {
		keep = append(keep, "SystemRoot", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "PATHEXT", "ComSpec")
	}
	env := []string{"RINGIL_HOOK_EVENT=" + event}
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// cappedBuffer keeps the first max bytes written to it, and takes the
// rest without keeping it, so that a chatty hook never blocks.
type cappedBuffer struct {
	bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// agentHooks are the agent's hooks from the user's: before and after a
// run, around each tool call. None set, none.
func (c *cli) agentHooks() agent.Hooks {
	h := c.conf().Hooks
	return agent.Hooks{BeforeRun: c.beforeRunHook(h.BeforeRun), BeforeTool: c.beforeToolHook(h.BeforeTool),
		AfterTool: c.afterToolHook(h.AfterTool), AfterRun: c.afterRunHook(h.AfterRun)}
}

// hookBase is an event of this run.
func (c *cli) hookBase(event string) hookEvent {
	return hookEvent{Event: event, Profile: c.roleName, Conversation: c.runConversation}
}

func (c *cli) beforeRunHook(list []hookEntry) func(context.Context, agent.Message) (agent.Message, error) {
	if len(list) == 0 {
		return nil
	}
	return func(ctx context.Context, in agent.Message) (agent.Message, error) {
		ev := c.hookBase("before_run")
		ev.Text = in.Text()
		for _, e := range list {
			r, err := c.runHook(ctx, e, ev)
			if err != nil {
				return in, err
			}
			if r.Text != nil {
				in = withText(in, *r.Text)
				ev.Text = in.Text()
			}
		}
		return in, nil
	}
}

// beforeToolHook runs the call's hooks in order, each told the input as
// the one before left it.
func (c *cli) beforeToolHook(list []hookEntry) func(context.Context, agent.ToolCall, agent.ToolSpec) (agent.ToolCall, error) {
	if len(list) == 0 {
		return nil
	}
	return func(ctx context.Context, call agent.ToolCall, _ agent.ToolSpec) (agent.ToolCall, error) {
		for _, e := range toolHooks(list, call.Name) {
			ev := c.hookBase("before_tool")
			ev.Tool, ev.Input = call.Name, call.Input
			r, err := c.runHook(ctx, e, ev)
			if err != nil {
				return call, err
			}
			if bad := applyHookInput(r.Input, &call); bad != "" {
				if _, ferr := c.hookFailed(e, ev, bad); ferr != nil {
					return call, ferr
				}
			}
		}
		return call, nil
	}
}

func applyHookInput(raw json.RawMessage, call *agent.ToolCall) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return "its input is not a JSON object"
	}
	call.Input = trimmed
	return ""
}

func (c *cli) afterToolHook(list []hookEntry) func(context.Context, agent.ToolCall, agent.ToolResult) agent.ToolResult {
	if len(list) == 0 {
		return nil
	}
	return func(ctx context.Context, call agent.ToolCall, res agent.ToolResult) agent.ToolResult {
		for _, e := range toolHooks(list, call.Name) {
			ev := c.hookBase("after_tool")
			ev.Tool, ev.Input, ev.Result = call.Name, call.Input, &hookResult{Content: res.Content, IsError: res.IsError}
			r, err := c.runHook(ctx, e, ev)
			if err != nil {
				return agent.ToolResult{CallID: res.CallID, Content: "The result was withheld: " + agent.ModelMessage(err), IsError: true}
			}
			if r.Content != nil {
				res.Content, res.Images = *r.Content, nil
			}
		}
		return res
	}
}

func (c *cli) afterRunHook(list []hookEntry) func(context.Context, *agent.Result, error) {
	if len(list) == 0 {
		return nil
	}
	return func(ctx context.Context, res *agent.Result, err error) {
		ev := c.hookBase("after_run")
		if res != nil {
			ev.Text, ev.Stop = res.Text, string(res.Stop)
		}
		if err != nil {
			ev.Error = describe(err)
		}
		_, _ = c.runHooks(ctx, list, ev) // told: it changes nothing
	}
}

// toolHooks are the hooks of list for tool.
func toolHooks(list []hookEntry, tool string) []hookEntry {
	var out []hookEntry
	for _, e := range list {
		if e.forTool(tool) {
			out = append(out, e)
		}
	}
	return out
}

// withText is in with its text replaced by text, its attachments kept.
func withText(in agent.Message, text string) agent.Message {
	out := agent.Message{Role: in.Role}
	out.Parts = append(out.Parts, agent.Part{Type: agent.PartText, Text: text})
	for _, p := range in.Parts {
		if p.Type != agent.PartText {
			out.Parts = append(out.Parts, p)
		}
	}
	return out
}

// automationHooks are the triggers' hooks from the user's, read at each
// run: the file in force decides.
func (c *cli) automationHooks() (func(context.Context, trigger.Trigger, trigger.Fired) (trigger.Fired, error), func(context.Context, trigger.Trigger, trigger.Fired, trigger.Outcome, error)) {
	before := func(ctx context.Context, t trigger.Trigger, f trigger.Fired) (trigger.Fired, error) {
		ev := hookEvent{Event: "before_automation", Trigger: t.Name, Kind: string(f.Kind), Detail: f.Detail, ByHand: f.ByHand, Profile: automationProfile(t)}
		for _, e := range c.conf().Hooks.BeforeAutomation {
			r, err := c.runHook(ctx, e, ev)
			if err != nil {
				return f, err
			}
			if r.Detail != nil {
				f.Detail = *r.Detail
				ev.Detail = f.Detail
			}
		}
		return f, nil
	}
	after := func(ctx context.Context, t trigger.Trigger, f trigger.Fired, out trigger.Outcome, err error) {
		ev := hookEvent{Event: "after_automation", Trigger: t.Name, Kind: string(f.Kind), ByHand: f.ByHand, Text: out.Text, Profile: automationProfile(t)}
		if err != nil {
			ev.Error = describe(err)
		}
		_, _ = c.runHooks(ctx, c.conf().Hooks.AfterAutomation, ev)
	}
	return before, after
}

// automationProfile is the agent of a trigger's run; none for one that
// runs a workflow (its steps name theirs).
func automationProfile(t trigger.Trigger) string {
	if t.Workflow != "" {
		return ""
	}
	return or(t.Profile, "default")
}

// workflowHooks gives a workflow runner the user's hooks, read at each
// run and step.
func (c *cli) workflowHooks(r *workflow.Runner) {
	r.BeforeRun = func(ctx context.Context, id string, _ workflow.Plan) error {
		_, err := c.runHooks(ctx, c.conf().Hooks.BeforeWorkflow, hookEvent{Event: "before_workflow", Workflow: id})
		return err
	}
	r.AfterRun = func(ctx context.Context, id string, res *workflow.Result, err error) {
		ev := hookEvent{Event: "after_workflow", Workflow: id}
		if res != nil {
			ev.Text = res.Output
		}
		if err != nil {
			ev.Error = describe(err)
		}
		_, _ = c.runHooks(ctx, c.conf().Hooks.AfterWorkflow, ev)
	}
	r.BeforeStep = func(ctx context.Context, id string, s workflow.Step) (workflow.Step, error) {
		ev := hookEvent{Event: "before_step", Workflow: id, Step: &s}
		for _, e := range c.conf().Hooks.BeforeStep {
			rep, err := c.runHook(ctx, e, ev)
			if err != nil {
				return s, err
			}
			if rep.Task != nil {
				s.Task = *rep.Task
				ev.Step = &s
			}
		}
		return s, nil
	}
	r.AfterStep = func(ctx context.Context, id string, s workflow.Step, report string, err error) {
		ev := hookEvent{Event: "after_step", Workflow: id, Step: &s, Report: report}
		if err != nil {
			ev.Error = describe(err)
		}
		_, _ = c.runHooks(ctx, c.conf().Hooks.AfterStep, ev)
	}
}

// HooksIO is the hooks as the app reads and writes them, by event.
type HooksIO struct {
	BeforeRun        []hookEntry `json:"before_run"`
	AfterRun         []hookEntry `json:"after_run"`
	BeforeTool       []hookEntry `json:"before_tool"`
	AfterTool        []hookEntry `json:"after_tool"`
	BeforeAutomation []hookEntry `json:"before_automation"`
	AfterAutomation  []hookEntry `json:"after_automation"`
	BeforeWorkflow   []hookEntry `json:"before_workflow"`
	AfterWorkflow    []hookEntry `json:"after_workflow"`
	BeforeStep       []hookEntry `json:"before_step"`
	AfterStep        []hookEntry `json:"after_step"`
}

func (h HooksIO) config() hooksConfig { return hooksConfig(h) }

// The hooks' API, mounted with the settings:
//
//	GET /v1/settings/hooks   the hooks, by event
//	PUT /v1/settings/hooks   all of them, by event (an event left out has none), checked as ringil.yaml is
func (c *cli) mountHooks(srv interface {
	Mount(pattern string, h http.Handler)
}) {
	srv.Mount("GET /v1/settings/hooks", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := HooksIO(c.conf().Hooks)
		for _, list := range []*[]hookEntry{&h.BeforeRun, &h.AfterRun, &h.BeforeTool, &h.AfterTool, &h.BeforeAutomation, &h.AfterAutomation,
			&h.BeforeWorkflow, &h.AfterWorkflow, &h.BeforeStep, &h.AfterStep} {
			if *list == nil {
				*list = []hookEntry{}
			}
		}
		writeJSON(w, http.StatusOK, h)
	}))
	srv.Mount("PUT /v1/settings/hooks", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in HooksIO
		if !decodeJSON(w, r, &in) {
			return
		}
		next := in.config()
		c.change(w, func(f *config) *refusal { f.Hooks = next; return nil }, func(ed *edit) {
			ed.set("hooks", nil)
			for event, list := range next.events() {
				if len(list) > 0 {
					ed.set("hooks."+event, hookMaps(list))
				}
			}
		})
	}))
}

// hookMaps are hook entries as ringil.yaml's values.
func hookMaps(list []hookEntry) []interface{} {
	out := make([]interface{}, 0, len(list))
	for _, e := range list {
		m := map[string]interface{}{"command": toAny(e.Command)}
		if len(e.Tools) > 0 {
			m["tools"] = toAny(e.Tools)
		}
		if e.Timeout != "" {
			m["timeout"] = e.Timeout
		}
		if e.OnError != "" {
			m["on_error"] = e.OnError
		}
		out = append(out, m)
	}
	return out
}

func toAny(list []string) []interface{} {
	out := make([]interface{}, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}
