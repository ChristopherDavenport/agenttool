package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The names, descriptions and parameters of the resource tools, as RFC
// 0001 states them word for word: a definition is hashed, so two
// implementations that offer these tools must offer the same bytes.
const (
	listResourcesName = "list_resources"
	readResourceName  = "read_resource"

	listResourcesDescription = "List the resources this server offers, a page at a time, and the URI templates it reads others by. Read one with read_resource."
	readResourceDescription  = "Read a resource this server offers, by the URI list_resources or a resource_link gives, or one a URI template from list_resources makes."

	listResourcesParameters = `{"type":"object","properties":{"cursor":{"type":"string","description":"The cursor the previous page ended with, to list the page after it."}},"required":[]}`
	readResourceParameters  = `{"type":"object","properties":{"uri":{"type":"string","description":"The resource's URI."}},"required":["uri"]}`
)

// maxTemplatePages is how many pages of URI templates list_resources
// asks for before it gives up on a server that never stops paging.
const maxTemplatePages = 100

// resourceAnnotations are the resource tools' hints: they read and
// change nothing, and what they read may come from outside the server.
var resourceAnnotations = agenttool.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true}

// ResourceTools returns tools for the server's resources, so the model
// can list them and follow a resource_link a tool returned: one named
// list_resources, which lists the resources a page at a time with the
// URI templates, and one named read_resource, which reads one by its
// URI. Both are named as the server's tools are, under [WithPrefix],
// and read-only. It returns nil for a server that does not advertise
// resources.
//
// They are offered alongside [Remote.Tools] only when the host appends
// them, since MCP leaves resources to the client: a host that wants the
// model to follow links does. A server tool of the same name wins: when
// the server lists a list_resources or read_resource tool of its own,
// the one here is not returned, and the call of one returned before the
// server listed it fails. Their output follows the content mapping of
// [ResultOf]: a resource is listed in the form a resource_link is
// rendered in, and a read resource is rendered as an embedded one.
func (s *Remote) ResourceTools() []agenttool.Tool {
	res := s.current().session.InitializeResult()
	if res == nil || res.Capabilities == nil || res.Capabilities.Resources == nil {
		return nil
	}
	s.mu.RLock()
	taken := map[string]bool{}
	for _, e := range s.claims {
		taken[e.remote] = true
	}
	s.mu.RUnlock()
	// Open-world, as their annotations say, so their flow is MCP's
	// reading of that, unless the host declares one.
	opts := func(name string) []agenttool.Option {
		flow := agenttool.Flow{Reads: []agenttool.Label{agenttool.LabelUntrusted}, Sends: true}
		if f, ok := latest(s.opts.flows, name, s.Name(name)); ok {
			flow = f
		}
		return []agenttool.Option{agenttool.WithAnnotations(resourceAnnotations), agenttool.WithFlow(flow)}
	}
	var tools []agenttool.Tool
	if !taken[listResourcesName] {
		tools = append(tools, agenttool.NewFunc(s.Name(listResourcesName), listResourcesDescription, json.RawMessage(listResourcesParameters),
			func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
				var args struct {
					Cursor string `json:"cursor"`
				}
				if err := json.Unmarshal(orEmpty(call.Args), &args); err != nil {
					return agenttool.Result{}, fmt.Errorf("invalid arguments: %w", err)
				}
				return s.resourceRequest(ctx, listResourcesName, func(ctx context.Context, session *sdk.ClientSession) (agenttool.Result, error) {
					return listResources(ctx, session, args.Cursor)
				})
			}, opts(listResourcesName)...))
	}
	if !taken[readResourceName] {
		tools = append(tools, agenttool.NewFunc(s.Name(readResourceName), readResourceDescription, json.RawMessage(readResourceParameters),
			func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
				var args struct {
					URI string `json:"uri"`
				}
				if err := json.Unmarshal(orEmpty(call.Args), &args); err != nil {
					return agenttool.Result{}, fmt.Errorf("invalid arguments: %w", err)
				}
				if args.URI == "" {
					return agenttool.Result{}, errors.New("invalid arguments: uri: required")
				}
				return s.resourceRequest(ctx, readResourceName, func(ctx context.Context, session *sdk.ClientSession) (agenttool.Result, error) {
					res, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: args.URI})
					if err != nil {
						return agenttool.Result{}, err
					}
					return readResult(res), nil
				})
			}, opts(readResourceName)...))
	}
	return tools
}

func orEmpty(args json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		return json.RawMessage("{}")
	}
	return args
}

