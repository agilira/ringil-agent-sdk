// speech.go: an OpenAI-compatible /audio/speech endpoint
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/httpcall"
)

// Bounds of speech: the text the main APIs take, and the audio read back.
const (
	MaxSpeechText   = 4096
	maxSpeechReply  = 25 << 20
	defaultVoice    = "alloy"
	speechMIME      = "audio/ogg" // Opus in Ogg: what chat apps play as a voice message
	speechFormat    = "opus"
	speechAcceptHdr = "audio/ogg, audio/opus, application/octet-stream"
)

// speechTimeout bounds one synthesis; a variable for tests.
var speechTimeout = time.Minute

type synthesizer struct {
	*provider
	voice string
}

// NewSynthesizer returns a Synthesizer for cfg.Model (tts-1, gpt-4o-mini-tts,
// kokoro...) at cfg.BaseURL + "/audio/speech", speaking with voice
// (default "alloy"), in Opus; with the same key, header and transport
// rules as New.
func NewSynthesizer(cfg Config, voice string) (agent.Synthesizer, error) {
	endpoint, err := endpointURL(cfg.BaseURL, cfg.AllowHTTP)
	if err != nil {
		return nil, err
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, configError("base URL does not parse")
	}
	u.Path = strings.TrimSuffix(u.Path, "/chat/completions") + "/audio/speech"
	if voice == "" {
		voice = defaultVoice
	}
	cfg.Headers = cloneHeaders(cfg.Headers)
	return &synthesizer{&provider{cfg: cfg, endpoint: u.String(), client: noRedirectClient(cfg.Client)}, voice}, nil
}

// Synthesize implements [agent.Synthesizer].
func (s *synthesizer) Synthesize(ctx context.Context, text string) ([]byte, string, error) {
	if strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > MaxSpeechText {
		return nil, "", errors.New(agent.ErrProviderInput, "speech needs 1 to 4096 characters of text")
	}
	body, err := json.Marshal(map[string]string{"model": s.cfg.Model, "input": text, "voice": s.voice, "response_format": speechFormat})
	if err != nil {
		return nil, "", errors.Wrap(err, agent.ErrProviderConfig, "request cannot be encoded")
	}
	ctx, cancel := context.WithTimeout(ctx, speechTimeout)
	defer cancel()
	key, err := s.apiKey(ctx)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", errors.Wrap(err, agent.ErrProviderConfig, "request cannot be built")
	}
	s.setHeaders(req, key)
	req.Header.Set("Accept", speechAcceptHdr)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", httpcall.NetworkError(ctx, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", httpcall.WithSecondary(httpcall.StatusError(resp), httpcall.CloseError(resp))
	}
	return readSpeech(resp)
}

func readSpeech(resp *http.Response) ([]byte, string, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSpeechReply+1))
	if cerr := resp.Body.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil || len(data) == 0 || len(data) > maxSpeechReply {
		return nil, "", errors.New(agent.ErrProviderResponse, "the speech reply could not be read")
	}
	mime := speechType(data, resp.Header.Get("Content-Type"))
	if mime == "" {
		return nil, "", errors.New(agent.ErrProviderResponse, "the speech reply is not audio")
	}
	return data, mime, nil
}

// speechType is the audio's MIME type, from its bytes (Opus in Ogg, as
// asked; servers that ignore the format send other audio), else from
// the reply's label when it is audio; "" when it is not audio.
func speechType(data []byte, label string) string {
	if bytes.HasPrefix(data, []byte("OggS")) {
		return speechMIME
	}
	if sniffed := http.DetectContentType(data); strings.HasPrefix(sniffed, "audio/") {
		return sniffed
	}
	if label = strings.ToLower(strings.TrimSpace(strings.Split(label, ";")[0])); strings.HasPrefix(label, "audio/") {
		return label
	}
	return ""
}
