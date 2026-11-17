// doc.go: package documentation
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

// Package agent is an SDK for building AI agents in Go: a model, tools, a
// loop that alternates them and a conversation that outlives the process.
//
// # Providers
//
// A [Provider] generates one model response from a [Request]. Adapters live
// in their own packages and translate between these types and a vendor wire
// format:
//
//   - provider/openai: any OpenAI-compatible server (OpenAI, vLLM, Ollama,
//     llama.cpp, OpenRouter, Groq, DeepSeek and most free tiers);
//   - provider/fake: a scripted provider for tests;
//   - provider/retry: a decorator that retries transient failures.
//
// Streaming is explicit: pass a [TextSink] to receive text as it arrives.
// Tool calls are never streamed; the adapter returns them complete.
//
// # Errors
//
// Adapters classify every failure with the codes in this package
// ([ErrProviderRate], [ErrProviderContext], ...), so callers react to the
// kind of failure and never to vendor text. Upstream error bodies are never
// returned: they can echo prompts or credentials.
package agent
