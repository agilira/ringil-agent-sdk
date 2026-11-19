// files_test.go: images, voice and documents through the bridge
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package channel

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

func pngData(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func fileOf(name, mime string, data []byte) File {
	return File{Name: name, MIME: mime, Size: int64(len(data)), Fetch: func(_ context.Context, max int64) ([]byte, error) {
		return data[:min(int64(len(data)), max+1)], nil
	}}
}

type ear struct{ heard []string }

func (e *ear) Transcribe(_ context.Context, audio []byte, mime string) (string, error) {
	e.heard = append(e.heard, mime+":"+string(audio))
	return "buy milk", nil
}

// fileRig is a rig whose bridge hears voice and keeps documents.
func fileRig(t *testing.T, steps ...fake.Step) (*rig, *Bridge, *ear, string) {
	t.Helper()
	r := newRig(t, true, steps...)
	e, inbox := &ear{}, filepath.Join(t.TempDir(), "inbox")
	b, err := NewBridge(Config{Channel: r.app, Server: r.srv, Allow: []string{me}, Transcriber: e, Inbox: inbox})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.wg.Wait)
	return r, b, e, inbox
}

func TestImageReachesTheModel(t *testing.T) {
	r, b, _, _ := fileRig(t, fake.Reply("a square"))
	b.handle(context.Background(), Message{Chat: room, From: me, Text: "what is it?", Files: []File{fileOf("p.png", "image/png", pngData(t))}})
	if got := r.app.next(t).Text; got != "a square" {
		t.Fatalf("reply %q", got)
	}
	m := r.model.Requests()[0].Messages[0]
	if m.Text() != "what is it?" || m.Images() != 1 || m.Parts[1].MIME != "image/png" {
		t.Fatalf("model got %+v", m)
	}
}

func TestAFakeImageIsRefused(t *testing.T) {
	r, b, _, _ := fileRig(t, fake.Reply("never"))
	b.handle(context.Background(), Message{Chat: room, From: me, Files: []File{fileOf("x.png", "image/png", []byte("<script>"))}})
	if got := r.app.next(t).Text; !strings.Contains(got, "JPEG, PNG") {
		t.Fatalf("reply %q", got)
	}
	if r.model.Remaining() != 1 {
		t.Fatal("the model was called")
	}
}

func TestVoiceIsHeard(t *testing.T) {
	r, b, e, _ := fileRig(t, fake.Reply("noted"))
	voice := fileOf("", "audio/ogg", []byte("OggS"))
	voice.Voice = true
	b.handle(context.Background(), Message{Chat: room, From: me, Files: []File{voice}})
	r.app.next(t)
	if len(e.heard) != 1 || e.heard[0] != "audio/ogg:OggS" {
		t.Fatalf("heard %v", e.heard)
	}
	if got := r.model.Requests()[0].Messages[0].Text(); got != "[voice message, transcribed] buy milk" {
		t.Fatalf("model got %q", got)
	}
}

