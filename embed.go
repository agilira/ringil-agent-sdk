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

// ErrEmbed marks a failed embedding.
const ErrEmbed errors.ErrorCode = "EMBED_FAILED"
