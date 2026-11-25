// archive.go: unpacking a repository archive and finding its skill
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"archive/tar"
	"compress/gzip"
	"context"
	stderrors "errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// maxStream bounds what the archive inflates to, whatever part of it is
// kept: a small download must not expand without end.
const maxStream = 1 << 30

// maxDepth is how deep under the link's folder skills are looked for.
const maxDepth = 4

// maxListed is how many skills a refusal lists.
const maxListed = 20

// unpack is the state of one archive being unpacked under a folder.
type unpack struct {
	root   *os.Root
	prefix string
	lim    limits
	files  int
	bytes  int64
	links  []string // entries left out: links and special files
}

// download fetches the archive at u and unpacks the entries under prefix
// into dst, returning the links and special files it left out.
func download(ctx context.Context, client *http.Client, u, prefix, dst string, lim limits) ([]string, error) {
	body, err := get(ctx, client, u, "application/x-gzip")
	if err != nil {
		return nil, err
	}
	defer closeQuietly(body)
	gz, err := gzip.NewReader(body)
	if err != nil {
		return nil, fetchError("GitHub sent an archive that cannot be read.")
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return nil, errors.Wrap(err, ErrIO, "cannot prepare the download")
	}
	defer closeQuietly(root)
	u2 := &unpack{root: root, prefix: prefix, lim: lim}
	if err := u2.all(ctx, tar.NewReader(io.LimitReader(gz, maxStream))); err != nil {
		return nil, err
	}
	if u2.files == 0 {
		return nil, fetchError("Nothing found at " + or(prefix, "the root") + " in that commit: check the folder.")
	}
	return u2.links, nil
}

func (u *unpack) all(ctx context.Context, tr *tar.Reader) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if stderrors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fetchError("GitHub sent an archive that cannot be read.")
		}
		if err := u.entry(hdr, tr); err != nil {
			return err
		}
	}
}

// entry keeps one archive entry if it lies under the prefix. GitHub's
// archives put everything under one top folder (owner-repo-commit/).
func (u *unpack) entry(hdr *tar.Header, r io.Reader) error {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return nil
	}
	rel, ok := u.relative(hdr.Name)
	if !ok {
		return nil
	}
	if !fsLocal(rel) {
		return errors.New(ErrInvalid, "the archive holds an unsafe path: "+rel)
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return u.mkdir(rel)
	case tar.TypeReg:
		return u.file(rel, hdr.Size, r)
	}
	u.links = append(u.links, rel)
	return nil
}

// relative is name without the top folder and the prefix; ok is false
// for an entry outside the prefix, or the prefix itself.
func (u *unpack) relative(name string) (string, bool) {
	_, rest, found := strings.Cut(strings.TrimPrefix(name, "./"), "/")
	rest = strings.TrimSuffix(rest, "/")
	if !found || rest == "" {
		return "", false
	}
	if u.prefix == "" {
		return rest, true
	}
	after, ok := strings.CutPrefix(rest, u.prefix+"/")
	return after, ok && after != ""
}

func (u *unpack) mkdir(rel string) error {
	if err := u.root.MkdirAll(filepath.FromSlash(rel), 0o700); err != nil {
		return errors.Wrap(err, ErrIO, "cannot unpack the archive")
	}
	return nil
}

func (u *unpack) file(rel string, size int64, r io.Reader) error {
	u.files++
	u.bytes += size
	if u.files > u.lim.files || u.bytes > u.lim.bytes || size < 0 {
		return fetchError("The folder is too big to be a skill: link the skill's own folder.")
	}
	if dir := path.Dir(rel); dir != "." {
		if err := u.mkdir(dir); err != nil {
			return err
		}
	}
	f, err := u.root.OpenFile(filepath.FromSlash(rel), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot unpack "+rel)
	}
	_, err = io.Copy(f, io.LimitReader(r, size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return errors.Wrap(err, ErrIO, "cannot unpack "+rel)
	}
	return nil
}

// fsLocal: p is a relative slash path that stays inside its root, on
// every system: no backslash, and no colon (a drive on Windows, or an
// alternate data stream of a file).
func fsLocal(p string) bool {
	return fs.ValidPath(p) && !strings.ContainsAny(p, "\\:") && filepath.IsLocal(filepath.FromSlash(p))
}

// locate finds the skill in the unpacked folder: its own SKILL.md, or the
// only folder below holding one. Several are refused, with their links.
func locate(dir string, ref ghRef, commit, host string) (string, error) {
	if st, err := os.Lstat(filepath.Join(dir, skillFile)); err == nil && st.Mode().IsRegular() {
		return ".", nil
	}
	found, err := findSkills(dir)
	if err != nil {
		return "", errors.Wrap(err, ErrIO, "cannot read the download")
	}
	switch len(found) {
	case 0:
		return "", agent.ToolError(ErrInvalid, "There is no SKILL.md there: it is not a skill.")
	case 1:
		return found[0], nil
	}
	return "", agent.ToolError(ErrInvalid, severalSkills(found, ref, commit, host))
}

// findSkills lists the folders holding a SKILL.md, not looking inside a
// skill nor in hidden folders.
func findSkills(dir string) ([]string, error) {
	var found []string
	err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case !d.IsDir() || p == ".":
			return nil
		case strings.HasPrefix(d.Name(), ".") || strings.Count(p, "/") >= maxDepth:
			return fs.SkipDir
		}
		if st, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p), skillFile)); err == nil && st.Mode().IsRegular() {
			found = append(found, p)
			return fs.SkipDir
		}
		return nil
	})
	return found, err
}

func severalSkills(found []string, ref ghRef, commit, host string) string {
	var b strings.Builder
	b.WriteString("That folder holds several skills. Ask the user which to install, then call again with its link:")
	for i, f := range found {
		if i == maxListed {
			b.WriteString("\n… and others")
			break
		}
		b.WriteString("\n" + ref.source(host, path.Join(ref.path, f), commit))
	}
	return b.String()
}

// linkInside is the first left-out entry inside the skill's folder.
func linkInside(links []string, skillDir string) string {
	for _, l := range links {
		if skillDir == "." || l == skillDir || strings.HasPrefix(l, skillDir+"/") {
			return l
		}
	}
	return ""
}
