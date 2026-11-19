// transcribe_test.go: speech to text against a fake endpoint
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

type sentAudio struct{ path, auth, model, name, mime, audio string }

func TestTranscribe(t *testing.T) {
	var got sentAudio
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = readAudio(t, r)
		_, _ = io.WriteString(w, `{"text":" ciao a tutti "}`)
	}))
	defer srv.Close()
	tr, err := NewTranscriber(Config{BaseURL: srv.URL + "/v1", Model: "whisper-1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	text, err := tr.Transcribe(context.Background(), []byte("OggS..."), "Audio/OGG; codecs=opus\r\nX-Injected: 1")
	if err != nil || text != "ciao a tutti" {
		t.Fatalf("%q %v", text, err)
	}
	if want := (sentAudio{"/v1/audio/transcriptions", "Bearer k", "whisper-1", "audio.ogg", "audio/ogg", "OggS..."}); got != want {
		t.Fatalf("request %+v", got)
	}
}

func TestTranscribeRefuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "bad") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"nope":1}`)
	}))
	defer srv.Close()
	tr, err := NewTranscriber(Config{BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := tr.Transcribe(ctx, []byte("x"), "video/mp4"); !errors.HasCode(err, agent.ErrProviderInput) {
		t.Errorf("video taken: %v", err)
	}
	if _, err := tr.Transcribe(ctx, nil, "audio/ogg"); !errors.HasCode(err, agent.ErrProviderInput) {
		t.Errorf("empty taken: %v", err)
	}
	if _, err := tr.Transcribe(ctx, []byte("x"), "audio/ogg"); !errors.HasCode(err, agent.ErrProviderResponse) {
		t.Errorf("reply without text: %v", err)
	}
	bad, _ := NewTranscriber(Config{BaseURL: srv.URL, Model: "m", APIKey: "bad"})
	if _, err := bad.Transcribe(ctx, []byte("x"), "audio/ogg"); !errors.HasCode(err, agent.ErrProviderAuth) {
		t.Errorf("refused key: %v", err)
	}
}

func readAudio(t *testing.T, r *http.Request) sentAudio {
	got := sentAudio{path: r.URL.Path, auth: r.Header.Get("Authorization")}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Error(err)
		return got
	}
	got.model = r.FormValue("model")
	f, h, err := r.FormFile("file")
	if err != nil {
		t.Error(err)
		return got
	}
	data, _ := io.ReadAll(f)
	got.name, got.mime, got.audio = h.Filename, h.Header.Get("Content-Type"), string(data)
	return got
}
