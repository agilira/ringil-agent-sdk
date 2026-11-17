// stream_test.go: tests for streamed completions
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package openai

import (
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
)

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	return b.String()
}

func collect() (*[]string, agent.TextSink) {
	var got []string
	return &got, func(d string) error { got = append(got, d); return nil }
}

func TestStreamText(t *testing.T) {
	body := sse(
		`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"delta":{"content":"Buon"}}]}`,
		`{"choices":[{"delta":{"content":"giorno"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		`[DONE]`,
	)
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		reply(t, w, body)
	})
	got, sink := collect()
	resp, err := ask(p, sink)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(*got, "|") != "Buon|giorno" || resp.Message.Text() != "Buongiorno" {
		t.Fatalf("deltas %v response %+v", *got, resp)
	}
	if resp.Stop != agent.StopEnd || resp.Usage != (agent.Usage{Input: 5, Output: 2}) {
		t.Fatalf("stop %q usage %+v", resp.Stop, resp.Usage)
	}
}

func TestStreamToolCallFragments(t *testing.T) {
	body := sse(
		`{"choices":[{"delta":{"content":"Controllo."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"meteo","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"citta\""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"ora","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"Roma\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	)
	resp, err := decodeStream(strings.NewReader(body), func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	calls := resp.Message.Calls
	if len(calls) != 2 || resp.Stop != agent.StopToolUse || resp.Message.Text() != "Controllo." {
		t.Fatalf("response %+v", resp)
	}
	want := []agent.ToolCall{
		{ID: "a", Name: "meteo", Input: json.RawMessage(`{"citta":"Roma"}`)},
		{ID: "b", Name: "ora", Input: json.RawMessage(`{}`)},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %+v", calls)
	}
}

func TestStreamCallsWithoutIndex(t *testing.T) {
	body := sse(
		`{"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"x","arguments":"{\"k\""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":":1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"y","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	resp, err := decodeStream(strings.NewReader(body), func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	calls := resp.Message.Calls
	if len(calls) != 2 || string(calls[0].Input) != `{"k":1}` || calls[1].Name != "y" {
		t.Fatalf("calls %+v", calls)
	}
}

func TestStreamEndMarkers(t *testing.T) {
	text := `{"choices":[{"delta":{"content":"a"}}]}`
	finish := `{"choices":[{"delta":{},"finish_reason":"stop"}]}`
	complete := []string{
		sse(text, finish),                       // finish reason, no [DONE]
		sse(text, `[DONE]`),                     // [DONE], no finish reason
		"data:" + text + "\r\n\r\ndata: [DONE]", // CRLF, no space, no final blank line
	}
	for i, body := range complete {
		if _, err := decodeStream(strings.NewReader(body), func(string) error { return nil }); err != nil {
			t.Errorf("complete stream %d refused: %v", i, err)
		}
	}
	_, err := decodeStream(strings.NewReader(sse(text)), func(string) error { return nil })
	if !errors.HasCode(err, agent.ErrProviderInterrupted) {
		t.Fatalf("cut stream accepted: %v", err)
	}
}

func TestStreamIgnoresCommentsAndOtherFields(t *testing.T) {
	body := ": keep-alive\n\nevent: message\nid: 7\nretry: 100\ndata: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\n"
	resp, err := decodeStream(strings.NewReader(body), func(string) error { return nil })
	if err != nil || resp.Message.Text() != "x" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
}

func TestStreamMultiLineData(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":\ndata: {\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\n"
	resp, err := decodeStream(strings.NewReader(body), func(string) error { return nil })
	if err != nil || resp.Message.Text() != "x" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
}

func TestStreamFailures(t *testing.T) {
	cases := []struct {
		body string
		code errors.ErrorCode
	}{
		{sse(`not json`), agent.ErrProviderResponse},
		{sse(`{"error":{"message":"overloaded"}}`), agent.ErrProviderUnavailable},
		{"data: " + strings.Repeat("a", maxLine+1) + "\n\n", agent.ErrProviderResponse},
		{"data: " + strings.Repeat("a", maxLine/2+1) + "\ndata: " + strings.Repeat("a", maxLine/2+1) + "\n\n", agent.ErrProviderResponse},
	}
	for i, c := range cases {
		if _, err := decodeStream(strings.NewReader(c.body), func(string) error { return nil }); !errors.HasCode(err, c.code) {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestStreamErrorNullIsNotAnError(t *testing.T) {
	body := sse(`{"error":null,"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
	if _, err := decodeStream(strings.NewReader(body), func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestStreamSinkErrorAborts(t *testing.T) {
	stop := stderrors.New("ui closed")
	body := sse(`{"choices":[{"delta":{"content":"a"}}]}`, `{"choices":[{"delta":{"content":"b"},"finish_reason":"stop"}]}`)
	calls := 0
	_, err := decodeStream(strings.NewReader(body), func(string) error { calls++; return stop })
	if !stderrors.Is(err, stop) || calls != 1 {
		t.Fatalf("err %v after %d deltas", err, calls)
	}
}

func TestStreamCutIsInterrupted(t *testing.T) {
	r := io.MultiReader(strings.NewReader(sse(`{"choices":[{"delta":{"content":"a"}}]}`)), errReader{})
	if _, err := decodeStream(r, func(string) error { return nil }); !errors.HasCode(err, agent.ErrProviderInterrupted) {
		t.Fatalf("err %v", err)
	}
}

func TestStreamBounds(t *testing.T) {
	s := &streamState{sink: func(string) error { return nil }, byIndex: map[int]*partialCall{}}
	s.size = maxStreamed
	if err := s.textDelta("x"); !errors.HasCode(err, agent.ErrProviderResponse) {
		t.Fatalf("total bound: %v", err)
	}
	s = &streamState{byIndex: map[int]*partialCall{}}
	for i := range maxToolCalls {
		idx := i
		if err := s.callDelta(wireToolCall{Index: &idx, ID: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	idx := maxToolCalls
	if err := s.callDelta(wireToolCall{Index: &idx, ID: "x"}); !errors.HasCode(err, agent.ErrProviderResponse) {
		t.Fatalf("call count bound: %v", err)
	}
}

// FuzzDecodeStream feeds arbitrary bytes as a stream. It must never panic,
// and whatever it accepts must be a well-formed response.
func FuzzDecodeStream(f *testing.F) {
	f.Add([]byte(sse(`{"choices":[{"delta":{"content":"a"},"finish_reason":"stop"}]}`)))
	f.Add([]byte(sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"t","arguments":"{"}}]}}]}`, `[DONE]`)))
	f.Add([]byte("data: [DONE]\n\n"))
	f.Add([]byte(": x\n\ndata:{\"choices\":null}\r\n\r\n"))
	f.Fuzz(func(t *testing.T, body []byte) {
		resp, err := decodeStream(strings.NewReader(string(body)), func(string) error { return nil })
		if err == nil {
			checkResponse(t, resp)
		}
	})
}

// FuzzParseBody does the same for non-streamed responses.
func FuzzParseBody(f *testing.F) {
	f.Add([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	f.Add([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"t","arguments":"x"}}]}}],"usage":{"prompt_tokens":-1}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		resp, err := parseBody(body)
		if err == nil {
			checkResponse(t, resp)
		}
	})
}

func checkResponse(t *testing.T, resp *agent.Response) {
	t.Helper()
	if resp.Message.Role != agent.RoleAssistant || resp.Usage.Input < 0 || resp.Usage.Output < 0 || resp.Usage.CacheRead < 0 {
		t.Fatalf("malformed response %+v", resp)
	}
	if (len(resp.Message.Calls) > 0) != (resp.Stop == agent.StopToolUse) {
		t.Fatalf("stop %q with %d calls", resp.Stop, len(resp.Message.Calls))
	}
	checkWellFormedCalls(t, resp.Message)
}

func checkWellFormedCalls(t *testing.T, msg agent.Message) {
	t.Helper()
	for _, call := range msg.Calls {
		if call.ID == "" || !json.Valid(call.Input) {
			t.Fatalf("malformed call %+v", call)
		}
	}
	if _, err := json.Marshal(msg); err != nil {
		t.Fatalf("accepted message does not serialize: %v", err)
	}
}
