// audio.go: voice for an app, speech to text and text to speech
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"context"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Bounds of the voice routes: the audio of one recording, the text of one
// spoken answer (an app reads a long answer in parts).
const (
	maxAudio      = 25 << 20
	maxSpokenText = 4096
)

// audioTypes are what a browser records and the transcription APIs take.
var audioTypes = map[string]bool{
	"audio/webm": true, "audio/ogg": true, "audio/opus": true, "audio/mpeg": true, "audio/mp3": true,
	"audio/wav": true, "audio/x-wav": true, "audio/mp4": true, "audio/m4a": true, "audio/x-m4a": true,
}

// Voice is what the voice routes use, asked at each request so that a
// model changed in the settings counts at once: nil, or one that answers
// nil, means that half of the voice is off.
type Voice struct {
	Transcriber func(ctx context.Context) (agent.Transcriber, error)
	Synthesizer func(ctx context.Context) (agent.Synthesizer, error)
}

func (v Voice) transcriber(ctx context.Context) (agent.Transcriber, error) {
	if v.Transcriber == nil {
		return nil, nil
	}
	return v.Transcriber(ctx)
}

func (v Voice) synthesizer(ctx context.Context) (agent.Synthesizer, error) {
	if v.Synthesizer == nil {
		return nil, nil
	}
	return v.Synthesizer(ctx)
}

// audioInfo says which halves of the voice are on, for an app to show or
// hide its microphone and its speaker.
func (s *Server) audioInfo(w http.ResponseWriter, r *http.Request) {
	t, terr := s.cfg.Voice.transcriber(r.Context())
	sy, serr := s.cfg.Voice.synthesizer(r.Context())
	respond(w, http.StatusOK, map[string]bool{"transcribe": terr == nil && t != nil, "speech": serr == nil && sy != nil})
}

// transcribe turns a recording into text: the body is the audio, its
// Content-Type the format.
func (s *Server) transcribe(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !audioTypes[media] {
		failure(w, http.StatusUnsupportedMediaType, fail(ErrInput, "send the recording as audio/webm, ogg, mpeg, wav or mp4"))
		return
	}
	t, err := s.cfg.Voice.transcriber(r.Context())
	if err != nil || t == nil {
		failure(w, http.StatusNotFound, fail(ErrNotFound, "no transcription model is set (transcribe: in the settings)"))
		return
	}
	audio, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAudio))
	if err != nil || len(audio) == 0 {
		failure(w, http.StatusBadRequest, fail(ErrInput, "the recording is empty or larger than 25 MiB"))
		return
	}
	text, err := t.Transcribe(r.Context(), audio, media)
	if err != nil {
		failure(w, http.StatusBadGateway, fail("SERVE_VOICE", "the transcription failed: "+agent.ModelMessage(err)))
		return
	}
	respond(w, http.StatusOK, map[string]string{"text": text})
}

type speechIn struct {
	Text string `json:"text"`
}

// speak turns text into audio, answered as the audio itself.
func (s *Server) speak(w http.ResponseWriter, r *http.Request) {
	var in speechIn
	if !readJSON(w, r, &in) {
		return
	}
	text := strings.TrimSpace(in.Text)
	if text == "" || utf8.RuneCountInString(text) > maxSpokenText {
		failure(w, http.StatusBadRequest, fail(ErrInput, "the text to speak is 1 to 4096 characters: read a long answer in parts"))
		return
	}
	sy, err := s.cfg.Voice.synthesizer(r.Context())
	if err != nil || sy == nil {
		failure(w, http.StatusNotFound, fail(ErrNotFound, "no speech model is set (speech: in the settings)"))
		return
	}
	audio, kind, err := sy.Synthesize(r.Context(), text)
	if err != nil {
		failure(w, http.StatusBadGateway, fail("SERVE_VOICE", "the speech failed: "+agent.ModelMessage(err)))
		return
	}
	w.Header().Set("Content-Type", safeAudioType(kind))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(audio)
}

func safeAudioType(kind string) string {
	media, _, err := mime.ParseMediaType(kind)
	if err == nil && strings.HasPrefix(media, "audio/") {
		return media
	}
	return "application/octet-stream"
}
