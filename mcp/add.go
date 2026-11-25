// add.go: adding an MCP server the user asked for
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"context"
	stderrors "errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
)

// Entry is an MCP server to add, as the user or the agent describes it
// from its documentation: a Command for a local server, or a URL for a
// remote one. Values may hold ${NAME}, a secret or variable the host
// resolves when the server starts; header values must be only that, so
// that no key is written down in clear.
type Entry struct {
	Name    string            `json:"name"`
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Source is where it came from, such as the repository's link.
	Source string `json:"source,omitempty"`
}

const (
	maxArgs     = 64
	maxValue    = 4096
	maxVars     = 64
	maxEntryURL = 2048
)

var (
	varName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	headerName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,128}$`)
	// headerRef is a header value naming a secret, with an optional
	// scheme before it: "${API_KEY}", "Bearer ${TOKEN}".
	headerRef = regexp.MustCompile(`^([A-Za-z]{1,16} )?\$\{[A-Za-z_][A-Za-z0-9_]{0,127}\}$`)
	reference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]{0,127})\}`)
)

// Check refuses an entry that could not run or would hide something
// from the user who approves it: a bad name, both or neither of a
// command and a URL, a URL that is not https (but to loopback), values
// with characters that do not print, a header value written in clear.
func (e Entry) Check() error {
	if !serverName.MatchString(e.Name) {
		return errors.New(ErrConfig, "MCP server names are 1-32 letters, digits, _ or -")
	}
	if (len(e.Command) == 0) == (e.URL == "") {
		return errors.New(ErrConfig, "an MCP server needs either a command or a URL, not both")
	}
	checks := []func() error{e.checkCommand, e.checkURL, e.checkEnv, e.checkHeaders}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	if !printable(e.Source, maxEntryURL) {
		return errors.New(ErrConfig, "the source holds characters that do not print")
	}
	return nil
}

func (e Entry) checkCommand() error {
	if len(e.Command) > maxArgs {
		return errors.New(ErrConfig, "the command has more than "+strconv.Itoa(maxArgs)+" arguments")
	}
	for i, a := range e.Command {
		if !printable(a, maxValue) || (i == 0 && strings.TrimSpace(a) == "") {
			return errors.New(ErrConfig, "the command holds an empty program or characters that do not print")
		}
	}
	return nil
}

func (e Entry) checkURL() error {
	if e.URL == "" {
		return nil
	}
	if len(e.URL) > maxEntryURL || !printable(e.URL, maxEntryURL) {
		return errors.New(ErrConfig, "the URL is too long or holds characters that do not print")
	}
	if err := safehttp.CheckURL(e.URL, false); err != nil {
		return errors.New(ErrConfig, "the URL must use https (http only to this computer), without credentials")
	}
	return nil
}

func (e Entry) checkEnv() error {
	if len(e.Env) > maxVars {
		return errors.New(ErrConfig, "too many environment variables")
	}
	if len(e.Env) > 0 && e.URL != "" {
		return errors.New(ErrConfig, "environment variables are for a local server; a remote one takes headers")
	}
	for k, v := range e.Env {
		if !varName.MatchString(k) || !printable(v, maxValue) {
			return errors.New(ErrConfig, "environment variable "+clip(k, 64)+": bad name, or a value with characters that do not print")
		}
	}
	return nil
}

func (e Entry) checkHeaders() error {
	if len(e.Headers) > maxVars {
		return errors.New(ErrConfig, "too many headers")
	}
	if len(e.Headers) > 0 && e.URL == "" {
		return errors.New(ErrConfig, "headers are for a remote server; a local one takes environment variables")
	}
	for k, v := range e.Headers {
		if !headerName.MatchString(k) {
			return errors.New(ErrConfig, "not a header name: "+clip(k, 64))
		}
		if !headerRef.MatchString(v) {
			return errors.New(ErrConfig, "header "+http.CanonicalHeaderKey(k)+" must name a secret, like ${API_KEY} or Bearer ${TOKEN}, never hold a key in clear")
		}
	}
	return nil
}

// printable: s is valid UTF-8 of at most max bytes, with no control,
// zero-width or bidi character, so the approval shows all of it.
func printable(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029
	})
}

