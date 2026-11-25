// github.go: installing a skill from a GitHub link
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
)

// ErrFetch: the skill could not be fetched (not found, refused, too big).
const ErrFetch errors.ErrorCode = "SKILL_FETCH"

// GitHub configures where skills are fetched from. Its zero value is
// github.com, without a token.
type GitHub struct {
	// Headers, when set, adds headers to every request, such as an
	// Authorization with a token for private repositories or a higher
	// rate limit; called per request, so a rotated token applies.
	Headers func(ctx context.Context) (http.Header, error)
	// Client is the base HTTP client (a proxy, a timeout).
	Client *http.Client
	// Host, API and Codeload are the web host links are accepted for,
	// the REST API and the archive server; default github.com,
	// https://api.github.com and https://codeload.github.com. Change them
	// for a GitHub Enterprise server or a test.
	Host, API, Codeload string
	// MaxDownload bounds the compressed archive; default 64 MiB.
	MaxDownload int64
	// MaxFiles and MaxBytes bound what is unpacked; defaults 5000 files
	// and 128 MiB.
	MaxFiles int
	MaxBytes int64
}

const (
	defaultHost        = "github.com"
	defaultAPI         = "https://api.github.com"
	defaultCodeload    = "https://codeload.github.com"
	defaultMaxDownload = 64 << 20
	defaultFetchFiles  = 5000
	defaultFetchBytes  = 128 << 20
)

func (g GitHub) host() string     { return or(g.Host, defaultHost) }
func (g GitHub) api() string      { return strings.TrimSuffix(or(g.API, defaultAPI), "/") }
func (g GitHub) codeload() string { return strings.TrimSuffix(or(g.Codeload, defaultCodeload), "/") }

func (g GitHub) maxDownload() int64 {
	if g.MaxDownload > 0 {
		return g.MaxDownload
	}
	return defaultMaxDownload
}

func (g GitHub) limits() limits {
	l := limits{files: defaultFetchFiles, bytes: defaultFetchBytes}
	if g.MaxFiles > 0 {
		l.files = g.MaxFiles
	}
	if g.MaxBytes > 0 {
		l.bytes = g.MaxBytes
	}
	return l
}

func or(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

// Installed is a skill installed from a link.
type Installed struct {
	Info
	// Source is the repository, folder and commit it came from:
	// github.com/owner/repo/path@commit.
	Source string
	// Files are its files; Scripts those run_skill_script can run.
	Files   []string
	Scripts []string
}

// InstallGitHub installs the skill a GitHub link points to into the
// catalog directory dir. The link is a repository, a folder (…/tree/ref/
// path), a SKILL.md (…/blob/ref/path/SKILL.md), or owner/repo/path@ref.
// The ref (the default branch when there is none) is resolved to its
// commit, and that commit's archive is what is installed, so the source
// recorded is exactly what was fetched. A folder holding several skills
// is refused with their links listed, for the user to choose one.
func InstallGitHub(ctx context.Context, link, dir string, g GitHub) (Installed, error) {
	ref, err := parseLink(link, g.host())
	if err != nil {
		return Installed{}, err
	}
	client, err := g.client()
	if err != nil {
		return Installed{}, err
	}
	commit, err := resolveCommit(ctx, client, g.api(), ref)
	if err != nil {
		return Installed{}, err
	}
	tmp, err := os.MkdirTemp("", "ringil-fetch-")
	if err != nil {
		return Installed{}, errors.Wrap(err, ErrIO, "cannot prepare the download")
	}
	defer removeQuietly(tmp)
	skillDir, err := fetchSkill(ctx, client, g, ref, commit, tmp)
	if err != nil {
		return Installed{}, err
	}
	source := ref.source(g.host(), path.Join(ref.path, skillDir), commit)
	info, err := install(ctx, filepath.Join(tmp, filepath.FromSlash(skillDir)), dir, source)
	if err != nil {
		return Installed{}, err
	}
	return describe(dir, info, source)
}

// client is the HTTP client for the API and the archives, both https.
func (g GitHub) client() (*http.Client, error) {
	for _, u := range []string{g.api(), g.codeload()} {
		if err := safehttp.CheckURL(u, false); err != nil {
			return nil, errors.Wrap(err, ErrInvalid, "GitHub's address must use https: "+u)
		}
	}
	return safehttp.Client(g.Client, g.Headers, g.maxDownload(), false), nil
}

// fetchSkill unpacks the commit's archive into tmp and returns the
// folder of the skill in it, refusing one that holds a link.
func fetchSkill(ctx context.Context, client *http.Client, g GitHub, ref ghRef, commit, tmp string) (string, error) {
	links, err := download(ctx, client, g.codeload()+"/"+ref.owner+"/"+ref.repo+"/tar.gz/"+commit, ref.path, tmp, g.limits())
	if err != nil {
		return "", err
	}
	skillDir, err := locate(tmp, ref, commit, g.host())
	if err != nil {
		return "", err
	}
	if l := linkInside(links, skillDir); l != "" {
		return "", errors.New(ErrInvalid, "the skill holds a link or special file: "+l)
	}
	return skillDir, nil
}

// describe tells what a skill just installed holds, from the catalog.
func describe(dir string, info Info, source string) (Installed, error) {
	c, err := Open(dir)
	if err != nil {
		return Installed{}, err
	}
	files, err := c.Files(info.Name)
	if err != nil {
		return Installed{}, err
	}
	return Installed{Info: info, Source: source, Files: files, Scripts: c.scripts(info.Name)}, nil
}

// ghRef is a parsed link: a repository, a ref (empty: the default
// branch) and a folder in it (empty: the root).
type ghRef struct {
	owner, repo, ref, path string
}

func (r ghRef) source(host, dir, commit string) string {
	s := host + "/" + r.owner + "/" + r.repo
	if dir != "." && dir != "" {
		s += "/" + dir
	}
	return s + "@" + commit
}

var (
	ghOwner = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}$`) // GitHub's rule: no dots, so no host reads as an owner
	ghName  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	ghRefs  = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,255}$`)
	ghSHA   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// parseLink reads the link forms InstallGitHub accepts.
