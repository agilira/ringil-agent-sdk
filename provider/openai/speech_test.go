// speech_test.go: text to speech against a fake endpoint
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

func TestSynthesize(t *testing.T) {
	var got map[string]string
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &got)
		_, _ = io.WriteString(w, "OggS-audio")
	}))
	defer srv.Close()
	notAudio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>proxy login</html>")
	}))
	defer notAudio.Close()
	s, err := NewSynthesizer(Config{BaseURL: srv.URL + "/v1", Model: "tts-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	audio, mime, err := s.Synthesize(context.Background(), "ciao")
	if err != nil || string(audio) != "OggS-audio" || mime != "audio/ogg" {
		t.Fatalf("%q %q %v", audio, mime, err)
	}
	want := map[string]string{"model": "tts-1", "input": "ciao", "voice": "alloy", "response_format": "opus"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q", k, got[k])
		}
	}
	if path != "/v1/audio/speech" {
		t.Fatalf("path %s", path)
	}
	bad, _ := NewSynthesizer(Config{BaseURL: notAudio.URL, Model: "tts-1"}, "")
	if _, _, err := bad.Synthesize(context.Background(), "ciao"); !errors.HasCode(err, agent.ErrProviderResponse) {
		t.Fatalf("a web page taken as speech: %v", err)
	}
}

func TestSynthesizeRefusesBadText(t *testing.T) {
	s, err := NewSynthesizer(Config{BaseURL: "http://127.0.0.1:1", Model: "tts-1"}, "nova")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"", "  ", strings.Repeat("a", MaxSpeechText+1)} {
		if _, _, err := s.Synthesize(context.Background(), text); !errors.HasCode(err, agent.ErrProviderInput) {
			t.Errorf("%d characters taken", len(text))
		}
	}
}

func TestSpeechType(t *testing.T) {
	for want, c := range map[string][2]string{
		"audio/ogg":  {"OggS....", ""},
		"audio/mpeg": {"ID3\x03\x00\x00\x00\x00\x00\x00", "application/octet-stream"},
		"audio/wav":  {"not sniffable", "audio/wav; rate=24000"},
		"":           {"<html>", "text/html"},
	} {
		if got := speechType([]byte(c[0]), c[1]); got != want {
			t.Errorf("%q: %q, want %q", c[0], got, want)
		}
	}
}
