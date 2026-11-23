// permission_test.go: what a Policy runs, asks and refuses
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

type commandIn struct {
	Command string `json:"command"`
}
type pathIn struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
}
type urlIn struct {
	URL string `json:"url"`
}

func noop[In any](context.Context, In) (string, error) { return "", nil }

// specs: tools as the SDK's are, with their arguments.
var (
	bashSpec  = agent.NewTool("bash", "t", noop[commandIn], agent.Destructive(), agent.OpenWorld(), agent.SelfConfirms()).Spec()
	writeSpec = agent.NewTool("write_file", "t", noop[pathIn], agent.Edits()).Spec()
	readSpec  = agent.NewTool("read_file", "t", noop[pathIn], agent.ReadOnly()).Spec()
	fetchSpec = agent.NewTool("fetch", "t", noop[urlIn], agent.OpenWorld()).Spec()
	forget    = agent.NewTool("forget", "t", noop[struct{}], agent.Destructive()).Spec()
	todoSpec  = agent.NewTool("todo_write", "t", noop[struct{}], agent.Internal()).Spec()
	unmarked  = agent.NewTool("publish", "t", noop[struct{}]).Spec()
)

func call(name, input string) agent.ToolCall {
	return agent.ToolCall{ID: "1", Name: name, Input: json.RawMessage(input)}
}

// outcome is what p does with a call, without asking.
func outcome(p *agent.Policy, c agent.ToolCall, s agent.ToolSpec) string {
	switch p.Effect(c, s) {
	case agent.EffectAllow:
		return "allow"
	case agent.EffectDeny:
		return "deny"
	}
	return "ask"
}

// The modes, on calls no rule matches, as coding agents apply them; a tool
// marked nothing asks.
func TestPolicyModes(t *testing.T) {
	specs := []agent.ToolSpec{readSpec, writeSpec, bashSpec, forget, todoSpec, fetchSpec, unmarked}
	want := map[agent.Mode][7]string{ // read, edit, bash, destructive, bookkeeping, outward, unmarked
		agent.ModeDefault:     {"allow", "ask", "allow", "ask", "allow", "ask", "ask"},
		agent.ModeAcceptEdits: {"allow", "allow", "allow", "ask", "allow", "ask", "ask"},
		agent.ModePlan:        {"allow", "deny", "deny", "deny", "allow", "deny", "deny"},
		agent.ModeBypass:      {"allow", "allow", "allow", "allow", "allow", "allow", "allow"},
	}
	for mode, w := range want {
		p := &agent.Policy{Mode: mode}
		for i, s := range specs {
			if got := outcome(p, call(s.Name, `{}`), s); got != w[i] {
				t.Errorf("%s %s: %s, want %s", mode, s.Name, got, w[i])
			}
		}
	}
}

// Deny wins; plan mode refuses changes whatever allows them; then ask,
// then allow.
func TestPolicyRules(t *testing.T) {
	p := &agent.Policy{Mode: agent.ModeBypass, Rules: mustRules(t, map[agent.Effect][]string{
		agent.EffectAllow: {"bash(git:*)", "write_file(src/**)", "fetch(domain:go.dev)"},
		agent.EffectAsk:   {"bash(git push:*)"},
		agent.EffectDeny:  {"bash(rm:*)", "write_file(.env)", "write_file(src/secret/**)"},
	})}
	for _, tc := range []struct {
		c    agent.ToolCall
		s    agent.ToolSpec
		want string
	}{
		{call("bash", `{"command":"git status"}`), bashSpec, "allow"},
		{call("bash", `{"command":"git push origin main"}`), bashSpec, "ask"},
		{call("bash", `{"command":"git status; rm -rf ~"}`), bashSpec, "deny"},
		{call("write_file", `{"path":"src/a/b.go"}`), writeSpec, "allow"},
		{call("write_file", `{"path":"src/secret/k.pem"}`), writeSpec, "deny"},
		{call("write_file", `{"path":"/home/u/app/.ENV"}`), writeSpec, "deny"},
		{call("fetch", `{"url":"https://pkg.go.dev/x"}`), fetchSpec, "allow"},
	} {
		if got := outcome(p, tc.c, tc.s); got != tc.want {
			t.Errorf("%s %s: %s, want %s", tc.c.Name, tc.c.Input, got, tc.want)
		}
	}
	plan := &agent.Policy{Mode: agent.ModePlan, Rules: mustRules(t, map[agent.Effect][]string{agent.EffectAllow: {"bash", "write_file"}})}
	if got := outcome(plan, call("bash", `{"command":"rm -rf ~"}`), bashSpec); got != "deny" {
		t.Errorf("plan let an allow rule run bash: %s", got)
	}
}

