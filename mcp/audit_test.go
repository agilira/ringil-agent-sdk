// audit_test.go: regressions for the MCP audit of 18/11
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// rawServer answers JSON-RPC lines with reply(method, id), which returns
// the lines to send back. It plays servers the SDK would never be.
func rawServer(t *testing.T, reply func(method string, id json.RawMessage) []string) sdk.Transport {
	t.Helper()
	toServer, clientOut := io.Pipe()
	serverOut, toClient := io.Pipe()
	go func() {
		defer func() {
			if err := toClient.Close(); err != nil {
				t.Log(err)
			}
		}()
		sc := bufio.NewScanner(toServer)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var msg struct {
				Method string          `json:"method"`
				ID     json.RawMessage `json:"id"`
			}
			if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID == nil {
				continue
			}
			for _, line := range reply(msg.Method, msg.ID) {
				if _, err := io.WriteString(toClient, line+"\n"); err != nil {
					return
				}
			}
		}
	}()
	return &sdk.IOTransport{Reader: serverOut, Writer: clientOut}
}

func result(id json.RawMessage, v string) string {
	return `{"jsonrpc":"2.0","id":` + string(id) + `,"result":` + v + `}`
}

const initResult = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"raw","version":"1"}}`

// Finding 1: list_changed before the initialize answer crashed the process.
func TestEarlyListChangedDoesNotCrash(t *testing.T) {
	tr := rawServer(t, func(method string, id json.RawMessage) []string {
		switch method {
		case "initialize":
			return []string{`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`, result(id, initResult)}
		case "tools/list":
			return []string{result(id, `{"tools":[{"name":"t","inputSchema":{"type":"object"}}]}`)}
		}
		return []string{result(id, `{}`)}
	})
	c, err := connect(bg, Server{Name: "raw"}, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Log(err)
	}
}

// Findings 4, 9, 10: untrusted hints, colliding names, schemas that are
// not objects.
func TestToolListHygiene(t *testing.T) {
	tr := rawServer(t, func(method string, id json.RawMessage) []string {
		switch method {
		case "initialize":
			return []string{result(id, initResult)}
		case "tools/list":
			return []string{result(id, `{"tools":[`+
				`{"name":"a.b","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},`+
				`{"name":"a b","inputSchema":{"type":"object"}},`+
				`{"name":"s","inputSchema":{"type":"string"}}]}`)}
		}
		return []string{result(id, `{}`)}
	})
	c, err := connect(bg, Server{Name: "my_srv"}, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Log(err)
		}
	}()
	var names []string
	for _, tl := range c.Tools() {
		names = append(names, tl.Spec().Name)
		if !tl.Spec().Annotations.Destructive {
			t.Errorf("%s: server hints trusted by default", tl.Spec().Name)
		}
	}
	if strings.Join(names, ",") != "my-srv__a_b,my-srv__a_b_2" {
		t.Fatalf("names %v", names)
	}
}

// Findings 3 and 5: huge results are capped; a hung call does not hold
// up Close.
func TestOutputCapAndHungCall(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	tr := rawServer(t, func(method string, id json.RawMessage) []string {
		switch method {
		case "initialize":
			return []string{result(id, initResult)}
		case "tools/list":
			return []string{result(id, `{"tools":[{"name":"big","inputSchema":{"type":"object"}}]}`)}
		case "tools/call":
			return []string{result(id, `{"content":[{"type":"text","text":"`+big+`"}]}`)}
		}
		return []string{result(id, `{}`)}
	})
	c, err := connect(bg, Server{Name: "raw", MaxOutput: 1000}, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := byName(t, c.Tools(), "raw__big").Call(bg, json.RawMessage(`{}`))
	if err != nil || len(out.Content) > 1100 || !strings.Contains(out.Content, "more bytes") {
		t.Fatalf("not capped: %d bytes %v", len(out.Content), err)
	}
	if err := c.Close(); err != nil {
		t.Log(err)
	}
}

func TestCloseCancelsCallInFlight(t *testing.T) {
	tr := rawServer(t, func(method string, id json.RawMessage) []string {
		switch method {
		case "initialize":
			return []string{result(id, initResult)}
		case "tools/list":
			return []string{result(id, `{"tools":[{"name":"hang","inputSchema":{"type":"object"}}]}`)}
		case "tools/call":
			return nil // never answers
		}
		return []string{result(id, `{}`)}
	})
	c, err := connect(bg, Server{Name: "raw"}, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkCloseUnblocks(t, c)
}

func checkCloseUnblocks(t *testing.T, c *Client) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := byName(t, c.Tools(), "raw__hang").Call(bg, json.RawMessage(`{}`))
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Log(err)
	}
	select {
	case err := <-done:
		if err == nil || time.Since(start) > 3*time.Second {
			t.Fatalf("call %v after %v", err, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call still blocked after Close")
	}
}

// Finding 2: an endless JSON body must not fill memory.
func TestHugeHTTPBodyIsRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := []byte(strings.Repeat(" ", 1<<16))
		for range 64 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	if _, err := Connect(ctx, Server{Name: "h", URL: srv.URL + "/mcp", HTTPClient: srv.Client(), MaxResponse: 1 << 20}); err == nil {
		t.Fatal("a 4 MiB body over a 1 MiB bound was accepted")
	}
}

// Finding 6: plain servers get PATH and HOME, not the rest of the host's
// environment.
func TestPlainServerEnvironment(t *testing.T) {
	t.Setenv("RINGIL_HOST_SECRET", "leak")
	env := strings.Join(envList(map[string]string{"X": "1"}), "\n")
	if !strings.Contains(env, "PATH="+os.Getenv("PATH")) || !strings.Contains(env, "X=1") || strings.Contains(env, "leak") {
		t.Fatalf("env %q", env)
	}
}
