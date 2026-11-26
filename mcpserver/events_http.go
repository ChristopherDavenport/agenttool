package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPHandler wraps the Streamable HTTP handler of the server so that a
// request whose routing headers disagree with its params is refused
// with HTTP 400, as the SEP says, and not with the status 200 the SDK
// gives an error a handler or middleware returns for a custom method.
//
// The refusal is the same one the server makes without the wrapper: a
// JSON-RPC error -32020 ([EventsCodeHeaderMismatch]) with
// data.mismatch naming the header and the value in the params. The
// wrapper only moves it to the status, by running the same check
// ([Events.checkHeaders]) before the SDK sees the request. A request
// that agrees, one for another method, one that is not a POST, and one
// whose body is not a single JSON-RPC request is passed on untouched, so
// the server still answers a mismatch the wrapper did not see, with 200.
//
// Wrap the handler that [sdk.NewStreamableHTTPHandler] returns, and put
// anything that must run first, such as authentication, outside it. The
// wrapper reads a body up to the SDK's limit before passing it on, and
// answers a refusal itself, so the SDK's own origin checks run only for
// a request that agrees; the refusal carries nothing but the error the
// server would give. It does nothing for a protocol before 2026-07-28,
// which sends no routing headers.
func (e *Events) HTTPHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil || r.Header.Get(protocolHeader) < headersFrom {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sdk.DefaultMaxRequestBodyBytes))
		if err != nil {
			status := http.StatusBadRequest
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "failed to read the request body", status)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if !bytes.Contains(body, []byte("events/")) {
			next.ServeHTTP(w, r)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params *eventsParams   `json:"params"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.ID) == 0 || !strings.HasPrefix(req.Method, "events/") {
			next.ServeHTTP(w, r)
			return
		}
		if herr := e.checkHeaders(req.Method, req.Params, r.Header); herr != nil {
			out, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": herr.wireValue()})
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(out)
			return
		}
		next.ServeHTTP(w, r)
	})
}
