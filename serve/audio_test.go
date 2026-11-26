// audio_test.go: voice for an app
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

type heard struct{ mime string }

func (h *heard) Transcribe(_ context.Context, audio []byte, mime string) (string, error) {
	h.mime = mime
	return "dictated " + string(audio), nil
}

type spoken struct{}

func (spoken) Synthesize(_ context.Context, text string) ([]byte, string, error) {
	return []byte("OggS" + text), "audio/ogg", nil
}

func voiceAPI(t *testing.T, v Voice) *api {
	t.Helper()
	return newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}, Voice: v})
}

func (a *api) send(method, path, ctype, body string, auth bool) (int, http.Header, string) {
	a.t.Helper()
	req, _ := http.NewRequest(method, a.url+path, strings.NewReader(body))
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", ctype)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(out)
}

// A recording becomes text, text becomes audio; the app learns which
// halves are on.
func TestVoice(t *testing.T) {
	h := &heard{}
	a := voiceAPI(t, Voice{
		Transcriber: func(context.Context) (agent.Transcriber, error) { return h, nil },
		Synthesizer: func(context.Context) (agent.Synthesizer, error) { return spoken{}, nil },
	})
	if _, _, out := a.send("GET", "/v1/audio", "", "", true); !strings.Contains(out, `"transcribe":true`) || !strings.Contains(out, `"speech":true`) {
		t.Fatalf("info %s", out)
	}
	code, _, out := a.send("POST", "/v1/audio/transcriptions", "audio/webm;codecs=opus", "hello", true)
	if code != http.StatusOK || !strings.Contains(out, `"text":"dictated hello"`) || h.mime != "audio/webm" {
		t.Fatalf("transcribe %d %s %q", code, out, h.mime)
	}
	code, hdr, out := a.send("POST", "/v1/audio/speech", "application/json", `{"text":"ciao"}`, true)
	if code != http.StatusOK || hdr.Get("Content-Type") != "audio/ogg" || out != "OggSciao" {
		t.Fatalf("speech %d %v %q", code, hdr, out)
	}
}

type badSynth struct{ mime string }

func (b badSynth) Synthesize(_ context.Context, _ string) ([]byte, string, error) {
	return []byte("<html>"), b.mime, nil
}

func TestSpeechSanitizesContentType(t *testing.T) {
	for _, m := range []string{"text/html", ""} {
		a := voiceAPI(t, Voice{Synthesizer: func(context.Context) (agent.Synthesizer, error) { return badSynth{m}, nil }})
		code, hdr, _ := a.send("POST", "/v1/audio/speech", "application/json", `{"text":"ciao"}`, true)
		if code != http.StatusOK || hdr.Get("Content-Type") != "application/octet-stream" || hdr.Get("Content-Security-Policy") != "default-src 'none'" {
			t.Fatalf("%q: code=%d ct=%q csp=%q", m, code, hdr.Get("Content-Type"), hdr.Get("Content-Security-Policy"))
		}
	}
}

func TestVoiceRefusals(t *testing.T) {
	on := voiceAPI(t, Voice{
		Transcriber: func(context.Context) (agent.Transcriber, error) { return &heard{}, nil },
		Synthesizer: func(context.Context) (agent.Synthesizer, error) { return spoken{}, nil },
	})
	off := voiceAPI(t, Voice{})
	for name, c := range map[string]struct {
		a                 *api
		path, ctype, body string
		auth              bool
		code              int
	}{
		"not audio":        {on, "/v1/audio/transcriptions", "text/plain", "x", true, http.StatusUnsupportedMediaType},
		"empty recording":  {on, "/v1/audio/transcriptions", "audio/ogg", "", true, http.StatusBadRequest},
		"too long to say":  {on, "/v1/audio/speech", "application/json", `{"text":"` + strings.Repeat("a", maxSpokenText+1) + `"}`, true, http.StatusBadRequest},
		"nothing to say":   {on, "/v1/audio/speech", "application/json", `{"text":"  "}`, true, http.StatusBadRequest},
		"no token":         {on, "/v1/audio/transcriptions", "audio/ogg", "x", false, http.StatusUnauthorized},
		"no transcription": {off, "/v1/audio/transcriptions", "audio/ogg", "x", true, http.StatusNotFound},
		"no speech":        {off, "/v1/audio/speech", "application/json", `{"text":"x"}`, true, http.StatusNotFound},
	} {
		if code, _, out := c.a.send("POST", c.path, c.ctype, c.body, c.auth); code != c.code {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	if _, _, out := off.send("GET", "/v1/audio", "", "", true); !strings.Contains(out, `"transcribe":false`) {
		t.Errorf("off info %s", out)
	}
}
