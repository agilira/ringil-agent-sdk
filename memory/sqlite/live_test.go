// live_test.go: a real embedding model with the memory
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build live

package sqlite

import (
	"context"
	"os"
	"strings"
	"testing"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/openai"
)

// TestLiveEmbeddings: a real embedding model finds a memory by meaning.
// Cost: a handful of short texts, about $0.00002 with
// text-embedding-3-small.
func TestLiveEmbeddings(t *testing.T) {
	base, model := os.Getenv("RINGIL_BASE_URL"), os.Getenv("RINGIL_EMBED_MODEL")
	if base == "" || model == "" {
		t.Skip("RINGIL_EMBED_MODEL not set")
	}
	emb, err := openai.NewEmbedder(openai.Config{BaseURL: base, Model: model, APIKey: os.Getenv("RINGIL_API_KEY"), EmbedInputType: os.Getenv("RINGIL_EMBED_INPUT_TYPE") == "1"})
	if err != nil {
		t.Fatal(err)
	}
	mem := openIn(t, t.TempDir(), emb)
	ctx := context.Background()
	for _, fact := range []string{"Il gatto di Antonio si chiama Lira.", "La riunione settimanale con Acme è il martedì alle 10.", "La fattura 42 va pagata entro venerdì."} {
		if _, err := mem.Put(ctx, agent.MemoryRecord{Content: fact}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := mem.Search(ctx, "quando ci vediamo con il cliente?", 1)
	if err != nil || len(got) != 1 || !strings.Contains(got[0].Content, "Acme") {
		t.Fatalf("by meaning: %v %v", got, err)
	}
}
