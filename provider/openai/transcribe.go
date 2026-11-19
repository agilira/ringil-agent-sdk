// transcribe.go: an OpenAI-compatible /audio/transcriptions endpoint
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// Bounds of a transcription: the audio sent (the main APIs take 25 MB)
// and the reply read.
const (
	MaxAudioBytes      = 25 << 20
	maxTranscriptReply = 1 << 20
)

// transcribeTimeout bounds one transcription; a variable for tests.
var transcribeTimeout = 3 * time.Minute

type transcriber struct {
	*provider
}

// NewTranscriber returns a Transcriber for cfg.Model (whisper-1,
// whisper-large-v3...) at cfg.BaseURL + "/audio/transcriptions", with the
// same key, header and transport rules as New.
func NewTranscriber(cfg Config) (agent.Transcriber, error) {
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
	u.Path = strings.TrimSuffix(u.Path, "/chat/completions") + "/audio/transcriptions"
	cfg.Headers = cloneHeaders(cfg.Headers)
	return &transcriber{&provider{cfg: cfg, endpoint: u.String(), client: noRedirectClient(cfg.Client)}}, nil
}

// audioNames are file names the endpoints recognise the format by.
var audioNames = map[string]string{
	"audio/ogg": "audio.ogg", "audio/opus": "audio.ogg", "audio/mpeg": "audio.mp3", "audio/mp3": "audio.mp3",
	"audio/wav": "audio.wav", "audio/x-wav": "audio.wav", "audio/webm": "audio.webm", "audio/mp4": "audio.m4a",
	"audio/m4a": "audio.m4a", "audio/x-m4a": "audio.m4a", "audio/flac": "audio.flac",
}

// Transcribe implements [agent.Transcriber].
func (t *transcriber) Transcribe(ctx context.Context, audio []byte, mime string) (string, error) {
	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) // only the type goes on the wire
	name, ok := audioNames[mime]
	switch {
	case !ok:
		return "", errors.New(agent.ErrProviderInput, "audio of type "+mime+" cannot be transcribed")
	case len(audio) == 0 || len(audio) > MaxAudioBytes:
		return "", errors.New(agent.ErrProviderInput, "the audio must be 1 byte to 25 MiB")
	}
	body, contentType, err := t.form(audio, name, mime)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, transcribeTimeout)
	defer cancel()
	return t.post(ctx, body, contentType)
}

func (t *transcriber) form(audio []byte, name, mime string) (*bytes.Buffer, string, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+name+`"`)
	h.Set("Content-Type", mime)
	part, err := w.CreatePart(h)
	if err == nil {
		_, err = part.Write(audio)
	}
	if err == nil {
		err = w.WriteField("model", t.cfg.Model)
	}
	if err == nil {
		err = w.WriteField("response_format", "json")
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		return nil, "", errors.Wrap(err, agent.ErrProviderConfig, "request cannot be encoded")
	}
	return &b, w.FormDataContentType(), nil
}

func (t *transcriber) post(ctx context.Context, body io.Reader, contentType string) (string, error) {
	key, err := t.apiKey(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, body)
	if err != nil {
		return "", errors.Wrap(err, agent.ErrProviderConfig, "request cannot be built")
	}
	t.setHeaders(req, key)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return "", networkError(ctx, err)
	}
	if resp.StatusCode/100 != 2 {
		return "", withSecondary(statusError(resp), closeError(resp))
	}
	return readTranscript(resp)
}

// readTranscript reads the text of a transcription reply.
func readTranscript(resp *http.Response) (string, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTranscriptReply+1))
	if cerr := resp.Body.Close(); err == nil && cerr != nil {
		err = cerr
	}
	var reply struct {
		Text *string `json:"text"`
	}
	if err != nil || len(data) > maxTranscriptReply || json.Unmarshal(data, &reply) != nil || reply.Text == nil {
		return "", errors.New(agent.ErrProviderResponse, "the transcription reply could not be read")
	}
	return strings.TrimSpace(*reply.Text), nil
}
