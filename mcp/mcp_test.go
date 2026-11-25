// mcp_test.go: MCP tools through the agent, in memory and over stdio
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package mcp

import (
	"context"
	"encoding/json"
	"github.com/agilira/ringil-agent-sdk/internal/testhome"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agilira/go-errors"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/egress"
	"github.com/agilira/ringil-agent-sdk/sandbox"
)

var bg = context.Background()

type addIn struct {
	A int `json:"a"`
	B int `json:"b"`
}

type addOut struct {
	Sum int `json:"sum"`
}

func ptr(b bool) *bool { return &b }

// testServer is a small MCP server: add (read-only), wipe (destructive by
// default), fail (an error result).
func testServer() *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "add", Description: "Add two numbers", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}},
		func(_ context.Context, _ *sdk.CallToolRequest, in addIn) (*sdk.CallToolResult, addOut, error) {
			return nil, addOut{Sum: in.A + in.B}, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "wipe.all", Description: "Delete everything"},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "wiped"}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "fail"},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "quota exceeded"}}}, nil, nil
		})
	return s
}

func inMemory(t *testing.T, allow ...string) *Client {
	t.Helper()
	ct, st := sdk.NewInMemoryTransports()
	ss, err := testServer().Connect(bg, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ss.Close(); err != nil {
			t.Log(err)
		}
	})
	c, err := connect(bg, Server{Name: "srv", Tools: allow, TrustAnnotations: true}, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Log(err)
		}
	})
	return c
}

func byName(t *testing.T, tools []agent.Tool, name string) agent.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Spec().Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %s", name)
	return nil
}

func TestToolsAndAnnotations(t *testing.T) {
	tools := inMemory(t).Tools()
	if len(tools) != 3 {
		t.Fatalf("%d tools", len(tools))
	}
	add := byName(t, tools, "srv__add").Spec()
	if !add.Annotations.ReadOnly || add.Annotations.Destructive || add.Annotations.OpenWorld || !strings.Contains(string(add.InputSchema), `"a"`) {
		t.Fatalf("add %+v %s", add.Annotations, add.InputSchema)
	}
	if w := byName(t, tools, "srv__wipe_all").Spec(); !w.Annotations.Destructive || !w.Annotations.OpenWorld {
		t.Fatalf("wipe defaults %+v", w.Annotations)
	}
}

func TestCallResultsAndErrors(t *testing.T) {
	tools := inMemory(t).Tools()
	out, err := byName(t, tools, "srv__add").Call(bg, json.RawMessage(`{"a":2,"b":3}`))
	if err != nil || !strings.Contains(out.Content, `"sum":5`) {
		t.Fatalf("add %q %v", out.Content, err)
	}
	_, err = byName(t, tools, "srv__fail").Call(bg, json.RawMessage(`{}`))
	if !errors.HasCode(err, ErrCall) || !strings.Contains(agent.ModelMessage(err), "quota exceeded") {
		t.Fatalf("fail %v", err)
	}
}

func TestAllowList(t *testing.T) {
	if tools := inMemory(t, "add").Tools(); len(tools) != 1 || tools[0].Spec().Name != "srv__add" {
		t.Fatalf("allow list: %d tools", len(tools))
	}
}

func TestConfigValidation(t *testing.T) {
	for _, s := range []Server{
		{Name: "bad name", URL: "https://x"},
		{Name: "a"},
		{Name: "a", URL: "https://x", Command: []string{"x"}},
		{Name: "a", URL: "http://example.com/mcp"},
		{Name: "a", URL: "https://user:pw@example.com/mcp"},
	} {
		if _, err := Connect(bg, s); !errors.HasCode(err, ErrConfig) {
			t.Errorf("%+v: %v", s, err)
		}
	}
}

// TestMain lets the test binary act as an MCP server over stdio.
func TestMain(m *testing.M) {
	if pidFile := os.Getenv("RINGIL_MCP_TEST_CHILD"); pidFile != "" {
		child := exec.Command("sleep", "60")
		if child.Start() != nil || os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600) != nil {
			os.Exit(1)
		}
	}
	if os.Getenv("RINGIL_MCP_TEST_SERVER") == "1" {
		if err := testServer().Run(context.Background(), &sdk.StdioTransport{}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	testhome.Main(m)
}

func TestStdioServerInSandbox(t *testing.T) {
	sb, err := sandbox.New()
	if err != nil {
		if os.Getenv("RINGIL_REQUIRE_SANDBOX") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 20*time.Second)
	defer cancel()
	c, err := Connect(ctx, Server{
		Name: "local", Command: []string{self}, Env: map[string]string{"RINGIL_MCP_TEST_SERVER": "1"},
		Sandbox: sb, Mounts: []sandbox.Mount{{Source: filepath.Dir(self)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := byName(t, c.Tools(), "local__add").Call(ctx, json.RawMessage(`{"a":40,"b":2}`))
	if err != nil || !strings.Contains(out.Content, `"sum":42`) {
		t.Fatalf("add %q %v", out.Content, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

// A local server must outlive the context used to connect to it.
func TestPlainStdioServerOutlivesConnectContext(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 20*time.Second)
	c, err := Connect(ctx, Server{Name: "plain", Command: []string{self}, Env: map[string]string{"RINGIL_MCP_TEST_SERVER": "1"}})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Log(err)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	out, err := byName(t, c.Tools(), "plain__add").Call(bg, json.RawMessage(`{"a":1,"b":2}`))
	if err != nil || !strings.Contains(out.Content, `"sum":3`) {
		t.Fatalf("after connect: %q %v", out.Content, err)
	}
}

// An image a server returns reaches the model as an image (21/11: it was
// named only, "[image not shown]"); one that is not what it claims to be
// stays named.
func TestImagesInResults(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	res := &sdk.CallToolResult{Content: []sdk.Content{
		&sdk.TextContent{Text: "a chart"},
		&sdk.ImageContent{MIMEType: "image/png", Data: png},
		&sdk.ImageContent{MIMEType: "image/png", Data: []byte("not a png")},
	}}
	imgs := images(res)
	if len(imgs) != 1 || imgs[0].MIME != "image/png" || string(imgs[0].Data) != string(png) {
		t.Fatalf("%+v", imgs)
	}
	if text := render(res); !strings.Contains(text, "attached") || !strings.Contains(text, "not shown") {
		t.Fatalf("%q", text)
	}
}

// 23/11: a sandboxed server that cannot start fails at once (its input
// was an io.Pipe, and Wait waited for it until the deadline); mounts and
// a proxy need a sandbox.
func TestASandboxedServerThatCannotStartFailsAtOnce(t *testing.T) {
	sb, err := sandbox.New()
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(bg, 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err = Connect(ctx, Server{Name: "boxed", Command: []string{"/nonexistent/server"}, Sandbox: sb})
	if err == nil || time.Since(start) > 15*time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
	if err := (Server{Name: "x", Command: []string{"x"}, Proxy: &egress.Proxy{}}).validate(); err == nil {
		t.Fatal("a proxy without a sandbox")
	}
}
