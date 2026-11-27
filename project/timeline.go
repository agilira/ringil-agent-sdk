// timeline.go: what happened on a project, in order
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package project

import (
	"bufio"
	"bytes"
	"encoding/json"
	stderrors "errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"
)

// EventKind is what happened.
type EventKind string

// What happens on a project.
const (
	ProjectCreated EventKind = "project_created"
	ProjectChanged EventKind = "project_changed"
	CardCreated    EventKind = "card_created"
	CardChanged    EventKind = "card_changed"
	CardMoved      EventKind = "card_moved"
	CardRemoved    EventKind = "card_removed"
	// Comment is a word on a card or on the project; Decision a choice
	// made, for those who come after.
	Comment  EventKind = "comment"
	Decision EventKind = "decision"
	// RunStarted and RunEnded: an agent's run on the project (the host
	// records them); Action: a change it made (a file written, a message
	// sent: the host's tools that change something).
	RunStarted EventKind = "run_started"
	RunEnded   EventKind = "run_ended"
	Action     EventKind = "action"
	// Note: a note of the project was written or removed.
	Note EventKind = "note"
)

var kinds = map[EventKind]bool{ProjectCreated: true, ProjectChanged: true, CardCreated: true, CardChanged: true, CardMoved: true,
	CardRemoved: true, Comment: true, Decision: true, RunStarted: true, RunEnded: true, Action: true, Note: true}

// Event is one line of the timeline.
type Event struct {
	// Seq numbers the project's events from 1; At is when.
	Seq int64     `json:"seq"`
	At  time.Time `json:"at"`
	// Actor is who: "agent:<name>", "user" or "user:<name>", "system".
	Actor string    `json:"actor"`
	Kind  EventKind `json:"kind"`
	// Card is the card it is about, if any; Column and Agent the card's
	// as it stood then.
	Card   string `json:"card,omitempty"`
	Column string `json:"column,omitempty"`
	Agent  string `json:"agent,omitempty"`
	// Text says what, in a line or a paragraph (at most 4 KiB).
	Text string `json:"text,omitempty"`
	// Run and Conversation: where it happened, when it was a run's.
	Run          string `json:"run,omitempty"`
	Conversation string `json:"conversation,omitempty"`
	// Start: this event handed the card to its agent (Project.Starts),
	// set by the store, on one event of a change.
	Start bool `json:"start,omitempty"`
}

// Bounds of the timeline.
const (
	maxEventText = 4 << 10
	// maxTimeline: past it, the file is set aside as timeline.<n>.jsonl
	// (n the next number: none is ever replaced) and a new one starts.
	maxTimeline = 32 << 20
	maxPage     = 1000
	// maxLine bounds a line read: one longer (written by hand) is skipped.
	maxLine = 1 << 20
	// tailRead is what is read of a file's end for its last event: far
	// above the longest line the store writes.
	tailRead = 256 << 10
)

var validActor = func(a string) bool {
	who, name, _ := strings.Cut(a, ":")
	return (who == "agent" || who == "user" || who == "system") && validWho.MatchString(name) && len(a) <= 80
}

func (e Event) check() error {
	switch {
	case !kinds[e.Kind]:
		return errors.New(ErrInput, "not an event's kind: "+string(e.Kind))
	case !validActor(e.Actor):
		return errors.New(ErrInput, "an event's actor is agent:<name>, user, user:<name> or system")
	case !text(e.Text, maxEventText):
		return errors.New(ErrInput, "an event's text is at most 4 KiB")
	case e.Card != "" && !validCard.MatchString(e.Card):
		return errors.New(ErrInput, "not a card's id: "+e.Card)
	case len(e.Run) > 128 || len(e.Conversation) > 128 || !validWho.MatchString(e.Agent):
		return errors.New(ErrInput, "an event's run, conversation or agent is too long")
	}
	return nil
}

// Log adds an event to the project's timeline: a comment, a decision, or
// what the host saw (a run, an action). Its Seq and At are the store's.
func (s *Store) Log(id string, e Event) (Event, error) {
	if e.Actor == "" {
		e.Actor = "system"
	}
	if err := e.check(); err != nil {
		return Event{}, err
	}
	var out Event
	err := s.locked(id, func() error {
		p, err := s.Get(id)
		if err != nil {
			return err
		}
		if err := writable(p); err != nil {
			return err
		}
		if e.Card != "" {
			if _, err := s.Card(id, e.Card); err != nil {
				return err
			}
		}
		out, err = s.appendEvent(id, e)
		return err
	})
	return out, err
}

// appendEvent writes e as the next line; the caller holds the lock.
func (s *Store) appendEvent(id string, e Event) (Event, error) {
	if e.Actor == "" {
		e.Actor = "system"
	}
	if err := e.check(); err != nil {
		return Event{}, err
	}
	last, err := s.lastSeq(id)
	if err != nil {
		return Event{}, err
	}
	if err := s.rotate(id); err != nil {
		return Event{}, err
	}
	e.Seq, e.At = last+1, s.now()
	line, err := encode(e)
	if err != nil {
		return Event{}, errors.Wrap(err, ErrStore, "cannot encode an event")
	}
	if err := s.appendLine(filepath.Join(id, timelineFile), line); err != nil {
		return Event{}, errors.Wrap(err, ErrStore, "cannot write the timeline of "+id)
	}
	return e, nil
}

