// skills.go: installed skills in the terminal agent
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/sandbox"
	"github.com/agilira/ringil-agent-sdk/skills"
	"github.com/agilira/ringil-agent-sdk/tools/files"
)

func (c *cli) skillsDir() (string, error) {
	base, err := c.configDir()
	if err != nil {
		return "", errors.Wrap(err, ErrUsage, "no user config directory")
	}
	return filepath.Join(base, "ringil", "skills"), nil
}

// skillCatalog opens the installed skills; nil with --no-tools.
func (c *cli) skillCatalog(ctx *orpheus.Context) (*skills.Catalog, error) {
	if ctx.GetGlobalFlagBool("no-tools") {
		return nil, nil
	}
	dir, err := c.skillsDir()
	if err != nil {
		return nil, err
	}
	return skills.Open(dir)
}

// skillOptions: scripts run in the sandbox when this platform has one,
// never seeing the agent's own configuration. The start directory is
// visible read-only, with the secrets the file tools refuse hidden, unless
// it is the home or a system directory.
func (c *cli) skillOptions() skills.Options {
	sb, err := c.newSandbox()
	if err != nil {
		return skills.Options{}
	}
	opts := skills.Options{Sandbox: sb}
	base, berr := c.configDir()
	if berr == nil {
		opts.Protected = []string{filepath.Join(base, "ringil")}
	}
	home := c.realHome()
	opts.MountsFunc = func() []sandbox.Mount {
		if m, ok := c.startMount(home, base); ok {
			return []sandbox.Mount{m}
		}
		return nil
	}
	opts.Output = c.outputDir(home)
	opts.OutputQuota = 10 << 30
	c.addRuntimes(&opts, home)
	if libs := c.pythonLibs(home); len(libs) > 0 {
		for _, l := range libs {
			if m, ok := maskedMount(l); ok {
				opts.Mounts = append(opts.Mounts, m)
			}
		}
		opts.Env = withEnv(opts.Env, "PYTHONPATH", strings.Join(libs, string(os.PathListSeparator)))
	}
	return opts
}

// realHome is the home with links resolved, so that comparisons with
// resolved paths hold; empty when unknown.
func (c *cli) realHome() string {
	home, err := c.home()
	if err != nil || !filepath.IsAbs(home) {
		return ""
	}
	if real, err := filepath.EvalSymlinks(home); err == nil {
		return real
	}
	return home
}

// maxMasks keeps each mount's hidden paths a share of the sandbox's
// total (sandbox.MaxHidden); a directory with more secrets than that is
// not shown at all.
const maxMasks = sandbox.MaxHidden / 5

// maskedMount is dir read-only with the file tools' secrets hidden.
func maskedMount(dir string) (sandbox.Mount, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hide, err := files.Hidden(ctx, dir, nil)
	if err != nil || len(hide) > maxMasks {
		return sandbox.Mount{}, false
	}
	return sandbox.Mount{Source: dir, Hide: hide}, true
}

// outputDir: files scripts create are handed over in ~/ringil-output, one
// folder per run, outside any project: dropped into the start directory
// they could be picked up by tools that run code (git, test runners).
func (c *cli) outputDir(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, "ringil-output")
}

// runtimeNames are interpreters often installed per user (nvm, rbenv,
// perlbrew, pyenv) rather than under /usr.
var runtimeNames = []string{"node", "ruby", "perl", "python3"}

// addRuntimes makes interpreters installed outside /usr usable by
// scripts: their installation directory is mounted read-only, with the
// secrets policy applied, and scripts run them by absolute path.
func (c *cli) addRuntimes(opts *skills.Options, home string) {
	for _, name := range runtimeNames {
		real, prefix, ok := c.userRuntime(name, home)
		if !ok {
			continue
		}
		m, ok := maskedMount(prefix)
		if !ok {
			continue
		}
		if opts.Runtimes == nil {
			opts.Runtimes = map[string]string{}
		}
		opts.Runtimes[name] = real
		opts.Mounts = append(opts.Mounts, m)
		if name == "node" {
			opts.Env = withEnv(opts.Env, "NODE_PATH", filepath.Join(prefix, "lib", "node_modules"))
		}
	}
}