// resourceRequest sends one request of a resource tool on the session
// as it is, restarting an ended one with [WithReconnect], unless the
// server now lists a tool of the same name, which wins. [Remote.Close]
// ends it with [ErrClosed], as it ends a call, and the server's limits
// from [WithCallTimeout] bound it as they bound a call, a limit naming
// the tool by its name on the server or under the prefix winning over
// the server's.
func (s *Remote) resourceRequest(ctx context.Context, name string, fn func(context.Context, *sdk.ClientSession) (agenttool.Result, error)) (agenttool.Result, error) {
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	limit := s.opts.timeout
	if t, ok := latest(s.opts.timeouts, name, s.Name(name)); ok {
		limit = t
	}
	lim := startLimiter(s.Name(name), limit, stop)
	defer lim.end()
	l, err := s.live(ctx)
	if err != nil {
		if cause := context.Cause(ctx); stoppedBy(cause) {
			err = cause
		}
		return agenttool.Result{}, fmt.Errorf("mcp: %s: %w", name, err)
	}
	id := s.callSeq.Add(1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return agenttool.Result{}, fmt.Errorf("mcp: %s: %w", name, ErrClosed)
	}
	for _, e := range s.claims {
		if e.remote == name {
			s.mu.Unlock()
			return agenttool.Result{}, fmt.Errorf("mcp: %s: the server now offers a tool of this name, which is used in its place", name)
		}
	}
	s.asking[id] = stop
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.asking, id)
		s.mu.Unlock()
	}()
	r, err := fn(ctx, l.session)
	if err != nil {
		if cause := context.Cause(ctx); stoppedBy(cause) {
			err = cause
		}
		return agenttool.Result{}, fmt.Errorf("mcp: %s: %w", name, err)
	}
	return r, nil
}

// listResources lists one page of resources, and on the first page the
// server's URI templates, one line each, then the line next_cursor and
// the cursor when there is another page; "no resources" when there is
// nothing to list. A resource or template whose audience leaves out the
// assistant is not listed.
//
// Every page of templates is listed, up to [maxTemplatePages]. A server
// that refuses the first, with an error response, has no templates and
// lists none. Anything else that keeps the listing from finishing, a
// later page refused, a cursor the server already gave, too many pages
// or the end of the call, is the call's error, never a listing cut
// short.
func listResources(ctx context.Context, session *sdk.ClientSession, cursor string) (agenttool.Result, error) {
	page, err := session.ListResources(ctx, &sdk.ListResourcesParams{Cursor: cursor})
	if err != nil {
		return agenttool.Result{}, err
	}
	var lines []string
	for _, r := range page.Resources {
		if r == nil || !forAssistant(r.Annotations) {
			continue
		}
		var size *int64
		if r.Size > 0 {
			size = &r.Size
		}
		lines = append(lines, linkLine("resource_link", r.URI, r.Name, r.Description, r.MIMEType, size))
	}
	if cursor == "" {
		templates, err := listTemplates(ctx, session)
		if err != nil {
			return agenttool.Result{}, err
		}
		lines = append(lines, templates...)
	}
	if page.NextCursor != "" {
		lines = append(lines, "next_cursor "+page.NextCursor)
	}
	if len(lines) == 0 {
		return agenttool.Text("no resources"), nil
	}
	return agenttool.Text(strings.Join(lines, "\n")), nil
}

// listTemplates lists every page of the server's URI templates as
// lines; see [listResources] for when it stops.
func listTemplates(ctx context.Context, session *sdk.ClientSession) ([]string, error) {
	var lines []string
	seen := map[string]bool{}
	next := ""
	for page := 0; ; page++ {
		if page == maxTemplatePages {
			return nil, fmt.Errorf("resource templates: the server gave more than %d pages", maxTemplatePages)
		}
		templates, err := session.ListResourceTemplates(ctx, &sdk.ListResourceTemplatesParams{Cursor: next})
		if err != nil {
			var refused *jsonrpc.Error
			if page == 0 && ctx.Err() == nil && errors.As(err, &refused) {
				return nil, nil
			}
			return nil, fmt.Errorf("resource templates: %w", err)
		}
		for _, t := range templates.ResourceTemplates {
			if t == nil || !forAssistant(t.Annotations) {
				continue
			}
			lines = append(lines, linkLine("resource_template", t.URITemplate, t.Name, t.Description, t.MIMEType, nil))
		}
		next = templates.NextCursor
		if next == "" {
			return lines, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("resource templates: the server repeated the cursor %q", next)
		}
		seen[next] = true
	}
}

// readResult renders a read resource's contents as [ResultOf] renders
// embedded resources: text alone as text, joined by newlines, and with a
// blob among them as parts. The SDK result is the details.
func readResult(res *sdk.ReadResourceResult) agenttool.Result {
	var parts openresponses.Contents
	var texts []string
	textOnly := true
	for _, c := range res.Contents {
		if c == nil {
			continue
		}
		if len(c.Blob) > 0 {
			textOnly = false
			parts = append(parts, blobPart(c))
			continue
		}
		text := resourceText(c)
		texts = append(texts, text)
		parts = append(parts, &openresponses.InputText{Text: text})
	}
	out := agenttool.Result{Details: res}
	if textOnly {
		out.Output.Text = strings.Join(texts, "\n")
	} else {
		out.Output.Parts = parts
	}
	return out
}
