package mcpclient

//lint:file-ignore SA1019 roots and logging are deprecated from protocol version 2026-07-28 and still work there; these are their tests

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRootsOfferedOnlyWhenGiven: the client offers roots only when the
// host gives them, and answers with the host's (#99).
func TestRootsOfferedOnlyWhenGiven(t *testing.T) {
	type seen struct {
		offered bool
		roots   []string
		err     error
	}
	probe := func(t *testing.T, opts ...Option) seen {
		t.Helper()
		server := sdk.NewServer(&sdk.Implementation{Name: "fs", Version: "1"}, nil)
		var got seen
		server.AddTool(&sdk.Tool{Name: "roots", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				caps := req.Session.InitializeParams().Capabilities
				got.offered = caps != nil && caps.RootsV2 != nil
				if got.offered {
					res, err := req.Session.ListRoots(ctx, nil)
					got.err = err
					if err == nil {
						for _, r := range res.Roots {
							got.roots = append(got.roots, r.URI)
						}
					}
				}
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
			})
		s := connect(t, server, opts...)
		if _, err := lookup(t, s, "roots").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"plain", nil},
		{"with elicitation", []Option{WithElicitation()}},
		{"older server", []Option{older}},
	} {
		t.Run(tc.name+" offers none", func(t *testing.T) {
			if got := probe(t, tc.opts...); got.offered {
				t.Errorf("roots offered without WithRoots: %+v", got)
			}
		})
	}
	t.Run("given", func(t *testing.T) {
		got := probe(t, WithRoots(&sdk.Root{URI: "file:///work", Name: "work"}), older)
		if !got.offered || got.err != nil || len(got.roots) != 1 || got.roots[0] != "file:///work" {
			t.Errorf("roots = %+v, want file:///work", got)
		}
	})
}

// TestLogMessagesReachTheHost: with WithLogging a server's log messages
// sent during a call reach the host's function, with the call on its
// context, and never the call's output (#99).
func TestLogMessagesReachTheHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"current", nil},
		{"older server", []Option{older}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := sdk.NewServer(&sdk.Implementation{Name: "net", Version: "1"}, nil)
			server.AddTool(&sdk.Tool{Name: "fetch", InputSchema: json.RawMessage(`{"type":"object"}`)},
				func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
					_ = req.Session.Log(ctx, &sdk.LoggingMessageParams{Level: "debug", Data: "too fine"})
					if err := req.Session.Log(ctx, &sdk.LoggingMessageParams{Level: "warning", Logger: "upstream", Data: "lost the upstream connection"}); err != nil {
						return nil, err
					}
					return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "fetched"}}}, nil
				})
			var mu sync.Mutex
			var got []string
			var calls []string
			s := connect(t, server, append(tc.opts, WithLogging("info", func(ctx context.Context, msg *sdk.LoggingMessageParams) {
				mu.Lock()
				defer mu.Unlock()
				got = append(got, msg.Logger+": "+msg.Data.(string))
				if c, ok := agenttool.CallFrom(ctx); ok {
					calls = append(calls, c.ID)
				}
			}))...)
			res, err := lookup(t, s, "fetch").Execute(context.Background(), agenttool.Call{ID: "call_1", Args: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if res.Output.Text != "fetched" {
				t.Errorf("output = %+v; a log message must not reach it", res.Output)
			}
			msgs := waitFor(t, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }, 1)
			if len(msgs) != 1 || msgs[0] != "upstream: lost the upstream connection" {
				t.Errorf("messages = %q, want the warning alone", msgs)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls) != 1 || calls[0] != "call_1" {
				t.Errorf("calls on the context = %q, want call_1", calls)
			}
		})
	}
}

// TestNoLoggingNoMessages: without WithLogging a server is not asked for
// log messages, so a current one sends none.
func TestNoLoggingNoMessages(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "net", Version: "1"}, nil)
	var sent error
	server.AddTool(&sdk.Tool{Name: "fetch", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			sent = req.Session.Log(ctx, &sdk.LoggingMessageParams{Level: "emergency", Data: "x"})
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "fetched"}}}, nil
		})
	var heard bool
	s := connect(t, server, WithClientOptions(sdk.ClientOptions{LoggingMessageHandler: func(context.Context, *sdk.LoggingMessageRequest) { heard = true }}))
	if _, err := lookup(t, s, "fetch").Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if sent != nil || heard {
		t.Errorf("sent = %v, heard = %v; want nothing sent", sent, heard)
	}
}