// userRuntime finds name on PATH outside /usr; prefix is the installation
// directory (the parent of its bin). Refused: a binary that is not the
// interpreter itself (a version manager's shim), and a prefix that is the
// home, contains it, or is a dot directory right under it (~/.local holds
// far more than one runtime).
func (c *cli) userRuntime(name, home string) (string, string, bool) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", "", false
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil || strings.HasPrefix(real, "/usr/") || !strings.HasPrefix(filepath.Base(real), name) {
		return "", "", false
	}
	prefix := filepath.Dir(filepath.Dir(real))
	if prefix == "/" || sandbox.Reserved(filepath.ToSlash(prefix)) || !allowedPrefix(prefix, home) {
		return "", "", false
	}
	return real, prefix, true
}

func allowedPrefix(prefix, home string) bool {
	if home == "" {
		return true
	}
	if inside(home, prefix) {
		return false // the home itself, or above it
	}
	rel, err := filepath.Rel(home, prefix)
	return err != nil || !strings.HasPrefix(rel, ".") || strings.Contains(rel, string(filepath.Separator))
}

func withEnv(env map[string]string, k, v string) map[string]string {
	if env == nil {
		env = map[string]string{}
	}
	env[k] = v
	return env
}

// startMount is the start directory for scripts, read-only with secrets
// hidden, unless it is the home, a system directory or holds the agent's
// configuration. Computed before every script, so secrets created during
// the session are hidden too.
func (c *cli) startMount(home, config string) (sandbox.Mount, bool) {
	cwd, err := c.cwd()
	if err != nil {
		return sandbox.Mount{}, false
	}
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	if cwd == home || (home != "" && inside(home, cwd)) || sandbox.Reserved(filepath.ToSlash(cwd)) || (config != "" && inside(config, cwd)) {
		return sandbox.Mount{}, false
	}
	return maskedMount(cwd)
}

// inside reports whether p is dir or below it.
func inside(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// pythonVersion is the directory name of the python3 that scripts run
// ("python3.14"): packages built for another version would not load.
func pythonVersion() string {
	p, err := exec.LookPath("python3")
	if err == nil {
		if real, err := filepath.EvalSymlinks(p); err == nil && strings.HasPrefix(filepath.Base(real), "python3.") {
			return filepath.Base(real)
		}
	}
	return "python3*"
}

// pythonLibs are the Python packages the user installed outside the
// system: the active virtualenv first, then the user site (pip install
// --user). Scripts see them read-only, as the user's own Python would.
func (c *cli) pythonLibs(home string) []string {
	var patterns []string
	if venv := c.getenv("VIRTUAL_ENV"); venv != "" {
		patterns = append(patterns, filepath.Join(venv, "lib", "python3*", "site-packages"))
	}
	if home != "" {
		patterns = append(patterns, filepath.Join(home, ".local", "lib", pythonVersion(), "site-packages"))
	}
	var libs []string
	for _, p := range patterns {
		found, err := filepath.Glob(p)
		if err != nil {
			continue
		}
		libs = append(libs, found...)
	}
	return libs
}

func (c *cli) cmdSkills(*orpheus.Context) error {
	dir, err := c.skillsDir()
	if err != nil {
		return err
	}
	cat, err := skills.Open(dir)
	if err != nil {
		return err
	}
	if len(cat.List())+len(cat.Broken()) == 0 {
		return c.line("no skills installed (ringil skill install <dir>)")
	}
	for _, s := range cat.List() {
		if err := c.line(s.Name + "  " + short(s.Description)); err != nil {
			return err
		}
	}
	for name, err := range cat.Broken() {
		if lerr := c.line("! " + name + "  damaged, reinstall it: " + short(err.Error())); lerr != nil {
			return lerr
		}
	}
	return nil
}

func (c *cli) cmdSkillInstall(ctx *orpheus.Context) error {
	if len(ctx.Positional()) != 1 {
		return errors.New(ErrUsage, "name the skill directory: ringil skill install <dir>")
	}
	dir, err := c.skillsDir()
	if err != nil {
		return err
	}
	info, err := skills.Install(ctx.Context(), ctx.Positional()[0], dir)
	if err != nil {
		return err
	}
	return c.line("installed " + info.Name + ": " + short(info.Description))
}

func (c *cli) cmdSkillRemove(ctx *orpheus.Context) error {
	if len(ctx.Positional()) == 0 {
		return errors.New(ErrUsage, "name the skills to remove (see: ringil skill)")
	}
	dir, err := c.skillsDir()
	if err != nil {
		return err
	}
	for _, name := range ctx.Positional() {
		if err := skills.Remove(dir, name); err != nil {
			return err
		}
	}
	return nil
}
