// scale_test.go: cost of a search with many memories (SCALE=1)
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package vector

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
)

type wideEmbedder struct{}

func (wideEmbedder) Model() string { return "wide" }
func (wideEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 1536)
		for j := range v {
			v[j] = float32((len(t)*31+j*7)%97) / 97
		}
		out[i] = v
	}
	return out, nil
}

func TestScaleSearch(t *testing.T) {
	if os.Getenv("SCALE") == "" {
		t.Skip("SCALE=1 to run")
	}
	m, err := New(t.TempDir()+"/mem", wideEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	s := m.(*store)
	all := make([]agent.MemoryRecord, 10000)
	for i := range all {
		all[i] = agent.MemoryRecord{ID: "r" + strconv.Itoa(i), Content: "fatto numero " + strconv.Itoa(i)}
	}
	if _, err := s.complete(context.Background(), all, 1536); err != nil {
		t.Fatal(err)
	}
	q, err := wideEmbedder{}.Embed(context.Background(), []string{"q"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		start := time.Now()
		_, dec, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		rankByMeaning(all, dec, q[0], 0, 5)
		t.Logf("search %d: %v", i, time.Since(start))
	}
}