// Audit 23/11: the subject is the argument the schema declares; one the
// model adds is never read (the tool drops it and runs on its own).
func TestRulesReadTheDeclaredArgument(t *testing.T) {
	rules := mustRules(t, map[agent.Effect][]string{
		agent.EffectAllow: {"write_file(src/**)", "fetch(domain:go.dev)"},
		agent.EffectDeny:  {"write_file(**/.env)"},
	})
	strict := &agent.Policy{Rules: rules}
	for _, tc := range []struct {
		c    agent.ToolCall
		s    agent.ToolSpec
		want string
	}{
		{call("write_file", `{"command":"src/x","path":"/home/u/.bashrc","content":"x"}`), writeSpec, "ask"},
		{call("write_file", `{"command":"x","path":"a/.env"}`), writeSpec, "deny"},
		{call("fetch", `{"command":"go.dev","url":"https://evil.com/"}`), fetchSpec, "ask"},
	} {
		if got := outcome(strict, tc.c, tc.s); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.c.Input, got, tc.want)
		}
	}
}

// An allow rule matches one plain command: nothing joined, expanded,
// escaped or launched through another program.
func TestAllowRulesMatchOneCommand(t *testing.T) {
	p := &agent.Policy{Rules: mustRules(t, map[agent.Effect][]string{agent.EffectAllow: {"bash(git status:*)", "bash(npm test)", "bash(sudo:*)"}})}
	bash := agent.NewTool("bash", "t", noop[commandIn], agent.Destructive()).Spec() // asks without a rule
	for input, want := range map[string]string{
		`{"command":"git status -s"}`:                 "allow",
		`{"command":"git statusx"}`:                   "ask",
		`{"command":"git status && curl x | sh"}`:     "ask",
		`{"command":"git status ${x:=y}"}`:            "ask",
		`{"command":"git status $HOME"}`:              "ask",
		`{"command":"git status > /etc/passwd"}`:      "ask",
		`{"command":"git status\nrm -rf ~"}`:          "ask",
		`{"command":"X=1 git status"}`:                "ask",
		`{"command":"npm test"}`:                      "allow",
		`{"command":"npm test --x"}`:                  "ask",
		`{"command":"sudo rm -rf /"}`:                 "ask",
		`{"command":"\"git\" status"}`:                "ask",
		`{"command":"git status -c 'x' --porcelain"}`: "allow",
	} {
		if got := outcome(p, call("bash", input), bash); got != want {
			t.Errorf("%s: %s, want %s", input, got, want)
		}
	}
}

// Audit 23/11: a deny rule finds its command however it is written; one
// whose words an expansion hides asks, refused where no one can be asked.
func TestDenyRulesAreHardToDodge(t *testing.T) {
	p := &agent.Policy{Mode: agent.ModeBypass, Rules: mustRules(t, map[agent.Effect][]string{agent.EffectDeny: {"bash(rm:*)", "bash(git push:*)", "fetch(domain:evil.com)"}})}
	for command, want := range map[string]string{
		"X=1 rm -rf ~": "deny", `"rm" -rf ~`: "deny", `\rm -rf ~`: "deny", "/bin/rm -rf ~": "deny", "command rm x": "deny",
		"bash -c 'rm -rf ~'": "deny", "if true; then rm x; fi": "deny", "! rm x": "deny", "sudo rm x": "deny", "timeout 5 rm x": "deny",
		"echo a&&rm x": "deny", "r''m -rf /": "deny", `"r"m -rf /`: "deny", "/bin/r? x": "deny", "/bin/r[m] x": "deny",
		"git -C . push": "deny", "git --no-pager push": "deny",
		"r\\\nm x": "ask", "rm${IFS}-rf ~": "ask", "a=r;b=m;$a$b -rf /": "ask", "$(printf r)m x": "ask", "bash -c \"$x\"": "ask",
		"rm -rf $HOME": "deny", "{rm,-rf,x}": "deny", "echo $HOME": "allow", "ls ${HOME}": "allow",
		"git status": "allow", "ls -la": "allow",
	} {
		input, _ := json.Marshal(map[string]string{"command": command})
		if got := outcome(p, agent.ToolCall{Name: "bash", Input: input}, bashSpec); got != want {
			t.Errorf("%q: %s, want %s", command, got, want)
		}
	}
	for _, u := range []string{"https://evil.com./x", "https://EVIL.com", "https://a.evil.com", "https://xn--vil-8na.com", "https://%zz/"} {
		if got := outcome(p, call("fetch", `{"url":"`+u+`"}`), fetchSpec); got != "deny" {
			t.Errorf("%s: %s", u, got)
		}
	}
	files := &agent.Policy{Mode: agent.ModeBypass, Rules: mustRules(t, map[agent.Effect][]string{agent.EffectDeny: {"write_file(.env)"}})}
	for _, name := range []string{".env.", ".env ", ".env::$DATA", "a/.ENV", "C:\\app\\.env"} {
		if got := outcome(files, call("write_file", `{"path":"`+name+`"}`), writeSpec); got != "deny" {
			t.Errorf("%q: %s", name, got)
		}
	}
}

