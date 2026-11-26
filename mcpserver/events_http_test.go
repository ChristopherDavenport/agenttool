package mcpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestEventsHTTPHandlerStatus holds go:events:http-400: a request whose
// routing headers disagree with its params is refused with HTTP 400 and
// the -32020 error the server gives without the wrapper, and what the
// wrapper does not judge reaches the SDK as it came.
func TestEventsHTTPHandlerStatus(t *testing.T) {
	server, et := eventsServer(t)
	sdkHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true})
	var reached int
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		sdkHandler.ServeHTTP(w, r)
	})
	hs := httptest.NewServer(et.events.HTTPHandler(counted))
	t.Cleanup(hs.Close)

	const poll = `{"jsonrpc":"2.0","id":7,"method":"events/poll","params":{"name":"deploy","arguments":{"region":"eu"}}}`
	post := func(t *testing.T, body string, headers map[string]string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, hs.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	routed := func(extra map[string]string) map[string]string {
		h := map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "events/poll", "Mcp-Name": "deploy", "Mcp-Param-Region": "eu"}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}

	refused := []struct {
		name    string
		headers map[string]string
		header  string
	}{
		{"name missing", routed(map[string]string{"Mcp-Name": ""}), "Mcp-Name"},
		{"name differs", routed(map[string]string{"Mcp-Name": "other"}), "Mcp-Name"},
		{"param differs", routed(map[string]string{"Mcp-Param-Region": "us"}), "Mcp-Param-Region"},
		{"param missing", routed(map[string]string{"Mcp-Param-Region": ""}), "Mcp-Param-Region"},
	}
	for _, tc := range refused {
		before := reached
		status, body := post(t, poll, tc.headers)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400; body %s", tc.name, status, body)
		}
		if reached != before {
			t.Errorf("%s: the SDK saw a request the wrapper refused", tc.name)
		}
		var out struct {
			ID    json.RawMessage `json:"id"`
			Error struct {
				Code int `json:"code"`
				Data struct {
					Mismatch struct {
						Header string `json:"header"`
					} `json:"mismatch"`
				} `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Errorf("%s: body %q is not JSON: %v", tc.name, body, err)
			continue
		}
		if out.Error.Code != EventsCodeHeaderMismatch || out.Error.Data.Mismatch.Header != tc.header || string(out.ID) != "7" {
			t.Errorf("%s: id %s code %d mismatch %q, want id 7, -32020, %s", tc.name, out.ID, out.Error.Code, out.Error.Data.Mismatch.Header, tc.header)
		}
	}

	passed := []struct {
		name    string
		method  string
		body    string
		headers map[string]string
	}{
		{"headers agree", http.MethodPost, poll, routed(nil)},
		{"events/list has no name", http.MethodPost, `{"jsonrpc":"2.0","id":8,"method":"events/list","params":{}}`, map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "events/list"}},
		{"another method", http.MethodPost, `{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{}}`, map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/list"}},
		{"protocol before the headers", http.MethodPost, poll, map[string]string{"Mcp-Protocol-Version": "2025-11-25", "Mcp-Name": "other"}},
		{"not JSON", http.MethodPost, `not json events/`, routed(nil)},
	}
	for _, tc := range passed {
		before := reached
		status, body := post(t, tc.body, tc.headers)
		if status == http.StatusBadRequest && strings.Contains(body, "-32020") {
			t.Errorf("%s: refused as a mismatch: %s", tc.name, body)
		}
		if reached != before+1 {
			t.Errorf("%s: the SDK saw %d requests, want 1", tc.name, reached-before)
		}
	}

	// A GET is the SDK's.
	before := reached
	resp, err := http.Get(hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if reached != before+1 {
		t.Errorf("GET did not reach the SDK")
	}

	// A body over the SDK's limit is refused, not read whole.
	big := `{"jsonrpc":"2.0","id":1,"method":"events/poll","params":{"name":"deploy","pad":"` + strings.Repeat("a", sdk.DefaultMaxRequestBodyBytes) + `"}}`
	if status, _ := post(t, big, routed(nil)); status != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized body: status %d, want 413", status)
	}
}
