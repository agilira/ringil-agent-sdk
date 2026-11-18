// tools.go: the tools that let the model use installed skills
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"context"
	stderrors "errors"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/sandbox"
)

// Options configure the skill tools.
type Options struct {
	// Sandbox runs skill scripts; nil leaves run_skill_script out.
	Sandbox sandbox.Sandbox
	// Mounts are extra directories scripts see, such as the user's
	// workspace (read-only unless marked writable). The skill itself is
	// always at /skill, read-only.
	Mounts []sandbox.Mount
	// Output, when set, is the host directory that receives the files a
	// script creates. Scripts run in an empty working directory (/work,
	// also $OUTPUT_DIR); afterwards its files are copied into Output,
	// never over an existing file: a name already taken gets a numbered
	// one. Without Output the working directory is discarded.
	Output string
	// Runtimes maps an interpreter name to the absolute path to run it
	// with, for interpreters installed outside /usr (node under ~/.nvm);
	// their directories must be in Mounts.
	Runtimes map[string]string
	// Env is added to every script's environment, such as PYTHONPATH
	// pointing at libraries mounted through Mounts.
	Env map[string]string
	// Network for scripts; default none.
	Network sandbox.Network
	// Limits for scripts; zero values take the sandbox defaults.
	Limits sandbox.Limits
	// Protected host paths no mount may expose (see sandbox.Spec).
	Protected []string
	// Timeout per script; default 2 minutes.
	Timeout time.Duration
	// MaxFile is how much of a file read_skill_file returns; default
	// 256 KiB, so one large reference cannot fill the model's context.
	MaxFile int
	// MaxOutput is how much of a script's output the model sees; default
	// 64 KiB. The rest is read and dropped.
	MaxOutput int
}

type loadIn struct {
	Name string `json:"name" desc:"Name of the skill, as listed in <skills>"`
}

type readIn struct {
	Skill string `json:"skill" desc:"Name of the skill"`
	File  string `json:"file" desc:"Path of the file inside the skill, as listed by load_skill"`
}

type runIn struct {
	Skill  string   `json:"skill" desc:"Name of the skill"`
	Script string   `json:"script" desc:"Path of the script inside the skill, such as scripts/stats.py"`
	Args   []string `json:"args,omitempty" desc:"Arguments passed to the script"`
	Stdin  string   `json:"stdin,omitempty" desc:"Text given to the script on standard input"`
}

// Tools returns load_skill and read_skill_file over c, and
// run_skill_script when opts.Sandbox is set.
func Tools(c *Catalog, opts Options) []agent.Tool {
	tools := []agent.Tool{
		agent.NewTool("load_skill", "Load the instructions of an installed skill. Call it when the task matches the skill's description, before acting.",
			func(_ context.Context, in loadIn) (string, error) { return c.loadForModel(in.Name) }, agent.ReadOnly()),
		agent.NewTool("read_skill_file", "Read a reference file of an installed skill.",
			func(_ context.Context, in readIn) (string, error) {
				data, err := c.ReadFile(in.Skill, in.File)
				if err != nil {
					return "", err
				}
				out := &capped{max: opts.maxFile()}
				if _, err := out.Write(data); err != nil {
					return "", err
				}
				return out.String(), nil
			}, agent.ReadOnly()),
	}
	if opts.Sandbox != nil {
		r := &runner{cat: c, opts: opts}
		tools = append(tools, agent.NewTool("run_skill_script",
			"Run a script of an installed skill in an isolated sandbox: Python, shell, JavaScript, Ruby, Perl, or any script with a #! line. The skill's files are at /skill, the working directory; output is returned.",
			r.run))
	}
	return tools
}

func (c *Catalog) loadForModel(name string) (string, error) {
	body, err := c.Load(name)
	if err != nil {
		return "", err
	}
	files, err := c.Files(name)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(body)
	if len(files) > 1 {
		b.WriteString("\n\n[Files of skill " + name + ": " + strings.Join(files, ", ") + "]")
	}
	return b.String(), nil
}

type runner struct {
	cat  *Catalog
	opts Options
}

// interpreters run scripts by extension. Startup files are skipped and
// bytecode is not written; the environment is the sandbox's own, so no
// variable of the host's reaches the interpreter.
var interpreters = map[string][]string{
	".py":  {"python3", "-B", "-s"},
	".sh":  {"bash", "--noprofile", "--norc"},
	".js":  {"node"},
	".mjs": {"node"},
	".cjs": {"node"},
	".rb":  {"ruby"},
	".pl":  {"perl"},
}

// interpreter picks how to run a script: by its extension, otherwise by
// its #! line, resolved by name inside the sandbox ("#!/usr/bin/env
// python3" and "#!/usr/local/bin/python3" both run python3).
func interpreter(script string, data []byte) ([]string, bool) {
	if argv, ok := interpreters[path.Ext(script)]; ok {
		return argv, true
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if !strings.HasPrefix(line, "#!") || len(fields) == 0 {
		return nil, false
	}
	name, args := path.Base(fields[0]), fields[1:]
	if name == "env" {
		args = dropEnvFlags(args)
		if len(args) == 0 {
			return nil, false
		}
		name, args = path.Base(args[0]), args[1:]
	}
	for _, argv := range interpreters {
		if argv[0] == name {
			return argv, true // the safe flags of a known interpreter
		}
	}
	return append([]string{name}, args...), true
}

// dropEnvFlags skips env's own options ("#!/usr/bin/env -S python3 -u").
func dropEnvFlags(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = args[1:]
	}
	return args
}

