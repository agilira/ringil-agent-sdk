// embed.go: turning text into vectors
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"

	"github.com/agilira/go-errors"
)

// Embedder turns texts into vectors whose closeness reflects closeness of
// meaning. Implementations: provider/openai (any OpenAI-compatible
// /embeddings endpoint), or a local model behind the same interface.
type Embedder interface {
	// Embed returns one vector per text, in order, all of one length.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Model names the embedding model: vectors of different models are
	// not comparable, so stores keep it beside their vectors.
	Model() string
}

// QueryEmbedder is an Embedder for asymmetric models, which embed a
// question differently from the texts it should find (NVIDIA NIM,
// Cohere, Voyage, E5). Stores use EmbedQuery for searches when the
// embedder has it.
type QueryEmbedder interface {
	Embedder
	// EmbedQuery returns one vector per query, comparable with those
	// Embed returns for documents.
	EmbedQuery(ctx context.Context, queries []string) ([][]float32, error)
}

// ErrEmbed marks a failed embedding.
const ErrEmbed errors.ErrorCode = "EMBED_FAILED"
