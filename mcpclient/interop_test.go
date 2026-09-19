package mcpclient

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestInteropEverythingServer talks to the upstream reference server,
// @modelcontextprotocol/server-everything, over stdio. It needs npx and
// the network, so it runs only with MCP_INTEROP=1 (make interop).
func TestInteropEverythingServer(t *testing.T) {
	if os.Getenv("MCP_INTEROP") == "" {
		t.Skip("set MCP_INTEROP=1 to run against the upstream reference server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npx", "-y", "@modelcontextprotocol/server-everything")
	cmd.Stderr = os.Stderr
	srv, err := Connect(ctx, &sdk.CommandTransport{Command: cmd}, WithPrefix("every"))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	tools := agenttool.Set(srv.Tools())
	for _, name := range []string{"every__echo", "every__get-tiny-image", "every__get-structured-content", "every__trigger-long-running-operation", "every__get-sum"} {
		if _, ok := tools.Lookup(name); !ok {
			t.Errorf("tool %q missing from %d tools", name, len(tools))
		}
	}
	echoTool, _ := tools.Lookup("every__echo")
	if !strings.Contains(string(echoTool.Parameters()), `"message"`) {
		t.Errorf("echo schema = %s", echoTool.Parameters())
	}

	t.Run("text", func(t *testing.T) {
		res, err := echoTool.Execute(ctx, agenttool.Call{ID: "1", Args: json.RawMessage(`{"message":"hello"}`)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output.Text, "hello") {
			t.Errorf("text = %q", res.Output.Text)
		}
	})
	t.Run("image", func(t *testing.T) {
		img, _ := tools.Lookup("every__get-tiny-image")
		res, err := img.Execute(ctx, agenttool.Call{ID: "2", Args: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		var images int
		for _, p := range res.Output.Parts {
			if im, ok := p.(*openresponses.InputImage); ok && strings.HasPrefix(im.ImageURL, "data:image/png;base64,") {
				images++
			}
		}
		if images != 1 {
			t.Errorf("parts = %+v", res.Output.Parts)
		}
	})
	t.Run("structured", func(t *testing.T) {
		sc, _ := tools.Lookup("every__get-structured-content")
		res, err := sc.Execute(ctx, agenttool.Call{ID: "3", Args: json.RawMessage(`{"location":"Chicago"}`)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Output.String(), "temperature") {
			t.Errorf("output = %q", res.Output.String())
		}
	})
	t.Run("invalid arguments are a tool error", func(t *testing.T) {
		sum, _ := tools.Lookup("every__get-sum")
		_, err := sum.Execute(ctx, agenttool.Call{ID: "4", Args: json.RawMessage(`{"a":"x"}`)})
		if err == nil {
			t.Error("expected an error for a string where a number is required")
		}
	})
	t.Run("progress", func(t *testing.T) {
		long, _ := tools.Lookup("every__trigger-long-running-operation")
		var updates atomic.Int32
		res, err := long.Execute(ctx, agenttool.Call{ID: "5", Args: json.RawMessage(`{"duration":1,"steps":3}`), OnUpdate: func(agenttool.Result) { updates.Add(1) }})
		if err != nil {
			t.Fatal(err)
		}
		// The SDK dispatches notifications on their own goroutines, so
		// an update the server sent before the result can be handled
		// after it; the token stays routable for a grace period.
		deadline := time.Now().Add(2 * time.Second)
		for updates.Load() < 3 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if updates.Load() != 3 {
			t.Errorf("progress notifications forwarded = %d, want 3", updates.Load())
		}
		if res.Output.String() == "" {
			t.Error("empty result")
		}
	})
}