func (r *runner) run(ctx context.Context, in runIn) (string, error) {
	argv, err := r.command(in)
	if err != nil {
		return "", err
	}
	copyDir, err := r.cat.verifiedCopy(in.Skill)
	if err != nil {
		return "", err
	}
	defer removeQuietly(copyDir)
	work, err := os.MkdirTemp("", "ringil-work-")
	if err != nil {
		return "", errors.Wrap(err, ErrIO, "cannot prepare the working directory")
	}
	defer removeQuietly(work)
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	out := &capped{max: r.maxOutput()}
	spec := sandbox.Spec{
		Argv:      argv,
		Dir:       workDir,
		Env:       r.env(),
		Mounts:    append([]sandbox.Mount{{Source: copyDir, Target: "/skill"}, {Source: work, Target: workDir, Writable: true}}, r.opts.Mounts...),
		Network:   r.opts.Network,
		Limits:    r.opts.Limits,
		Protected: r.opts.Protected,
		Stdin:     strings.NewReader(in.Stdin),
		Stdout:    out,
		Stderr:    out,
	}
	text, runErr := r.wait(ctx, spec, out)
	return r.deliver(work, text, runErr)
}

const workDir = "/work"

// command is the interpreter, the script and its arguments.
func (r *runner) command(in runIn) ([]string, error) {
	data, err := r.cat.ReadFile(in.Skill, in.Script)
	if err != nil {
		return nil, err
	}
	interp, ok := interpreter(in.Script, data)
	if !ok || in.Script == skillFile {
		return nil, agent.ToolError(ErrScript, "This file is not a script: it has no known extension and no #! line.")
	}
	argv := append([]string{}, interp...)
	if rt, ok := r.opts.Runtimes[argv[0]]; ok {
		argv[0] = rt
	}
	return append(append(argv, "/skill/"+in.Script), in.Args...), nil
}

func (r *runner) env() map[string]string {
	env := map[string]string{"SKILL_DIR": "/skill", "OUTPUT_DIR": workDir}
	for k, v := range r.opts.Env {
		env[k] = v
	}
	return env
}

// deliver copies what the script created into Output, whether it
// succeeded or not, and tells the model where the files are.
func (r *runner) deliver(work, text string, runErr error) (string, error) {
	if r.opts.Output == "" {
		return text, runErr
	}
	saved, err := deliverFiles(work, r.opts.Output)
	if len(saved) > 0 {
		text += "\n[Files saved: " + strings.Join(saved, ", ") + "]"
	}
	var failed *errors.Error
	switch {
	case runErr != nil && stderrors.As(runErr, &failed) && len(saved) > 0:
		return text, failed.WithUserMessage(failed.UserMsg + "\n[Files saved: " + strings.Join(saved, ", ") + "]")
	case err != nil && runErr == nil:
		return text, errors.Wrap(err, ErrIO, "cannot save the script's files").WithUserMessage(text + "\nSome files could not be saved.")
	}
	return text, runErr
}

func (r *runner) timeout() time.Duration {
	if r.opts.Timeout > 0 {
		return r.opts.Timeout
	}
	return 2 * time.Minute
}

func (o Options) maxFile() int {
	if o.MaxFile > 0 {
		return o.MaxFile
	}
	return 256 << 10
}

func (r *runner) maxOutput() int {
	if r.opts.MaxOutput > 0 {
		return r.opts.MaxOutput
	}
	return 64 << 10
}

// verifiedCopy writes every file of a skill, each checked against its
// hash, into a new private directory: the script runs on exactly what the
// user approved, whatever happens to the installed copy meanwhile.
func (c *Catalog) verifiedCopy(name string) (string, error) {
	files, err := c.Files(name)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "ringil-skill-")
	if err != nil {
		return "", errors.Wrap(err, ErrIO, "cannot prepare the skill copy")
	}
	for _, f := range files {
		if err := c.copyFile(name, f, dir); err != nil {
			removeQuietly(dir)
			return "", err
		}
	}
	return dir, nil
}

func (c *Catalog) copyFile(name, file, dir string) error {
	data, err := c.ReadFile(name, file)
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return errors.Wrap(err, ErrIO, "cannot prepare the skill copy")
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return errors.Wrap(err, ErrIO, "cannot prepare the skill copy")
	}
	return nil
}

func removeQuietly(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		return // a temporary copy; the OS cleans its temp directory
	}
}

// capped keeps the first max bytes written and counts the rest.
type capped struct {
	mu      sync.Mutex
	buf     []byte
	max     int
	dropped int
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.max - len(c.buf)
	if room > len(p) {
		room = len(p)
	}
	if room > 0 {
		c.buf = append(c.buf, p[:room]...)
	}
	c.dropped += len(p) - max(room, 0)
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := string(c.buf)
	if c.dropped > 0 {
		s += "\n[" + strconv.Itoa(c.dropped) + " more bytes of output not shown]"
	}
	return s
}

func (r *runner) wait(ctx context.Context, spec sandbox.Spec, out *capped) (string, error) {
	proc, err := r.opts.Sandbox.Start(ctx, spec)
	if err != nil {
		return "", errors.Wrap(err, ErrScript, "cannot start the sandbox").WithUserMessage("The sandbox could not start the script: " + err.Error())
	}
	werr := proc.Wait()
	text := out.String()
	switch {
	case ctx.Err() != nil:
		return text, errors.Wrap(ctx.Err(), ErrScript, "script stopped").WithUserMessage("The script was stopped after " + r.timeout().String() + ". Output so far:\n" + text)
	case werr != nil:
		return text, errors.Wrap(werr, ErrScript, "script failed").WithUserMessage("The script failed (" + werr.Error() + "). Output:\n" + text)
	}
	return text, nil
}
