// embed.go: an OpenAI-compatible /embeddings endpoint as an Embedder
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

// embedBatch is how many texts go in one request; providers accept more,
// smaller batches keep a failure cheap to retry.
const (
	embedBatch    = 64
	maxEmbedReply = 64 << 20
	// maxDims is far above any embedding model (3072 for the largest
	// OpenAI one); a reply beyond it is not an embedding.
	maxDims = 16384
	// embedTimeout bounds one request: embedding is quick, and a stalled
	// endpoint must not hold up recall before every turn.
	embedTimeout = 60 * time.Second
)

type embedder struct {
	*provider
}

// NewEmbedder returns an Embedder for cfg.Model at cfg.BaseURL +
// "/embeddings", with the same key, header and transport rules as New.
// ContextWindow, MaxTokens and StallTimeout are not used.
func NewEmbedder(cfg Config) (agent.Embedder, error) {
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
	u.Path = strings.TrimSuffix(u.Path, "/chat/completions") + "/embeddings"
	cfg.Headers = cloneHeaders(cfg.Headers)
	return &embedder{&provider{cfg: cfg, endpoint: u.String(), client: noRedirectClient(cfg.Client)}}, nil
}

func (e *embedder) Model() string { return e.cfg.Model }

// Embed implements [agent.Embedder].
func (e *embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embedBatch {
		batch := texts[start:min(start+embedBatch, len(texts))]
		vecs, err := e.embedBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, checkDims(out)
}

type embedReply struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (e *embedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{"model": e.cfg.Model, "input": texts, "encoding_format": "float"})
	if err != nil {
		return nil, errors.Wrap(err, agent.ErrProviderConfig, "request cannot be encoded")
	}
	resp, err := e.send(ctx, body)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbedReply+1))
	if cerr := resp.Body.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if ctxErr := context.Cause(ctx); err != nil && ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil || len(data) > maxEmbedReply {
		return nil, errors.New(agent.ErrEmbed, "the embedding reply could not be read")
	}
	return parseReply(data, len(texts))
}

func parseReply(data []byte, n int) ([][]float32, error) {
	var reply embedReply
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&reply); err != nil || dec.More() {
		return nil, errors.New(agent.ErrEmbed, "the embedding reply is not one valid JSON object")
	}
	return ordered(reply, n)
}

// ordered puts the vectors back in input order and checks there is one
// per text.
func ordered(reply embedReply, n int) ([][]float32, error) {
	out := make([][]float32, n)
	for _, d := range reply.Data {
		if d.Index < 0 || d.Index >= n || out[d.Index] != nil || len(d.Embedding) == 0 || len(d.Embedding) > maxDims {
			return nil, errors.New(agent.ErrEmbed, "the embedding reply does not match the texts sent")
		}
		out[d.Index] = d.Embedding
	}
	for _, v := range out {
		if v == nil {
			return nil, errors.New(agent.ErrEmbed, "the embedding reply is missing vectors")
		}
	}
	return out, nil
}

func checkDims(vecs [][]float32) error {
	for _, v := range vecs {
		if len(v) != len(vecs[0]) {
			return errors.New(agent.ErrEmbed, "the embedding model returned vectors of different lengths")
		}
	}
	return nil
}
