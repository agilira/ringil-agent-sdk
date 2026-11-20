// embed.go: Ollama's own /api/embed as an Embedder
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package ollama talks with an Ollama server in its own API: embeddings
// made on the user's machine, without a key and without a cent.
//
// Some embedding models are asymmetric and want the question told from
// the text it should find by a prefix ("search_query: " and
// "search_document: " for nomic-embed-text, an instruction for the
// queries of qwen3-embedding): Config.QueryPrefix and DocumentPrefix.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
)

// DefaultURL is where Ollama listens unless told otherwise.
const DefaultURL = "http://127.0.0.1:11434"

// Config is the server and the model.
type Config struct {
	// URL defaults to DefaultURL. https, or http to the loopback unless
	// AllowHTTP (an Ollama on the office network).
	URL       string
	AllowHTTP bool
	// Model is an embedding model the server has (ollama pull).
	Model string
	// QueryPrefix and DocumentPrefix go before questions and before the
	// texts they should find, for the models that want them.
	QueryPrefix, DocumentPrefix string
	// Dimensions shortens the vectors, for the models that allow it
	// (0: the model's own).
	Dimensions int
	// Headers go with every request (a proxy's token, for one).
	Headers func(ctx context.Context) (http.Header, error)
	// Client is the HTTP client to start from.
	Client *http.Client
}

// Bounds of a call.
const (
	batch      = 64
	attempts   = 2
	maxReply   = 64 << 20
	maxDims    = 16384
	maxQueries = 1 << 20
)

// callTimeout bounds one attempt: generous, as the first call loads the
// model into memory. A variable for tests only.
var callTimeout = 2 * time.Minute

type embedder struct {
	cfg      Config
	endpoint string
	client   *http.Client
}

// NewEmbedder returns an Embedder for cfg.Model; EmbedQuery adds the
// QueryPrefix (without one it is Embed).
func NewEmbedder(cfg Config) (agent.QueryEmbedder, error) {
	if cfg.URL == "" {
		cfg.URL = DefaultURL
	}
	if err := safehttp.CheckURL(cfg.URL, cfg.AllowHTTP); err != nil {
		return nil, errors.Wrap(err, agent.ErrProviderConfig, "ollama: "+err.Error())
	}
	switch {
	case strings.TrimSpace(cfg.Model) == "":
		return nil, errors.New(agent.ErrProviderConfig, "ollama: an embedding model is required")
	case cfg.Dimensions < 0 || cfg.Dimensions > maxDims:
		return nil, errors.New(agent.ErrProviderConfig, "ollama: dimensions out of range")
	}
	return &embedder{cfg: cfg, endpoint: strings.TrimRight(cfg.URL, "/") + "/api/embed",
		client: safehttp.Client(cfg.Client, cfg.Headers, maxReply, false)}, nil
}

// Model names the vectors: the model, with the document prefix when there
// is one (another prefix makes other vectors), and the dimensions.
func (e *embedder) Model() string {
	name := e.cfg.Model
	if e.cfg.DocumentPrefix != "" {
		name += "#" + e.cfg.DocumentPrefix
	}
	if e.cfg.Dimensions > 0 {
		name += "@" + itoa(e.cfg.Dimensions)
	}
	return name
}

func itoa(n int) string {
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// Embed implements agent.Embedder.
func (e *embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embedAll(ctx, texts, e.cfg.DocumentPrefix)
}

// EmbedQuery implements agent.QueryEmbedder.
func (e *embedder) EmbedQuery(ctx context.Context, queries []string) ([][]float32, error) {
	return e.embedAll(ctx, queries, e.cfg.QueryPrefix)
}

func (e *embedder) embedAll(ctx context.Context, texts []string, prefix string) ([][]float32, error) {
	if len(texts) > maxQueries {
		return nil, errors.New(agent.ErrEmbed, "ollama: too many texts at once")
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batch {
		part := texts[start:min(start+batch, len(texts))]
		input := make([]string, len(part))
		for i, t := range part {
			input[i] = prefix + t
		}
		vecs, err := e.embedBatch(ctx, input)
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	for _, v := range out {
		if len(v) != len(out[0]) {
			return nil, errors.New(agent.ErrEmbed, "ollama: vectors of different lengths")
		}
	}
	return out, nil
}

func (e *embedder) embedBatch(ctx context.Context, input []string) ([][]float32, error) {
	req := map[string]any{"model": e.cfg.Model, "input": input, "truncate": true}
	if e.cfg.Dimensions > 0 {
		req["dimensions"] = e.cfg.Dimensions
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrProviderConfig, "ollama: the request cannot be encoded")
	}
	var vecs [][]float32
	for range attempts {
		vecs, err = e.attempt(ctx, body, len(input))
		if err == nil || ctx.Err() != nil || !retryable(err) {
			break
		}
	}
	return vecs, err
}

// retryable: a stalled attempt or a server not answering, not a refusal.
func retryable(err error) bool {
	return stderrors.Is(err, context.DeadlineExceeded) || errors.HasCode(err, agent.ErrProviderUnavailable)
}

func (e *embedder) attempt(ctx context.Context, body []byte, n int) ([][]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrProviderConfig, "ollama: bad request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.Wrap(err, agent.ErrProviderUnavailable, "ollama: the server cannot be reached")
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrEmbed, "ollama: the reply could not be read")
	}
	if resp.StatusCode/100 != 2 {
		return nil, failure(resp.StatusCode, data)
	}
	return parse(data, n)
}

// failure is a refusal with Ollama's own words ("model 'x' not found"),
// clipped; a server or gateway in trouble may pass with a retry, a model
// that cannot embed (501) never does.
func failure(status int, data []byte) error {
	var e struct{ Error string }
	msg := http.StatusText(status)
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		msg = clip(e.Error, 300)
	}
	code := agent.ErrEmbed
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		code = agent.ErrProviderUnavailable
	}
	return errors.New(code, "ollama: "+msg)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

func parse(data []byte, n int) ([][]float32, error) {
	var reply struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&reply); err != nil || dec.More() {
		return nil, errors.New(agent.ErrEmbed, "ollama: the reply is not one valid JSON object")
	}
	if len(reply.Embeddings) != n {
		return nil, errors.New(agent.ErrEmbed, "ollama: the reply does not match the texts sent")
	}
	for _, v := range reply.Embeddings {
		if len(v) == 0 || len(v) > maxDims {
			return nil, errors.New(agent.ErrEmbed, "ollama: the reply holds no usable vector")
		}
	}
	return reply.Embeddings, nil
}
