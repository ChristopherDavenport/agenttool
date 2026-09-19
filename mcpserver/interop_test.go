package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInteropInspector serves agenttool.New tools from examples/stdio and
// drives them with the upstream MCP Inspector CLI,
// @modelcontextprotocol/inspector. It needs go, npx and the network, so
// it runs only with MCP_INTEROP=1 (make interop).
func TestInteropInspector(t *testing.T) {
	if os.Getenv("MCP_INTEROP") == "" {
		t.Skip("set MCP_INTEROP=1 to run against the upstream MCP Inspector")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	bin := filepath.Join(t.TempDir(), "stdio")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "./examples/stdio")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	inspect := func(t *testing.T, args ...string) map[string]any {
		t.Helper()
		cmd := exec.CommandContext(ctx, "npx", append([]string{"-y", "@modelcontextprotocol/inspector", "--cli", bin}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			var stderr []byte
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = ee.Stderr
			}
			t.Fatalf("inspector %v: %v\n%s\n%s", args, err, out, stderr)
		}
		var v map[string]any
		if err := json.Unmarshal(out, &v); err != nil {
			t.Fatalf("inspector output is not JSON: %v\n%s", err, out)
		}
		if e, ok := v["error"]; ok {
			t.Fatalf("inspector error: %v", e)
		}
		return v
	}

	t.Run("tools/list", func(t *testing.T) {
		v := inspect(t, "--method", "tools/list")
		tools, _ := v["tools"].([]any)
		byName := map[string]map[string]any{}
		for _, tl := range tools {
			m := tl.(map[string]any)
			byName[m["name"].(string)] = m
		}
		for _, name := range []string{"upper", "add", "tiny_image"} {
			if byName[name] == nil {
				t.Errorf("tool %q missing from %v", name, byName)
			}
		}
		schema, _ := json.Marshal(byName["add"]["inputSchema"])
		if !strings.Contains(string(schema), `"additionalProperties":false`) || !strings.Contains(string(schema), `"First addend"`) {
			t.Errorf("add schema = %s", schema)
		}
	})
	t.Run("tools/call text", func(t *testing.T) {
		v := inspect(t, "--method", "tools/call", "--tool-name", "upper", "--tool-arg", "text=abc")
		if text := firstText(v); text != "ABC" {
			t.Errorf("text = %q in %v", text, v)
		}
	})
	t.Run("tools/call json", func(t *testing.T) {
		v := inspect(t, "--method", "tools/call", "--tool-name", "add", "--tool-arg", "a=2", "b=3")
		if text := firstText(v); text != `{"sum":5}` {
			t.Errorf("text = %q in %v", text, v)
		}
	})
	t.Run("tools/call image", func(t *testing.T) {
		v := inspect(t, "--method", "tools/call", "--tool-name", "tiny_image")
		content, _ := v["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("content = %v", content)
		}
		part := content[0].(map[string]any)
		if part["type"] != "image" || part["mimeType"] != "image/png" || part["data"] == "" {
			t.Errorf("image part = %v", part)
		}
	})
	t.Run("tools/call error", func(t *testing.T) {
		// A missing required property is refused by schema validation
		// before the tool runs. The Inspector exits non-zero on isError
		// and prints the result before its own error object.
		cmd := exec.CommandContext(ctx, "npx", "-y", "@modelcontextprotocol/inspector", "--cli", bin,
			"--method", "tools/call", "--tool-name", "add", "--tool-arg", "a=1")
		out, err := cmd.Output()
		if err == nil {
			t.Fatalf("expected a non-zero exit for isError; output:\n%s", out)
		}
		if !strings.Contains(string(out), `"isError": true`) || !strings.Contains(string(out), "invalid arguments") || !strings.Contains(string(out), "required") {
			t.Errorf("error call output:\n%s", out)
		}
	})
}

func firstText(v map[string]any) string {
	content, _ := v["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	part, _ := content[0].(map[string]any)
	text, _ := part["text"].(string)
	return text
}