func parseLink(link, host string) (ghRef, error) {
	link = strings.TrimSpace(link)
	if host == defaultHost {
		if r, ok, err := parseRaw(link); ok {
			return r, err
		}
	}
	rest, ref, err := splitHost(link, host)
	if err != nil {
		return ghRef{}, err
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 {
		return ghRef{}, badLink()
	}
	r := ghRef{owner: parts[0], repo: strings.TrimSuffix(parts[1], ".git"), ref: ref}
	tail, err := r.web(parts[2:])
	if err != nil {
		return ghRef{}, err
	}
	r.path = strings.Join(tail, "/")
	return r, r.check()
}

// web reads the ref of a web link (…/tree/ref/… or …/blob/ref/…) into r
// and returns the folder's parts; a SKILL.md at the end names its folder.
func (r *ghRef) web(tail []string) ([]string, error) {
	if len(tail) == 0 || (tail[0] != "tree" && tail[0] != "blob") {
		return tail, nil
	}
	if len(tail) < 2 || r.ref != "" {
		return nil, badLink()
	}
	kind := tail[0]
	r.ref, tail = tail[1], tail[2:]
	if kind == "blob" && len(tail) > 0 && tail[len(tail)-1] == skillFile {
		tail = tail[:len(tail)-1]
	}
	return tail, nil
}

// rawHost serves a file of a repository as it is: the link people copy
// from a SKILL.md's "Raw" button.
const rawHost = "raw.githubusercontent.com"

// parseRaw reads https://raw.githubusercontent.com/owner/repo/ref/path/
// SKILL.md as the skill in path at ref; ok is false for another host.
func parseRaw(link string) (ghRef, bool, error) {
	u, err := url.Parse(link)
	if err != nil || !strings.EqualFold(u.Host, rawHost) {
		return ghRef{}, false, nil
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Scheme != "https" || u.User != nil || len(parts) < 3 {
		return ghRef{}, true, badLink()
	}
	r := ghRef{owner: parts[0], repo: parts[1], ref: parts[2]}
	tail := parts[3:]
	if len(tail) > 0 && tail[len(tail)-1] == skillFile {
		tail = tail[:len(tail)-1]
	}
	r.path = strings.Join(tail, "/")
	return r, true, r.check()
}

// splitHost drops the scheme, the host, a query and a fragment, and an
// @ref at the end of the short form.
func splitHost(link, host string) (string, string, error) {
	if strings.Contains(link, "://") {
		u, err := url.Parse(link)
		if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, host) || u.User != nil {
			return "", "", errors.New(ErrInvalid, "the link must be https://"+host+"/owner/repo…")
		}
		return u.Path, "", nil
	}
	link, _, _ = strings.Cut(link, "#")
	link, _, _ = strings.Cut(link, "?")
	if n := len(host) + 1; len(link) >= n && strings.EqualFold(link[:n], host+"/") {
		link = link[n:]
	}
	if i := strings.LastIndex(link, "@"); i >= 0 {
		return link[:i], link[i+1:], nil
	}
	return link, "", nil
}

func badLink() error {
	return errors.New(ErrInvalid, "not a GitHub link: use https://github.com/owner/repo, a folder of it, or owner/repo/folder@ref")
}

func (r ghRef) check() error {
	if !r.validRepo() {
		return badLink()
	}
	if r.ref != "" && (!ghRefs.MatchString(r.ref) || strings.Contains(r.ref, "..")) {
		return errors.New(ErrInvalid, "not a valid branch, tag or commit: "+r.ref)
	}
	if r.path != "" && !fsLocal(r.path) {
		return errors.New(ErrInvalid, "not a folder of the repository: "+r.path)
	}
	return nil
}

func (r ghRef) validRepo() bool {
	return ghOwner.MatchString(r.owner) && ghName.MatchString(r.repo) && r.repo != "." && r.repo != ".."
}

// resolveCommit asks the API which commit ref names now.
func resolveCommit(ctx context.Context, client *http.Client, api string, r ghRef) (string, error) {
	ref := or(r.ref, "HEAD")
	if ghSHA.MatchString(ref) {
		return ref, nil
	}
	u := api + "/repos/" + r.owner + "/" + r.repo + "/commits/" + escapeRef(ref)
	body, err := get(ctx, client, u, "application/vnd.github.sha")
	if err != nil {
		return "", err
	}
	defer closeQuietly(body)
	data, err := io.ReadAll(io.LimitReader(body, 128))
	sha := strings.TrimSpace(string(data))
	if err != nil || !ghSHA.MatchString(sha) {
		return "", errors.New(ErrFetch, "GitHub did not say which commit "+ref+" is")
	}
	return sha, nil
}

// escapeRef escapes each part of a ref, keeping its slashes: the API
// takes feature/x as it is, not as feature%2Fx.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// get fetches u, turning GitHub's refusals into what the model can act on.
func get(ctx context.Context, client *http.Client, u, accept string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.Wrap(err, ErrFetch, "cannot build the request")
	}
	req.Header.Set("Accept", accept)
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, ErrFetch, "cannot reach GitHub").WithUserMessage("GitHub could not be reached.")
	}
	if resp.StatusCode == http.StatusOK {
		return resp.Body, nil
	}
	closeQuietly(resp.Body)
	return nil, statusError(resp.StatusCode)
}