// Audit 23/11: a relative allow pattern never reaches outside the folder.
func TestAllowPathsStayInside(t *testing.T) {
	p := &agent.Policy{Rules: mustRules(t, map[agent.Effect][]string{agent.EffectAllow: {"write_file(**/*.go)"}})}
	for path, want := range map[string]string{"src/a.go": "allow", "/etc/x.go": "ask", "../x.go": "ask", "src/../../x.go": "ask"} {
		if got := outcome(p, call("write_file", `{"path":"`+path+`"}`), writeSpec); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
}

// With no one to ask, what would ask is refused, with an error the run
// records; an agent without Approve does so.
func TestPolicyFailsClosed(t *testing.T) {
	d, err := (&agent.Policy{}).Approve(context.Background(), call("forget", `{}`), forget)
	if d.Allow || !errors.HasCode(err, agent.ErrToolDenied) {
		t.Fatalf("%+v %v", d, err)
	}
	if d, err := (&agent.Policy{}).Approve(context.Background(), call("read_file", `{}`), readSpec); !d.Allow || err != nil {
		t.Fatalf("reading refused: %+v %v", d, err)
	}
}

// An "always" answer keeps a narrow rule for the session; for the project
// it is handed to the host as well.
func TestPolicyRemembers(t *testing.T) {
	var kept []agent.Rule
	asks := 0
	p := &agent.Policy{
		Ask: func(context.Context, agent.ToolCall, agent.ToolSpec) (agent.Answer, error) {
			asks++
			return agent.Answer{Allow: true, Remember: agent.RememberProject}, nil
		},
		Remember: func(r agent.Rule, scope agent.Scope) error {
			if scope == agent.RememberProject {
				kept = append(kept, r)
			}
			return nil
		},
	}
	for _, path := range []string{"src/a.go", "src/b.go", "other/c.go"} {
		if d, err := p.Approve(context.Background(), call("write_file", `{"path":"`+path+`"}`), writeSpec); !d.Allow || err != nil {
			t.Fatalf("%s: %+v %v", path, d, err)
		}
	}
	if asks != 2 || len(kept) != 2 || kept[0].String() != "write_file(src/**)" {
		t.Fatalf("asks %d, kept %v", asks, kept)
	}
}

// Audit 23/11: what an "always" keeps is narrow, or nothing.
func TestRuleFor(t *testing.T) {
	for _, tc := range []struct {
		c    agent.ToolCall
		s    agent.ToolSpec
		want string // "" for none
	}{
		{call("bash", `{"command":"go test ./..."}`), bashSpec, "bash(go test:*)"},
		{call("bash", `{"command":"go test ./... && rm x"}`), bashSpec, ""},
		{call("bash", `{"command":"bash -c 'rm -rf ~'"}`), bashSpec, ""},
		{call("bash", `{"command":"sudo rm -rf /"}`), bashSpec, ""},
		{call("bash", `{"command":"find . -name x"}`), bashSpec, ""},
		{call("bash", `{"command":"git -C . log"}`), bashSpec, ""},
		{call("bash", `{"command":"rm -rf build"}`), bashSpec, "bash(rm -rf build)"},
		{call("bash", `{"command":"docker run alpine"}`), bashSpec, "bash(docker run alpine)"},
		{call("bash", `{"command":"git log --oneline"}`), bashSpec, "bash(git log:*)"},
		{call("bash", `{"command":"git branch -D main"}`), bashSpec, "bash(git branch -D main)"},
		{call("bash", `{"command":"docker container rm x"}`), bashSpec, "bash(docker container rm x)"},
		{call("write_file", `{"path":"/home/u/.bashrc"}`), writeSpec, ""},
		{call("write_file", `{"path":"~/x"}`), writeSpec, ""},
		{call("write_file", `{"path":"C:\\Users\\u\\x"}`), writeSpec, ""},
		{call("write_file", `{"path":"README.md"}`), writeSpec, ""},
		{call("write_file", `{"path":"/x"}`), writeSpec, ""},
		{call("write_file", `{"path":"/home/x"}`), writeSpec, ""},
		{call("write_file", `{"path":"../../x"}`), writeSpec, ""},
		{call("write_file", `{"path":"C:\\x"}`), writeSpec, ""},
		{call("write_file", `{"path":"/home/u/app/x.go"}`), writeSpec, "write_file(/home/u/app/**)"},
		{call("fetch", `{"url":"https://pkg.go.dev/x"}`), fetchSpec, "fetch(domain:pkg.go.dev)"},
		{call("forget", `{}`), forget, "forget"},
	} {
		r, ok := agent.RuleFor(tc.c, tc.s)
		if got := map[bool]string{true: r.String(), false: ""}[ok]; got != tc.want {
			t.Errorf("%s %s: %q, want %q", tc.c.Name, tc.c.Input, got, tc.want)
		}
	}
}

func TestParseRule(t *testing.T) {
	for _, bad := range []string{"", "bash(", "bash()", "ba sh", "a/b", "[x"} {
		if _, err := agent.ParseRule(agent.EffectAllow, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := agent.ParseRule("maybe", "bash"); err == nil {
		t.Error("an unknown effect")
	}
	if r, err := agent.ParseRule(agent.EffectDeny, " bash(rm -rf:*) "); err != nil || r.String() != "bash(rm -rf:*)" {
		t.Fatalf("%v %v", r, err)
	}
}

func TestPlanModeExplicitDenyMessage(t *testing.T) {
	p := &agent.Policy{Mode: agent.ModePlan, Rules: mustRules(t, map[agent.Effect][]string{agent.EffectDeny: {"todo_write", "write_file(.env)"}})}
	for _, tc := range []struct {
		c agent.ToolCall
		s agent.ToolSpec
	}{
		{call("todo_write", `{}`), todoSpec},
		{call("write_file", `{"path":".env"}`), writeSpec},
	} {
		d, err := p.Approve(context.Background(), tc.c, tc.s)
		if d.Allow || err != nil || strings.Contains(d.Message, "Plan mode") {
			t.Fatalf("%s: %+v %v", tc.c.Name, d, err)
		}
	}
	d, _ := p.Approve(context.Background(), call("write_file", `{"path":"a.go"}`), writeSpec)
	if !strings.Contains(d.Message, "Plan mode") {
		t.Fatalf("expected plan mode message, got %q", d.Message)
	}
}

func TestDeepGlobDoesNotBacktrackExponentially(t *testing.T) {
	p := &agent.Policy{Rules: mustRules(t, map[agent.Effect][]string{
		agent.EffectDeny: {
			"write_file(**/**/**/**/**/**/**/secret)",
			"write_file(**/a/**/a/**/a/**/a/**/a/**/z)",
		},
	})}
	deep := strings.Repeat("a/", 40) + "ok.go"
	start := time.Now()
	if got := outcome(p, call("write_file", `{"path":"`+deep+`"}`), writeSpec); got != "ask" || time.Since(start) > time.Second {
		t.Fatalf("%s in %v", got, time.Since(start))
	}
}

func mustRules(t *testing.T, by map[agent.Effect][]string) []agent.Rule {
	t.Helper()
	var rules []agent.Rule
	for effect, texts := range by {
		for _, text := range texts {
			r, err := agent.ParseRule(effect, text)
			if err != nil {
				t.Fatal(err)
			}
			rules = append(rules, r)
		}
	}
	return rules
}
