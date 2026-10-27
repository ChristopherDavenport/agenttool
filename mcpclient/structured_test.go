package mcpclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestStructuredSentOnce: structured content a text block already holds,
// as MCP asks a server to send it, reaches the model once (#95).
func TestStructuredSentOnce(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		structured any
		want       string
	}{
		{"same JSON", `{"temp":21}`, map[string]any{"temp": 21}, `{"temp":21}`},
		{"equal under RFC 8785", "{ \"b\": [1.0, true, null], \"a\": \"x\" }", map[string]any{"a": "x", "b": []any{1, true, nil}}, "{ \"b\": [1.0, true, null], \"a\": \"x\" }"},
		{"a different value", `{"temp":22}`, map[string]any{"temp": 21}, "{\"temp\":22}\n{\"temp\":21}"},
		{"prose", "It is 21 degrees.", map[string]any{"temp": 21}, "It is 21 degrees.\n{\"temp\":21}"},
		{"a JSON prefix is not the value", `{"temp":21} and more`, map[string]any{"temp": 21}, "{\"temp\":21} and more\n{\"temp\":21}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ResultOf(&sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: tc.text}}, StructuredContent: tc.structured})
			if err != nil {
				t.Fatal(err)
			}
			if res.Output.Text != tc.want {
				t.Errorf("text = %q, want %q", res.Output.Text, tc.want)
			}
		})
	}
}

func TestJCSEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{`1`, `1.0`, true},
		{`1`, `1e0`, true},
		{`-0`, `0`, true},
		{`{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{`[1,2]`, `[2,1]`, false},
		{`"A"`, `"A"`, true},
		{`9007199254740993`, `9007199254740992`, true}, // one double
		{`1e400`, `1e400`, false},                      // no canonical form
		{`{"a":1}`, `{"a":1,"b":2}`, false},
		{`null`, `false`, false},
		{`"1"`, `1`, false},
	}
	for _, tc := range cases {
		a, _ := decodeJSON([]byte(tc.a))
		b, _ := decodeJSON([]byte(tc.b))
		if got := jcsEqual(a, b); got != tc.want {
			t.Errorf("jcsEqual(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestOutputSchemaChecked: a consumed tool that lists an output schema
// has its structured content validated, and a violation, or a result
// with none, is an error that says the call ran (#95).
func TestOutputSchemaChecked(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "weather", Version: "1"}, nil)
	schema := json.RawMessage(`{"type":"object","properties":{"temp":{"type":"number"}},"required":["temp"]}`)
	add := func(name string, out any, res *sdk.CallToolResult) {
		server.AddTool(&sdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: out},
			func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return res, nil })
	}
	add("good", schema, &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: `{"temp":21}`}}, StructuredContent: map[string]any{"temp": 21}})
	add("bad", schema, &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: `{"temp":"hot"}`}}, StructuredContent: map[string]any{"temp": "hot"}})
	add("none", schema, &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "21"}}})
	add("failed", schema, &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "no sensor"}}})
	add("unchecked", nil, &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "21"}}})
	add("unreadable", json.RawMessage(`{"type":"object","$schema":"https://example.com/other"}`), &sdk.CallToolResult{StructuredContent: map[string]any{}})
	s := connect(t, server)
	ctx := context.Background()
	call := func(name string) (agenttool.Result, error) {
		return lookup(t, s, name).Execute(ctx, agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
	}

	if res, err := call("good"); err != nil || res.Output.Text != `{"temp":21}` {
		t.Errorf("good = %+v, %v", res.Output, err)
	}
	for _, name := range []string{"bad", "none", "unreadable"} {
		res, err := call(name)
		if err == nil || !strings.HasPrefix(err.Error(), "invalid result: the call ran") {
			t.Errorf("%s: err = %v, want an invalid result that says the call ran", name, err)
		}
		if _, ok := res.Details.(*sdk.CallToolResult); !ok {
			t.Errorf("%s: details = %T, want the SDK result", name, res.Details)
		}
	}
	if _, err := call("bad"); err == nil || !strings.Contains(err.Error(), "temp") {
		t.Errorf("bad: err = %v, want the violation named", err)
	}
	if _, err := call("failed"); err == nil || err.Error() != "no sensor" {
		t.Errorf("failed: err = %v, want the tool's own error, unchecked", err)
	}
	if res, err := call("unchecked"); err != nil || res.Output.Text != "21" {
		t.Errorf("unchecked = %+v, %v", res.Output, err)
	}
}