func TestDocumentIsKept(t *testing.T) {
	r, b, _, inbox := fileRig(t, fake.Reply("read it"))
	b.handle(context.Background(), Message{Chat: room, From: me, Text: "summarise", Files: []File{fileOf("../../My Report.pdf", "application/pdf", []byte("%PDF"))}})
	r.app.next(t)
	got := r.model.Requests()[0].Messages[0].Text()
	dir := b.InboxOf(room)
	if dir != filepath.Join(inbox, "test-"+room) {
		t.Fatalf("inbox of the chat %q", dir)
	}
	if !strings.HasPrefix(got, "summarise\n\n[the user sent a file, saved as "+dir+string(filepath.Separator)) || !strings.HasSuffix(got, "-My_Report.pdf]") {
		t.Fatalf("model got %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("inbox %v %v", entries, err)
	}
	info, _ := entries[0].Info()
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

func TestWithoutEarOrInbox(t *testing.T) {
	r := newRig(t, true)
	b, err := NewBridge(Config{Channel: r.app, Server: r.srv, Allow: []string{me}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.wg.Wait()
	ctx := context.Background()
	voice := fileOf("", "audio/ogg", []byte("x"))
	voice.Voice = true
	b.handle(ctx, Message{Chat: room, From: me, Files: []File{voice}})
	if got := r.app.next(t).Text; !strings.Contains(got, "voice") {
		t.Fatalf("voice: %q", got)
	}
	b.handle(ctx, Message{Chat: room, From: me, Files: []File{fileOf("a.zip", "application/zip", []byte("PK"))}})
	if got := r.app.next(t).Text; !strings.Contains(got, "documents") {
		t.Fatalf("document: %q", got)
	}
}

func TestTooLarge(t *testing.T) {
	r, b, _, _ := fileRig(t)
	big := fileOf("big.bin", "application/octet-stream", nil)
	big.Size = MaxDocumentBytes + 1
	b.handle(context.Background(), Message{Chat: room, From: me, Files: []File{big}})
	if got := r.app.next(t).Text; !strings.Contains(got, "too large") {
		t.Fatalf("reply %q", got)
	}
	lying := fileOf("small.bin", "application/octet-stream", make([]byte, 100))
	lying.Size = 1
	b.cfg.Inbox = t.TempDir()
	if _, problem := b.fetch(context.Background(), room, lying, 10); !strings.Contains(problem, "too large") {
		t.Fatalf("a file larger than it said: %q", problem)
	}
	many := make([]File, maxFiles+1)
	b.handle(context.Background(), Message{Chat: room, From: me, Files: many})
	if got := r.app.next(t).Text; !strings.Contains(got, "at most") {
		t.Fatalf("reply %q", got)
	}
}

func TestStopWhileFetching(t *testing.T) {
	r, b, _, _ := fileRig(t, fake.Reply("never"))
	started := make(chan struct{})
	slow := File{Name: "x.pdf", MIME: "application/pdf", Fetch: func(ctx context.Context, _ int64) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx := context.Background()
	b.handle(ctx, Message{Chat: room, From: me, Files: []File{slow}})
	<-started
	b.handle(ctx, Message{Chat: room, From: me, Text: "/stop"})
	if got := r.app.next(t).Text; got != "Stopped." {
		t.Fatalf("reply %q", got)
	}
	if r.model.Remaining() != 1 {
		t.Fatal("the model was called")
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf": "report.pdf", "../../etc/passwd": "passwd", "..": "file", "": "file",
		".bashrc": "bashrc", `C:\x\evil.exe`: "evil.exe", "a b\x00c.txt": "a_bc.txt",
		"\u202egnp.exe": "gnp.exe",
	} {
		if got := safeName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := safeName(strings.Repeat("a", 200) + ".pdf"); len(got) != 80 || !strings.HasSuffix(got, ".pdf") {
		t.Fatalf("long: %q", got)
	}
}

func TestOtherImagesAndAudioAreDocuments(t *testing.T) {
	r := newRig(t, true, fake.Reply("ok"), fake.Reply("ok"))
	inbox := t.TempDir()
	b, err := NewBridge(Config{Channel: r.app, Server: r.srv, Allow: []string{me}, Inbox: inbox})
	if err != nil {
		t.Fatal(err)
	}
	defer b.wg.Wait()
	ctx := context.Background()
	b.handle(ctx, Message{Chat: room, From: me, Files: []File{fileOf("scan.heic", "image/heic", []byte("ftypheic"))}})
	r.app.next(t)
	b.handle(ctx, Message{Chat: room, From: me, Files: []File{fileOf("song.mp3", "audio/mpeg", []byte("ID3"))}})
	r.app.next(t)
	for i, req := range r.model.Requests() {
		if !strings.Contains(req.Messages[len(req.Messages)-1].Text(), "saved as") {
			t.Errorf("file %d not saved: %q", i, req.Messages[len(req.Messages)-1].Text())
		}
	}
}

func TestAFailedMessageLeavesNoDocument(t *testing.T) {
	r, b, _, _ := fileRig(t)
	bad := File{Name: "b.pdf", MIME: "application/pdf", Fetch: func(context.Context, int64) ([]byte, error) { return nil, os.ErrClosed }}
	b.handle(context.Background(), Message{Chat: room, From: me, Files: []File{fileOf("a.pdf", "application/pdf", []byte("%PDF")), bad}})
	if got := r.app.next(t).Text; !strings.Contains(got, "could not download") {
		t.Fatalf("reply %q", got)
	}
	if entries, _ := os.ReadDir(b.InboxOf(room)); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestACaptionIsNotAnAnswer(t *testing.T) {
	r, q := approval(t, fake.Reply("finished"))
	r.app.from(Message{Chat: room, From: me, Text: "yes", Files: []File{fileOf("p.png", "image/png", pngData(t))}})
	if got := r.app.next(t).Text; !strings.Contains(got, "still working") {
		t.Fatalf("a caption answered the question: %q", got)
	}
	r.app.press(q.Buttons[1].ID)
	r.app.next(t)
	r.app.next(t)
}
