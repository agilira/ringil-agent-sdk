// files_test.go: what comes with a message, and fetching it
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// withFiles serves the Bot API and its file storage.
func withFiles(t *testing.T, files map[string]string) (*botAPI, *Channel) {
	t.Helper()
	b, _ := newBotAPI(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/file/bot"+botToken+"/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/file/bot"+botToken+"/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	mux.Handle("/", http.HandlerFunc(b.serve))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return b, open(t, srv.URL)
}

func TestFilesOfAMessage(t *testing.T) {
	b, c := withFiles(t, nil)
	b.set("getUpdates", `{"ok":true,"result":[{"update_id":1,"message":{"chat":{"id":7},"from":{"id":5},"caption":"look",
		"photo":[{"file_id":"small","file_size":100},{"file_id":"big","file_size":900000},{"file_id":"huge","file_size":9000000}],
		"voice":{"file_id":"v","mime_type":"audio/ogg","file_size":10},
		"document":{"file_id":"d","file_name":"report.pdf","mime_type":"application/pdf","file_size":20},
		"video_note":{"file_id":"n"}}}]}`, `{"ok":true,"result":[]}`)
	m, err := c.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Text != "look" || len(m.Files) != 4 {
		t.Fatalf("%+v", m)
	}
	type seen struct {
		name, mime string
		size       int64
		voice      bool
	}
	want := []seen{{"photo.jpg", "image/jpeg", 900000, false}, {"", "audio/ogg", 10, true}, {"report.pdf", "application/pdf", 20, false}, {"", "application/octet-stream", 0, false}}
	for i, f := range m.Files {
		if got := (seen{f.Name, f.MIME, f.Size, f.Voice}); got != want[i] {
			t.Errorf("file %d: %+v", i, got)
		}
	}
}

func TestASlowAlbumIsOneMessage(t *testing.T) {
	b, c := withFiles(t, nil)
	part := func(id int, file string) string {
		return `{"ok":true,"result":[{"update_id":` + strconv.Itoa(id) + `,"message":{"chat":{"id":7},"from":{"id":5},"media_group_id":"g","photo":[{"file_id":"` + file + `"}]}}]}`
	}
	b.set("getUpdates", part(1, "a"), `{"ok":true,"result":[]}`, part(2, "b"), part(3, "c"), `{"ok":true,"result":[]}`)
	m, err := c.Receive(context.Background())
	if err != nil || len(m.Files) != 1 {
		t.Fatalf("the album did not stop at a quiet wait: %d files %v", len(m.Files), err)
	}
	b.set("getUpdates", part(4, "d"), part(5, "e"), part(6, "f"), `{"ok":true,"result":[]}`)
	if m, _ = c.Receive(context.Background()); len(m.Files) != 3 {
		t.Fatalf("an album arriving part by part: %d files", len(m.Files))
	}
}

func TestAnAlbumIsOneMessage(t *testing.T) {
	b, c := withFiles(t, nil)
	b.set("getUpdates",
		`{"ok":true,"result":[{"update_id":1,"message":{"chat":{"id":7},"from":{"id":5},"media_group_id":"g","caption":"two","photo":[{"file_id":"a"}]}}]}`,
		`{"ok":true,"result":[{"update_id":2,"message":{"chat":{"id":7},"from":{"id":5},"media_group_id":"g","photo":[{"file_id":"b"}]}},
			{"update_id":3,"message":{"chat":{"id":7},"from":{"id":5},"text":"after"}}]}`,
		`{"ok":true,"result":[]}`)
	m, err := c.Receive(context.Background())
	if err != nil || len(m.Files) != 2 || m.Text != "two" {
		t.Fatalf("%+v %v", m, err)
	}
	if m, _ := c.Receive(context.Background()); m.Text != "after" {
		t.Fatalf("next %+v", m)
	}
}

func TestDownload(t *testing.T) {
	b, c := withFiles(t, map[string]string{"photos/file_1.jpg": "JPEGDATA"})
	ctx := context.Background()
	b.set("getFile", `{"ok":true,"result":{"file_path":"photos/file_1.jpg","file_size":8}}`)
	if data, err := c.download(ctx, "x", 100); err != nil || string(data) != "JPEGDATA" {
		t.Fatalf("%q %v", data, err)
	}
	if _, err := c.download(ctx, "x", 4); err == nil {
		t.Fatal("a file past the bound was read")
	}
	b.set("getFile", `{"ok":true,"result":{"file_path":"photos/file_1.jpg"}}`) // size not said
	if _, err := c.download(ctx, "x", 4); err == nil {
		t.Fatal("a file past the bound was read when its size was not said")
	}
	for _, p := range []string{"../../etc/passwd", "photos/../../x", "/abs", "a?b=c", "photos/file 1.jpg"} {
		b.set("getFile", `{"ok":true,"result":{"file_path":"`+p+`"}}`)
		if _, err := c.download(ctx, "x", 100); err == nil {
			t.Errorf("path %q used", p)
		}
	}
	b.set("getFile", `{"ok":true,"result":{"file_path":"photos/missing.jpg"}}`)
	_, err := c.download(ctx, "x", 100)
	if err == nil || strings.Contains(err.Error(), botToken) {
		t.Fatalf("error %v", err)
	}
}

func TestBestPhoto(t *testing.T) {
	if _, ok := bestPhoto(nil); ok {
		t.Fatal("a photo from nothing")
	}
	for want, sizes := range map[string][]photoSize{
		"m": {{FileID: "s", Size: 10}, {FileID: "m", Size: 900}, {FileID: "l", Size: 6 << 20}},
		"c": {{FileID: "a"}, {FileID: "b"}, {FileID: "c"}},
		"a": {{FileID: "a", Size: 7 << 20}, {FileID: "b", Size: 8 << 20}},
	} {
		if p, _ := bestPhoto(sizes); p.FileID != want {
			t.Errorf("picked %s, want %s", p.FileID, want)
		}
	}
}
