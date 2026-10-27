package mcpclient

//lint:file-ignore SA1019 roots and logging are deprecated from protocol version 2026-07-28 and still work there; this file is their mapping, and the only place it is used

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// hostOptions is what the host takes part in that the contract has no
// field for: the roots it offers the server and the log messages it
// takes from it.
type hostOptions struct {
	// roots are offered when rootsSet; see [WithRoots].
	roots    []*sdk.Root
	rootsSet bool
	// level and onLog are the log messages the host takes; see
	// [WithLogging].
	level sdk.LoggingLevel
	onLog func(context.Context, *sdk.LoggingMessageParams)
}

// WithRoots offers the server roots, the directories it may work in,
// and answers its roots/list request with them. Without it the client
// does not offer roots at all, so a server that scopes itself to its
// client's roots is told there are none to ask for rather than asking
// and being given an empty list. The roots are the host's: a host that
// runs its agent in a workspace offers the workspace's root.
//
//	mcpclient.WithRoots(&mcp.Root{URI: "file:///home/me/project", Name: "project"})
//
// The roots are fixed for the remote's life, and offered without
// list-changed notifications. A capability the host sets itself through
// [WithClientOptions] wins: one that names roots offers them whether or
// not this option is given. Roots are deprecated from protocol version
// 2026-07-28 and still work there.
func WithRoots(roots ...*sdk.Root) Option {
	return func(o *options) {
		o.host.roots = append(o.host.roots, roots...)
		o.host.rootsSet = true
	}
}

// WithLogging takes the server's log messages at level and above and
// hands each to fn, which belongs to the host: a log message is for
// whoever runs the server and never reaches a call's output, its
// details or the model. The level goes in each call's _meta, which is
// how a server from protocol version 2026-07-28 is asked for messages,
// and without which it sends none; an older server that advertises
// logging is sent logging/setLevel when its session starts.
//
// fn's context carries the call, through [agenttool.CallFrom], when the
// message belongs to a call the client can name: the message arrived
// with that call's request, or exactly one call of the remote was in
// flight. A message about no call, or about one of several running,
// arrives on a context without one. A nil fn takes no messages, as
// without the option. A LoggingMessageHandler set through
// [WithClientOptions] is called as well, before fn. Logging is
// deprecated from 2026-07-28 and still works there.
func WithLogging(level sdk.LoggingLevel, fn func(ctx context.Context, msg *sdk.LoggingMessageParams)) Option {
	return func(o *options) { o.host.level, o.host.onLog = level, fn }
}

// offerHost states in caps the roots the host offers, and has the
// client hand log messages to the host, when it takes them.
func (s *Remote) offerHost(caps *sdk.ClientCapabilities, clientOpts *sdk.ClientOptions) {
	h := s.opts.host
	if h.rootsSet && caps.RootsV2 == nil {
		caps.RootsV2 = &sdk.RootCapabilities{}
	}
	if h.onLog != nil {
		userLog := clientOpts.LoggingMessageHandler
		clientOpts.LoggingMessageHandler = func(ctx context.Context, req *sdk.LoggingMessageRequest) {
			if userLog != nil {
				userLog(ctx, req)
			}
			s.onLog(ctx, req)
		}
	}
}

// addRoots gives the client the host's roots, which it answers
// roots/list with.
func (h hostOptions) addRoots(client *sdk.Client) {
	if len(h.roots) > 0 {
		client.AddRoots(h.roots...)
	}
}

// callMeta is the _meta a call starts with: the log level when the host
// takes log messages, and nil otherwise.
func (h hostOptions) callMeta() sdk.Meta {
	if h.onLog == nil {
		return nil
	}
	return sdk.Meta{sdk.MetaKeyLogLevel: h.level}
}

// onLog hands a server's log message to the host's function, with the
// call it belongs to on the context when that can be told; see
// [WithLogging]. The call's context is taken without its cancellation,
// so a message that arrives as its call ends is still handed over whole.
func (s *Remote) onLog(ctx context.Context, req *sdk.LoggingMessageRequest) {
	if req == nil || req.Params == nil {
		return
	}
	if own, _ := ctx.Value(ownCallKey{}).(*ownCall); own != nil && own.remote == s {
		ctx = context.WithoutCancel(own.ctx)
	} else if c, ok := s.soleCall(); ok {
		ctx = context.WithoutCancel(c.ctx)
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	s.opts.host.onLog(ctx, req.Params)
}

// setLogLevel asks a server before protocol version 2026-07-28 for its
// log messages, when the host takes them and the server advertises
// logging; a later server is asked on each call instead. It is best
// effort: a server that refuses sends no messages, which is what the
// host would have had without the option.
func (s *Remote) setLogLevel(ctx context.Context, l *link) {
	res := l.session.InitializeResult()
	if s.opts.host.onLog == nil || res == nil || res.Capabilities == nil || res.Capabilities.Logging == nil || res.ProtocolVersion >= "2026-07-28" {
		return
	}
	_ = l.session.SetLoggingLevel(ctx, &sdk.SetLoggingLevelParams{Level: s.opts.host.level})
}