func statusError(code int) error {
	switch code {
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		return fetchError("Not found on GitHub: check the link and the branch (a private repository needs a token).")
	case http.StatusUnauthorized:
		return fetchError("GitHub refused the token.")
	case http.StatusForbidden, http.StatusTooManyRequests:
		return fetchError("GitHub refused the request, probably its rate limit: try later, or set a token.")
	}
	return fetchError("GitHub answered with an error: try later.")
}

func fetchError(msg string) error {
	return errors.New(ErrFetch, msg).WithUserMessage(msg)
}

type installIn struct {
	Link string `json:"link" desc:"The GitHub link the user gave: a repository, a folder of it, a SKILL.md, or owner/repo/folder@ref"`
}

// InstallTool is install_skill: the agent installs the skill a GitHub
// link points to into the catalog directory dir, as coding agents install
// what their user pastes. It is destructive and reaches the network, so
// the host's policy asks the user first, showing the link. installed,
// when not nil, hears of each skill installed (to reload the catalog).
func InstallTool(dir string, g GitHub, installed func(Installed)) agent.Tool {
	return agent.NewTool("install_skill",
		"Install a skill from a GitHub link the user gave (a repository or folder with a SKILL.md). "+
			"Use it only when the user asks to install a skill. The exact commit is fetched and recorded.",
		func(ctx context.Context, in installIn) (string, error) {
			s, err := InstallGitHub(ctx, in.Link, dir, g)
			if err != nil {
				return "", modelError(err)
			}
			if installed != nil {
				installed(s)
			}
			return s.summary(), nil
		}, agent.Destructive(), agent.OpenWorld())
}

// summary tells the model, and through it the user, what was installed.
func (s Installed) summary() string {
	var b strings.Builder
	b.WriteString("Installed skill " + s.Name + " from " + s.Source + ": " + s.Description)
	b.WriteString("\nFiles: " + strings.Join(s.Files, ", "))
	if len(s.Scripts) > 0 {
		b.WriteString("\nScripts (they run only in the sandbox): " + strings.Join(s.Scripts, ", "))
	}
	b.WriteString("\nLater runs list it; load it with load_skill.")
	return b.String()
}

// modelError keeps a message the model can act on: a refusal of the
// link, the skill or GitHub's answer; anything else is a code.
func modelError(err error) error {
	var coded *errors.Error
	if stderrors.As(err, &coded) && coded.UserMsg == "" && (coded.Code == ErrInvalid || coded.Code == ErrExists) {
		return coded.WithUserMessage(coded.Message)
	}
	return err
}
