// deliver.go: handing over the files a script created
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package skills

import (
	"io"

	"github.com/agilira/go-errors"

	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Delivery bounds, defaults of Options.MaxFiles and Options.MaxBytes.
const (
	defaultMaxFiles = 1000
	defaultMaxBytes = 1 << 30
)

// delivery is the outcome of one hand-over.
type delivery struct {
	dir     string   // the run's folder under Output
	saved   []string // relative paths written
	skipped int      // files left behind: hidden names, odd names, caps, errors
}

// deliverFiles copies the regular files under work into a new folder of
// their own under output, named after the skill and the time. A folder of
// its own matters: files dropped straight into a project could be picked
// up by tools that run code (.git/config, conftest.py, .envrc). Names with
// a component starting with "." or holding control characters are left
// behind, as are links and special files. Nothing is ever replaced, every
// write goes through an os.Root, and the bounds stop a script that floods
// its working directory.
func deliverFiles(work, output, skill string, lim limits) (delivery, error) {
	unlockTree(work)
	d := delivery{}
	if !hasRegular(work) {
		return d, nil
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		return d, err
	}
	room, err := lim.room(output)
	if err != nil {
		return d, err
	}
	parent, err := os.OpenRoot(output)
	if err != nil {
		return d, err
	}
	defer closeQuietly(parent)
	name, err := freeDir(parent, skill+"-"+time.Now().Format("20060102-150405"))
	if err != nil {
		return d, err
	}
	d.dir = filepath.Join(output, name)
	err = handOver(work, parent, name, &d, lim.files, room)
	if len(d.saved) == 0 {
		d.dir = "" // nothing usable: no empty folder left behind
		if rerr := parent.RemoveAll(name); err == nil {
			err = rerr
		}
	}
	return d, err
}

func handOver(work string, parent *os.Root, name string, d *delivery, files int, room int64) error {
	src, err := os.OpenRoot(work)
	if err != nil {
		return err
	}
	defer closeQuietly(src)
	dst, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	defer closeQuietly(dst)
	h := &handover{src: src, dst: dst, d: d, files: files, bytes: room}
	return fs.WalkDir(src.FS(), ".", h.visit)
}

// limits bound a hand-over: files and bytes per run, and the total the
// output folder may hold (0: no total).
type limits struct {
	files int
	bytes int64
	quota int64
}

// room is how many bytes this run may add: the per-run bound, reduced to
// what the output folder's quota still allows. Sizes are apparent sizes,
// which is what a copy writes: a sparse file costs its full length.
func (l limits) room(output string) (int64, error) {
	if l.quota <= 0 {
		return l.bytes, nil
	}
	used := int64(0)
	walkErr := filepath.WalkDir(output, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if info, ierr := e.Info(); ierr == nil {
				used += info.Size()
			}
		}
		return nil
	})
	if walkErr != nil {
		return 0, walkErr
	}
	if used >= l.quota {
		return 0, errors.New(ErrIO, "the output folder "+output+" is full; empty it to receive new files")
	}
	return min(l.bytes, l.quota-used), nil
}

type handover struct {
	src, dst *os.Root
	d        *delivery
	files    int
	bytes    int64
}

func (h *handover) visit(p string, e fs.DirEntry, err error) error {
	switch {
	case err != nil:
		h.d.skipped++
		return nil // an unreadable entry is skipped, the rest still goes
	case p == ".":
		return nil
	case !safeName(path.Base(p)):
		h.d.skipped++
		if e.IsDir() {
			return fs.SkipDir
		}
		return nil
	case !e.Type().IsRegular():
		return nil
	}
	h.take(p, e)
	return nil
}

// take copies one file if the bounds allow it.
func (h *handover) take(p string, e fs.DirEntry) {
	info, err := e.Info()
	if err != nil || h.files <= 0 || info.Size() > h.bytes || copyOne(h.src, h.dst, p) != nil {
		h.d.skipped++
		return
	}
	h.files--
	h.bytes -= info.Size()
	h.d.saved = append(h.d.saved, p)
}

// safeName refuses hidden names and names that could forge lines or
// fool a terminal.
func safeName(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

func copyOne(src, dst *os.Root, p string) error {
	in, err := src.OpenFile(p, os.O_RDONLY|nonBlock, 0)
	if err != nil {
		return err
	}
	defer closeQuietly(in)
	if dir := path.Dir(p); dir != "." {
		if err := dst.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	out, err := dst.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// freeDir creates a new folder named base, or base-1, base-2...
func freeDir(root *os.Root, base string) (string, error) {
	name := base
	for i := 1; ; i++ {
		err := root.Mkdir(name, 0o700)
		if err == nil || !os.IsExist(err) || i > 999 {
			return name, err
		}
		name = base + "-" + strconv.Itoa(i)
	}
}

func hasRegular(dir string) bool {
	found := false
	walkErr := filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return walkErr == nil && found
}

// unlockTree gives the owner access to every directory the script left,
// so that a "chmod 000" neither hides files from the hand-over nor keeps
// the working directory from being removed. Links are not followed.
func unlockTree(dir string) {
	// The top directory is ours (a temporary directory we created); a
	// script may have locked it too.
	if err := chmodIn(filepath.Dir(dir), filepath.Base(dir)); err != nil {
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return // nothing to unlock
	}
	defer closeQuietly(root)
	walkErr := fs.WalkDir(root.FS(), ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		mode := fs.FileMode(0o600)
		if e.IsDir() {
			mode = dirMode
		} else if !e.Type().IsRegular() {
			return nil // links and special files are left as they are
		}
		if cerr := root.Chmod(p, mode); cerr != nil {
			return nil // best effort: what stays locked is skipped
		}
		return nil
	})
	if walkErr != nil {
		return // nothing more to unlock
	}
}

// chmodIn opens dir's parent as a root and unlocks dir through it.
func chmodIn(parent, name string) error {
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer closeQuietly(root)
	return root.Chmod(name, dirMode)
}

// dirMode lets the owner, and nobody else, list and enter a directory.
const dirMode fs.FileMode = fs.ModeDir | 0o700

// summary tells the model where the files went, quoting names and listing
// at most a handful.
func (d delivery) summary() string {
	if len(d.saved) == 0 && d.skipped == 0 {
		return ""
	}
	var b strings.Builder
	if len(d.saved) > 0 {
		b.WriteString("\n[Files saved in " + strconv.Quote(d.dir) + ":")
		for i, p := range d.saved {
			if i == 20 {
				b.WriteString(" and " + strconv.Itoa(len(d.saved)-20) + " more")
				break
			}
			b.WriteString(" " + strconv.Quote(p))
		}
		b.WriteString("]")
	}
	if d.skipped > 0 {
		b.WriteString("\n[" + strconv.Itoa(d.skipped) + " entries not saved: hidden or unusual names, limits or errors]")
	}
	return b.String()
}