// References are the ${NAME}s an entry's values hold, sorted: what the
// host must find among its secrets or variables before starting it.
func (e Entry) References() []string {
	seen := map[string]bool{}
	for _, values := range []map[string]string{e.Env, e.Headers} {
		for _, v := range values {
			for _, m := range reference.FindAllStringSubmatch(v, -1) {
				seen[m[1]] = true
			}
		}
	}
	for _, a := range append([]string{e.URL}, e.Command...) {
		for _, m := range reference.FindAllStringSubmatch(a, -1) {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Added is what the host reports of a server it added.
type Added struct {
	// Tools are the names the agent calls the server's tools by.
	Tools []string
	// Missing are the ${NAME}s the host could not resolve yet: the server
	// is saved and starts once the user sets them.
	Missing []string
}

// Adder saves an entry where the host keeps its servers and connects it.
// It is called only with an entry that passed Check.
type Adder func(ctx context.Context, e Entry) (Added, error)

// AddFunc checks e and hands it to add: the one path for every front
// (a tool call, a slash command, a command line).
func AddFunc(ctx context.Context, e Entry, add Adder) (Added, error) {
	if err := e.Check(); err != nil {
		return Added{}, err
	}
	return add(ctx, e)
}

type nameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type addServerIn struct {
	Name    string      `json:"name" desc:"A short name for the server: letters, digits, _ or -; it prefixes its tools"`
	Command []string    `json:"command,omitempty" desc:"For a local server: the program and its arguments, exactly as its documentation gives them, e.g. [\"npx\", \"-y\", \"@org/server\"]. Never a shell line"`
	Env     []nameValue `json:"env,omitempty" desc:"For a local server: environment variables; put a secret as ${NAME}, never its value"`
	URL     string      `json:"url,omitempty" desc:"For a remote server: its https URL"`
	Headers []nameValue `json:"headers,omitempty" desc:"For a remote server: headers, each value a secret reference like ${API_KEY} or Bearer ${TOKEN}"`
	Source  string      `json:"source,omitempty" desc:"The link the user gave, such as the GitHub repository"`
}

// AddTool is add_mcp_server: the agent adds the MCP server the user
// asked for, as coding agents add what their user pastes, reading the
// command or URL from the server's documentation. It is destructive and
// reaches the network, so the host's policy asks the user first, showing
// the command; a local server runs with the rights the host gives it.
func AddTool(add Adder) agent.Tool {
	return agent.NewTool("add_mcp_server",
		"Add an MCP server the user asked for (a GitHub link, a package, a URL): read its documentation for the command or the URL, then call this. "+
			"Secrets go as ${NAME}: tell the user to set them. Use it only when the user asks to add a server.",
		func(ctx context.Context, in addServerIn) (string, error) {
			e := Entry{Name: in.Name, Command: in.Command, URL: in.URL, Env: toMap(in.Env), Headers: toMap(in.Headers), Source: in.Source}
			if len(e.Env) != len(in.Env) || len(e.Headers) != len(in.Headers) {
				return "", agent.ToolError(ErrConfig, "A variable or header is given twice: give each once.")
			}
			got, err := AddFunc(ctx, e, add)
			if err != nil {
				return "", modelError(err)
			}
			return got.summary(e), nil
		}, agent.Destructive(), agent.OpenWorld())
}

func toMap(vars []nameValue) map[string]string {
	if len(vars) == 0 {
		return nil
	}
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Name] = v.Value
	}
	return m
}

func (a Added) summary(e Entry) string {
	var b strings.Builder
	b.WriteString("Added MCP server " + e.Name + ".")
	if len(a.Missing) > 0 {
		b.WriteString(" It starts once the user sets: " + strings.Join(a.Missing, ", ") + ".")
	}
	if len(a.Tools) > 0 {
		b.WriteString(" Its tools: " + strings.Join(a.Tools, ", ") + ".")
	}
	return b.String()
}

// modelError shows the model a refusal of the entry, which it can fix;
// anything else from the host is a code.
func modelError(err error) error {
	var coded *errors.Error
	if stderrors.As(err, &coded) && coded.UserMsg == "" && coded.Code == ErrConfig {
		return coded.WithUserMessage(coded.Message)
	}
	return err
}
