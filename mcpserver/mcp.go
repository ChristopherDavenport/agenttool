// Package mcpserver serves agenttool.Tool values over MCP: the serve
// side of MCP and the inverse of mcpclient.
//
// The native contract is the centre and MCP is an edge: a tool's name,
// description and Parameters become the MCP tool definition, and the
// SDK's handler decodes the call, runs Execute and maps the output back
// to MCP content. The official Go SDK owns transports (stdio, streamable
// HTTP) and protocol negotiation.
//
//	server := mcpserver.NewServer("files", "1.0", readFile, writeFile)
//	err := server.Run(ctx, &sdk.StdioTransport{})
//
// BeforeToolCall and AfterToolCall do not run here. Policy belongs to
// whoever hosts the loop, and this front hosts only tools; a caller who
// wants a policy applies it to the tools before serving them.
package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// emptySchema describes a tool that takes no arguments; MCP requires an
// object schema on every tool.
var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// NewServer builds an SDK server named name that serves tools.
func NewServer(name, version string, tools ...agenttool.Tool) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: name, Version: version}, nil)
	AddTools(s, tools...)
	return s
}

// AddTools registers tools on an SDK server. Each tool's Parameters is
// the MCP input schema verbatim, so no reflection happens in the SDK; a
// nil Parameters becomes an empty object schema. The handler validates
// the arguments against that schema, as MCP servers are expected to, so
// a missing required property or a wrong type is refused before the
// tool runs; a schema the validator cannot resolve is served as is and
// the tool's own decoding is the only check. It then runs Execute with
// the raw arguments and maps the result with [ContentOf]; a returned
// error, a validation failure included, sets isError with the message
// as text so the calling model can see it and retry.
//
// When the request carries a progress token, Call.OnUpdate forwards each
// update as a progress notification whose message is the update's text.
// An update whose Details is an agenttool.ProgressInfo supplies the
// notification's progress, total and message, so a tool that mcpclient
// consumed and this package serves again keeps its numbers; otherwise
// updates are numbered in order with no total.
func AddTools(s *sdk.Server, tools ...agenttool.Tool) {
	for _, tl := range tools {
		s.AddTool(Definition(tl), Handler(tl))
	}
}

// Definition builds the MCP tool definition for tl.
func Definition(tl agenttool.Tool) *sdk.Tool {
	schema := tl.Parameters()
	if len(schema) == 0 {
		schema = emptySchema
	}
	return &sdk.Tool{Name: tl.Name(), Description: tl.Description(), InputSchema: schema}
}

var callSeq atomic.Int64

// Handler builds the SDK handler that runs tl.
func Handler(tl agenttool.Tool) sdk.ToolHandler {
	resolved := resolveSchema(tl.Parameters())
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		call := agenttool.Call{
			ID:   "mcp_" + strconv.FormatInt(callSeq.Add(1), 10),
			Args: req.Params.Arguments,
		}
		if len(call.Args) == 0 {
			call.Args = json.RawMessage("{}")
		}
		if err := validate(resolved, call.Args); err != nil {
			return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}}}, nil
		}
		if token := req.Params.GetProgressToken(); token != nil && req.Session != nil {
			var n atomic.Int64
			session := req.Session
			call.OnUpdate = func(r agenttool.Result) {
				params := &sdk.ProgressNotificationParams{ProgressToken: token, Progress: float64(n.Add(1)), Message: r.Output.String()}
				if p, ok := r.Details.(agenttool.ProgressInfo); ok {
					params.Progress, params.Total = p.Progress, p.Total
					if p.Message != "" {
						params.Message = p.Message
					}
				}
				_ = session.NotifyProgress(ctx, params)
			}
		}
		res, err := tl.Execute(ctx, call)
		if err != nil {
			return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}}}, nil
		}
		return &sdk.CallToolResult{Content: ContentOf(res.Output)}, nil
	}
}

// resolveSchema prepares a tool's schema for validation. It returns nil
// when there is no schema or the validator cannot resolve it.
func resolveSchema(raw json.RawMessage) *jsonschema.Resolved {
	if len(raw) == 0 {
		raw = emptySchema
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	switch schema.Schema {
	case "", "http://json-schema.org/draft-07/schema#", "https://json-schema.org/draft-07/schema#", "https://json-schema.org/draft/2020-12/schema":
	default:
		// The validator handles draft-07 and 2020-12 only.
		return nil
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil
	}
	return resolved
}

// validate checks raw arguments against a resolved schema. The error is
// phrased like the tool package's own decode errors.
func validate(resolved *jsonschema.Resolved, raw json.RawMessage) error {
	if resolved == nil {
		return nil
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// ContentOf maps a function call output to MCP content. Text becomes one
// text block. Parts map by type: textual parts to text blocks, an
// input_image with a data URL to an image block and one with a plain URL
// to a resource link, an input_file with data to an embedded blob
// resource and one with a URL to a resource link. Anything else is
// emitted as its JSON.
func ContentOf(out openresponses.FunctionCallOutputData) []sdk.Content {
	if out.Parts == nil {
		return []sdk.Content{&sdk.TextContent{Text: out.Text}}
	}
	content := make([]sdk.Content, 0, len(out.Parts))
	for _, part := range out.Parts {
		switch p := part.(type) {
		case *openresponses.Text:
			content = append(content, &sdk.TextContent{Text: p.Text})
		case *openresponses.InputText:
			content = append(content, &sdk.TextContent{Text: p.Text})
		case *openresponses.OutputText:
			content = append(content, &sdk.TextContent{Text: p.Text})
		case *openresponses.InputImage:
			if mime, data, ok := parseDataURL(p.ImageURL); ok {
				content = append(content, &sdk.ImageContent{MIMEType: mime, Data: data})
			} else {
				content = append(content, &sdk.ResourceLink{URI: p.ImageURL, Name: "image"})
			}
		case *openresponses.InputFile:
			switch {
			case p.FileData != "":
				data, err := base64.StdEncoding.DecodeString(p.FileData)
				if err != nil {
					content = append(content, &sdk.TextContent{Text: p.FileData})
					continue
				}
				uri := p.FileURL
				if uri == "" {
					uri = "file:///" + strings.TrimPrefix(p.Filename, "/")
				}
				content = append(content, &sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: uri, Blob: data}})
			case p.FileURL != "":
				content = append(content, &sdk.ResourceLink{URI: p.FileURL, Name: p.Filename})
			default:
				content = append(content, &sdk.TextContent{Text: p.Filename})
			}
		default:
			data, err := json.Marshal(part)
			if err != nil {
				continue
			}
			content = append(content, &sdk.TextContent{Text: string(data)})
		}
	}
	return content
}

// parseDataURL splits a base64 data URL into its media type and bytes.
func parseDataURL(url string) (string, []byte, bool) {
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return "", nil, false
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", nil, false
	}
	mime, b64 := strings.CutSuffix(meta, ";base64")
	if !b64 {
		return "", nil, false
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, false
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	return mime, data, true
}