// encode is e as one line, without HTML escapes: a text of < and & does
// not grow six times.
func encode(e Event) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// rotate sets the current file aside when it is past maxTimeline.
func (s *Store) rotate(id string) error {
	name := filepath.Join(id, timelineFile)
	info, err := s.root.Stat(name)
	if err != nil || info.Size() <= maxTimeline {
		return nil
	}
	n := 1
	if old, err := s.archives(id); err == nil && len(old) > 0 {
		n = old[len(old)-1].n + 1
	}
	// A rename that fails (Windows, a reader holding the file) is tried
	// again at the next event: the file grows a while, nothing is lost.
	_ = s.root.Rename(name, filepath.Join(id, "timeline."+strconv.Itoa(n)+".jsonl"))
	return nil
}

// appendLine adds line to the file name, synced.
func (s *Store) appendLine(name string, line []byte) error {
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	if serr := f.Sync(); werr == nil {
		werr = serr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// archive is a timeline file set aside, by its number.
type archive struct {
	n    int
	name string
}

var archiveName = regexp.MustCompile(`^timeline\.([1-9][0-9]{0,8})\.jsonl$`)

// archives are the project's files set aside, the oldest first.
func (s *Store) archives(id string) ([]archive, error) {
	f, err := s.root.Open(id)
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot list the timeline of "+id)
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot list the timeline of "+id)
	}
	var out []archive
	for _, e := range entries {
		if m := archiveName.FindStringSubmatch(e.Name()); m != nil && e.Type().IsRegular() {
			n, _ := strconv.Atoi(m[1])
			out = append(out, archive{n: n, name: filepath.Join(id, e.Name())})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].n < out[j].n })
	return out, nil
}

// files are the project's timeline files in order: those set aside, then
// the current one.
func (s *Store) files(id string) ([]string, error) {
	old, err := s.archives(id)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(old)+1)
	for _, a := range old {
		out = append(out, a.name)
	}
	return append(out, filepath.Join(id, timelineFile)), nil
}

// lastSeq is the Seq of the project's last event, 0 for none.
func (s *Store) lastSeq(id string) (int64, error) {
	files, err := s.files(id)
	if err != nil {
		return 0, err
	}
	for i := len(files) - 1; i >= 0; i-- {
		seq, err := s.tailSeq(files[i])
		if err != nil || seq > 0 {
			return seq, err
		}
	}
	return 0, nil
}

// tailSeq is the Seq of the last event a timeline file holds that reads.
func (s *Store) tailSeq(name string) (int64, error) {
	f, err := s.root.Open(name)
	if stderrors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, errors.Wrap(err, ErrStore, "cannot read "+name)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0, errors.New(ErrStore, name+" is not a file")
	}
	from := max(0, info.Size()-tailRead)
	buf := make([]byte, info.Size()-from)
	if _, err := f.ReadAt(buf, from); err != nil && err != io.EOF {
		return 0, errors.Wrap(err, ErrStore, "cannot read "+name)
	}
	if seq := lastSeqIn(buf); seq > 0 || from == 0 {
		return seq, nil
	}
	return s.lastSeqBack(name)
}

// lastSeqBack scans backwards through name when the tail window holds no
// valid event (such as an oversized damaged line at the end).
func (s *Store) lastSeqBack(name string) (int64, error) {
	var seq int64
	err := s.eachEventBack(name, func(e Event) bool {
		seq = e.Seq
		return false
	})
	return seq, err
}

// lastSeqIn is the Seq of the last event that reads in buf, 0 for none.
func lastSeqIn(buf []byte) int64 {
	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte("\n"))
	for _, line := range slices.Backward(lines) {
		var e Event
		if json.Unmarshal(line, &e) == nil && e.Seq > 0 {
			return e.Seq
		}
	}
	return 0
}

// eachEvent calls fn for each event of a file that reads, in order,
// until fn says stop; a line too long or damaged is skipped.
func (s *Store) eachEvent(name string, fn func(Event) bool) error {
	f, err := s.root.Open(name)
	if stderrors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := readLine(r)
		if e, ok := eventFrom(line, 0); ok && !fn(e) {
			return nil
		}
		if err != nil {
			return ignoreEOF(err)
		}
	}
}

func ignoreEOF(err error) error {
	if err == io.EOF {
		return nil
	}
	return err
}

// readLine is the next line of r without its end; one past maxLine comes
// back empty (it is skipped whole).
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	tooLong := false
	for {
		part, err := r.ReadSlice('\n')
		if !tooLong {
			line = append(line, part...)
			tooLong = len(line) > maxLine
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if tooLong {
			return nil, err
		}
		return bytes.TrimRight(line, "\n"), err
	}
}

