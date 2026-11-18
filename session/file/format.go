// format.go: the on-disk format of a session
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package file

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// A session file is JSON Lines: a header, then one line per message.
//
//	{"format":"ringil-session","version":1}
//	{"seq":1,"msg":{...},"hash":"<hex>"}
//
// Lines are only ever appended. Each hash covers the previous hash, the
// sequence number and the exact message bytes, so a line edited, reordered
// or removed before the last one breaks the chain. Lines cut from the end
// cannot be detected without an external record of the length; the effect
// is bounded, since a lost tool result is reported as interrupted and never
// re-run. The chain detects damage and accidental edits; it is not a
// signature, and someone able to write the file can rewrite it too.
const (
	formatName    = "ringil-session"
	formatVersion = 1
)

var genesis = sha256.Sum256([]byte("ringil-session-v1"))

type header struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
}

type line struct {
	Seq  int64           `json:"seq"`
	Msg  json.RawMessage `json:"msg"`
	Hash string          `json:"hash"`
}

func chain(prev [32]byte, seq int64, msg []byte) [32]byte {
	h := sha256.New()
	h.Write(prev[:])
	h.Write([]byte("\n" + strconv.FormatInt(seq, 10) + "\n"))
	h.Write(msg)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func headerLine() []byte {
	return []byte(`{"format":"` + formatName + `","version":` + strconv.Itoa(formatVersion) + "}\n")
}

// encode turns messages into lines continuing the chain from (seq, prev).
func encode(msgs []agent.Message, seq int64, prev [32]byte) ([]byte, int64, [32]byte, error) {
	var buf bytes.Buffer
	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			return nil, seq, prev, errors.Wrap(err, agent.ErrSessionIO, "message does not encode")
		}
		seq++
		prev = chain(prev, seq, data)
		out, err := json.Marshal(line{Seq: seq, Msg: data, Hash: hex.EncodeToString(prev[:])})
		if err != nil {
			return nil, seq, prev, errors.Wrap(err, agent.ErrSessionIO, "line does not encode")
		}
		buf.Write(out)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), seq, prev, nil
}

// decoded is the state recovered from a session file.
type decoded struct {
	msgs    []agent.Message
	seq     int64
	prev    [32]byte
	checker *agent.HistoryChecker
	// valid is the length of the file that holds verified lines. A crash
	// can leave a partial last line; it was never acknowledged, so it is
	// cut off. A complete line that fails to verify is damage, not a crash.
	valid int64
	// unterminated is set when the last line verifies but lost its newline
	// (an editor or sync tool can strip it); the newline is put back
	// instead of dropping an acknowledged message.
	unterminated bool
}

func decode(data []byte) (*decoded, error) {
	d := &decoded{prev: genesis, checker: agent.NewHistoryChecker()}
	end := bytes.LastIndexByte(data, '\n') + 1
	lines := bytes.Split(bytes.TrimSuffix(data[:end], []byte("\n")), []byte("\n"))
	if end == 0 {
		return d, checkPartialHeader(data)
	}
	if err := checkHeader(lines[0]); err != nil {
		return nil, err
	}
	for _, raw := range lines[1:] {
		if err := d.add(raw); err != nil {
			return nil, err
		}
	}
	d.valid = int64(end)
	d.tail(data[end:])
	return d, nil
}

// checkPartialHeader accepts an empty file or a header cut by a crash, and
// refuses anything else: a foreign file must not be overwritten.
func checkPartialHeader(data []byte) error {
	if !bytes.HasPrefix(headerLine(), data) {
		return corrupt("not a session file")
	}
	return nil
}

// tail keeps a final line that verifies despite its missing newline.
func (d *decoded) tail(rest []byte) {
	if len(bytes.TrimSpace(rest)) == 0 {
		return
	}
	saved := *d
	if d.add(bytes.TrimSuffix(rest, []byte("\r"))) != nil {
		*d = saved
		return
	}
	d.valid += int64(len(rest))
	d.unterminated = true
}

func corrupt(msg string) error { return errors.New(agent.ErrSessionCorrupt, msg) }

func checkHeader(raw []byte) error {
	var h header
	if err := strictUnmarshal(raw, &h); err != nil || h.Format != formatName {
		return corrupt("not a session file")
	}
	if h.Version != formatVersion {
		return corrupt("unsupported session format version " + strconv.Itoa(h.Version))
	}
	return nil
}

func (d *decoded) add(raw []byte) error {
	var l line
	if err := strictUnmarshal(raw, &l); err != nil {
		return corrupt("line " + strconv.FormatInt(d.seq+1, 10) + " is not valid")
	}
	if l.Seq != d.seq+1 {
		return corrupt("line " + strconv.FormatInt(d.seq+1, 10) + " is out of sequence")
	}
	next := chain(d.prev, l.Seq, l.Msg)
	if l.Hash != hex.EncodeToString(next[:]) {
		return corrupt("line " + strconv.FormatInt(l.Seq, 10) + " does not match the hash chain")
	}
	var msg agent.Message
	if err := strictUnmarshal(l.Msg, &msg); err != nil {
		return corrupt("line " + strconv.FormatInt(l.Seq, 10) + " holds an invalid message")
	}
	if err := d.checker.Add(msg); err != nil {
		return err
	}
	d.msgs, d.seq, d.prev = append(d.msgs, msg), l.Seq, next
	return nil
}

func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return corrupt("trailing data")
	}
	return nil
}