// Timeline is a page of the project's events in order: limit of them
// from Seq from (from <= 0: the last limit), and the last Seq there is.
// It reads across the files set aside; a line that cannot be read
// (written by hand, damaged) is skipped.
func (s *Store) Timeline(id string, from int64, limit int) ([]Event, int64, error) {
	if _, err := s.Get(id); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > maxPage {
		limit = 100
	}
	files, err := s.files(id)
	if err != nil {
		return nil, 0, err
	}
	var page []Event
	if from > 0 {
		page, err = s.pageFrom(files, from, limit)
	} else {
		page, err = s.lastPage(files, limit)
	}
	if err != nil {
		return nil, 0, errors.Wrap(err, ErrStore, "cannot read the timeline of "+id)
	}
	last, err := s.lastSeq(id)
	return page, last, err
}

// pageFrom is limit events from Seq from, the files read oldest first.
func (s *Store) pageFrom(files []string, from int64, limit int) ([]Event, error) {
	page := []Event{}
	for _, name := range files {
		last, err := s.tailSeq(name)
		if err != nil {
			return nil, err
		}
		if last > 0 && last < from {
			continue // the whole file is before from
		}
		err = s.eachEvent(name, func(e Event) bool {
			if e.Seq >= from {
				page = append(page, e)
			}
			return len(page) < limit
		})
		if err != nil || len(page) == limit {
			return page, err
		}
	}
	return page, nil
}

// lastPage is the last limit events, read from the end: the cost is the
// page's, not the history's.
func (s *Store) lastPage(files []string, limit int) ([]Event, error) {
	page := []Event{}
	err := s.backThrough(files, func(e Event) bool {
		page = append(page, e)
		return len(page) < limit
	})
	slices.Reverse(page)
	return page, err
}

// maxCardScan bounds the events read back for a card's: a card's latest
// are near the end, and an old one's are not worth reading the history.
const maxCardScan = 20000

// CardEvents are the last limit events about a card, in order, read from
// the end of the timeline (at most its last 20000 events).
func (s *Store) CardEvents(id, card string, limit int) ([]Event, error) {
	if _, err := s.Get(id); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxPage {
		limit = 100
	}
	files, err := s.files(id)
	if err != nil {
		return nil, err
	}
	var out []Event
	read := 0
	more := func(e Event) bool {
		read++
		if e.Card == card {
			out = append(out, e)
		}
		return len(out) < limit && read < maxCardScan
	}
	if err := s.backThrough(files, more); err != nil {
		return nil, errors.Wrap(err, ErrStore, "cannot read the timeline of "+id)
	}
	slices.Reverse(out)
	return out, nil
}

// backThrough reads the files' events, the newest first, until fn says
// stop.
func (s *Store) backThrough(files []string, fn func(Event) bool) error {
	stopped := false
	wrapped := func(e Event) bool {
		if !fn(e) {
			stopped = true
			return false
		}
		return true
	}
	for _, name := range slices.Backward(files) {
		if err := s.eachEventBack(name, wrapped); err != nil || stopped {
			return err
		}
	}
	return nil
}

// backChunk is what eachEventBack reads at a time.
const backChunk = 64 << 10

// eachEventBack calls fn for each event of a file that reads, the last
// first, until fn says stop; a line too long or damaged is skipped.
func (s *Store) eachEventBack(name string, fn func(Event) bool) error {
	f, err := s.root.Open(name)
	if stderrors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var carry []byte // the start of a line whose end was read
	for end := info.Size(); end > 0; {
		start := max(0, end-backChunk)
		var lines [][]byte
		lines, carry, err = chunkLines(f, start, end, carry)
		if err != nil {
			return err
		}
		if !eachLineBack(lines, fn) {
			return nil
		}
		end = start
	}
	return nil
}

// chunkLines reads the file from start to end, followed by carry, and
// splits it in lines: the first, which may go on before start, comes
// back as the next carry (none at the file's start, or past maxLine).
func chunkLines(f *os.File, start, end int64, carry []byte) ([][]byte, []byte, error) {
	buf := make([]byte, end-start, end-start+int64(len(carry)))
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, nil, err
	}
	lines := bytes.Split(append(buf, carry...), []byte("\n"))
	if start == 0 {
		return lines, nil, nil
	}
	next := lines[0]
	if len(next) > maxLine {
		next = nil // a line too long: what is left of it is skipped too
	}
	return lines[1:], next, nil
}

// eachLineBack calls fn on the events of lines, the last first; false
// when fn said stop.
func eachLineBack(lines [][]byte, fn func(Event) bool) bool {
	for _, line := range slices.Backward(lines) {
		if e, ok := eventFrom(line, 0); ok && !fn(e) {
			return false
		}
	}
	return true
}

// eventFrom is the event of a line, when it reads and is not before from.
func eventFrom(line []byte, from int64) (Event, bool) {
	var e Event
	if len(line) == 0 || json.Unmarshal(line, &e) != nil || e.Seq <= 0 || (from > 0 && e.Seq < from) {
		return Event{}, false
	}
	return e, true
}
