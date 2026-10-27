package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrSessionEnded is what a request returns when the server's session
// ended under it, or ended before it and could not be restarted; see
// [WithReconnect]. A tool call that wraps it may or may not have run.
var ErrSessionEnded = errors.New("mcp: the server's session ended")

// ErrToolChanged is what a call returns, unsent, when the server
// restarted after the call was decided and no longer lists its tool as
// it was: the tool is gone, or its definition or claims differ. See
// [WithReconnect].
var ErrToolChanged = errors.New("mcp: the tool changed when the server restarted")

// reconnectTimeout bounds one attempt to restart a server: the dial,
// the session's initialisation and the listing of its tools.
const reconnectTimeout = 30 * time.Second

// endGrace is how long a request that failed waits for its session to
// report that it ended, to tell a request the end cut from one that
// failed on a live session, and so that a request the end cut returns
// only once the next request will find the session ended. The session
// reports it as soon as its connection is idle, which is at once unless
// a handler of the client's is still running.
const endGrace = 500 * time.Millisecond

// WithReconnect restarts the server when its session ends, a process
// that crashed or a connection that dropped, by connecting to a new
// transport from dial. [Connect] uses the transport it is given for the
// first session and dial for each one after it, since a transport such
// as [sdk.CommandTransport] connects once.
//
// The restart is lazy: the remote watches its session, and the next
// request that would send on it after it ended, a call, a claim, a
// [Remote.Facts] or a [Remote.Refresh], restarts it first and then
// sends. Requests that find it ended together share one attempt, which
// is bounded by 30 seconds whatever their contexts. dial's context ends
// when the attempt times out, fails, or the remote is closed during it,
// and not when it succeeds, so a transport may tie the server's life to
// it. An attempt that fails fails the requests waiting on it with
// [ErrSessionEnded], and the next request tries again; nothing retries
// in the background. [Remote.Close] stops an attempt and is final.
//
// A tool call in flight when the session ends returns its error,
// wrapping [ErrSessionEnded]: it may or may not have run, and it is not
// sent again. A request without side effects, a claim, [Remote.Facts]
// or a listing, is sent once more on the restarted session.
//
// A request the end of its session cut returns once the remote has seen
// the session end, so the very next request restarts the server rather
// than being sent on the dead session and cut too. The wait is bounded:
// a session that has not reported its end within half a second, because
// a handler of the client's is still running on it, or a request whose
// context ends first, returns cut without it, and the next request may
// then be cut as well.
//
// A restart is a refresh. The new session is initialised as the first
// was, with every option given to Connect, and its tools are listed
// and replace the snapshot and its claims in one step, as a
// tool-list-changed refresh does, so [Remote.Tools] returns the
// restarted server's list even when it differs. A call decided on a
// tool from before the restart is sent only when the server still lists
// that tool with the same definition and claims; otherwise it returns
// [ErrToolChanged] unsent, to be decided again against the tool as it
// is now. A tool's claims answer the same way.
//
// Without the option a session that ends stays ended, and every later
// request fails, as before.
func WithReconnect(dial func(ctx context.Context) (sdk.Transport, error)) Option {
	return func(o *options) { o.dial = dial }
}

// ReconnectInfo describes one attempt to restart a server; see
// [WithReconnected].
type ReconnectInfo struct {
	// Err is why the attempt failed, and nil when it succeeded. A failed
	// attempt leaves the remote as it was, its session ended, and the
	// next request tries again.
	Err error
	// Server is the restarted server's name and version, as
	// [Remote.ServerInfo] gives it.
	Server sdk.Implementation
	// Changed says the restarted server's tools differ from the ones
	// before it: one was added or removed, or its definition or claims
	// changed.
	Changed bool
	// Before and After are the local names of the tools before and
	// after the restart, sorted.
	Before, After []string
}

// WithReconnected sets the function called after each attempt to
// restart the server, with [WithReconnect], so a host can warn that the
// server restarted, and lost whatever state it held, and say so in its
// record. It is called once per attempt, however many requests share
// it, outside the remote's locks, and before those requests go on. It
// is not called for an attempt that [Remote.Close] stopped.
func WithReconnected(fn func(ReconnectInfo)) Option {
	return func(o *options) { o.onReconnect = fn }
}

// link is one session of a remote and what its server said at
// initialize. A remote has one at a time and replaces it when it
// restarts the server.
type link struct {
	session *sdk.ClientSession
	// gen counts the restarts before this session: 0 for the first.
	gen int64
	// factsCap says the server advertises [FactsCapability], and
	// listChanged that it sends tool-list-changed notifications.
	factsCap    bool
	listChanged bool
	// ended is closed when the session ends.
	ended chan struct{}

	// closing makes the session's close happen once, whoever asks for
	// it first, a restart releasing a session that ended or
	// [Remote.Close], and closeErr is that close's error.
	closing  sync.Once
	closeErr error
}

// newLink wraps a session the client just connected, and watches it.
func newLink(session *sdk.ClientSession, gen int64) *link {
	l := &link{session: session, gen: gen, ended: make(chan struct{})}
	if res := session.InitializeResult(); res != nil && res.Capabilities != nil {
		if res.Capabilities.Tools != nil {
			l.listChanged = res.Capabilities.Tools.ListChanged
		}
		_, l.factsCap = res.Capabilities.Experimental[FactsCapability]
	}
	go func() {
		_ = session.Wait()
		close(l.ended)
	}()
	return l
}

// isEnded reports whether the session has ended.
func (l *link) isEnded() bool {
	select {
	case <-l.ended:
		return true
	default:
		return false
	}
}

// close closes the session once. Every caller gets that close's error,
// and one that comes while it is in flight waits for it.
func (l *link) close() error {
	l.closing.Do(func() { l.closeErr = l.session.Close() })
	return l.closeErr
}

// endedWithin reports whether the session ends within d, giving up
// early when ctx ends.
func (l *link) endedWithin(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-l.ended:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	return l.isEnded()
}

// serverInfo is the server's implementation as it gave it at initialize.
func (l *link) serverInfo() sdk.Implementation {
	res := l.session.InitializeResult()
	if res == nil || res.ServerInfo == nil {
		return sdk.Implementation{}
	}
	return *res.ServerInfo
}

// toolRef is a listed tool as a call or a claim of it was decided on:
// its names, the session it was listed on, and its definition.
type toolRef struct {
	remote, name string
	gen          int64
	def          string
}

// definition is what a listed tool is, for telling whether a restarted
// server still offers the same tool: the tool as listed, its _meta and
// so its claims included, and whether the remote takes its claims.
func definition(t *sdk.Tool, claims bool) (string, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("mcp: tool %q: %w", t.Name, err)
	}
	if claims {
		return "claims " + string(b), nil
	}
	return string(b), nil
}

// attempt is one restart of the server, shared by the requests that
// found its session ended.
type attempt struct {
	done chan struct{}
	link *link // set before done is closed
	err  error

	mu     sync.Mutex
	over   bool
	cancel context.CancelCauseFunc
}

// stop ends the attempt's context with cause, unless it is over: the
// context of an attempt that succeeded is never ended.
func (a *attempt) stop(cause error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.over {
		a.cancel(cause)
	}
}

// finish marks the attempt over, and ends its context when it failed.
func (a *attempt) finish(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.over = true
	if err != nil {
		a.cancel(err)
	}
}

// live returns the session to send on, restarting the server first when
// its session has ended and [WithReconnect] is on. Without the option
// it is the session there is, as before.
func (s *Remote) live(ctx context.Context) (*link, error) {
	s.mu.RLock()
	l, closed := s.link, s.closed
	s.mu.RUnlock()
	switch {
	case s.opts.dial == nil:
		return l, nil
	case closed:
		return nil, ErrClosed
	case !l.isEnded():
		return l, nil
	}
	return s.restart(ctx, l)
}

// restart restarts the server whose session old has ended, joining the
// attempt in flight when there is one, and waits for it or for ctx.
func (s *Remote) restart(ctx context.Context, old *link) (*link, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if s.link != old {
		l := s.link
		s.mu.Unlock()
		return l, nil
	}
	a := s.attempt
	if a == nil {
		actx, cancel := context.WithCancelCause(context.Background())
		a = &attempt{done: make(chan struct{}), cancel: cancel}
		s.attempt = a
		go s.redial(actx, old, a)
	}
	s.mu.Unlock()
	select {
	case <-a.done:
		return a.link, a.err
	case <-ctx.Done():
		if cause := context.Cause(ctx); errors.Is(cause, ErrClosed) {
			return nil, cause
		}
		return nil, ctx.Err()
	}
}

// redial runs one attempt: it dials, connects and lists, reports the
// outcome, and releases the requests waiting on it.
func (s *Remote) redial(ctx context.Context, old *link, a *attempt) {
	timeout := time.AfterFunc(s.reconnectTimeout, func() {
		a.stop(fmt.Errorf("timed out after %v", s.reconnectTimeout))
	})
	// What is left of the old session is released, without waiting on
	// it.
	go func() { _ = old.close() }()
	l, info, err := s.reconnect(ctx, old)
	timeout.Stop()
	if err != nil {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(err, cause) {
			err = fmt.Errorf("%w: %w", err, cause)
		}
	}
	a.finish(err)
	s.mu.Lock()
	if s.attempt == a {
		s.attempt = nil
	}
	closed := s.closed
	s.mu.Unlock()
	switch {
	case closed:
		a.err = ErrClosed
	case err != nil:
		a.err = fmt.Errorf("%w and could not be restarted; the next request tries again: %w", ErrSessionEnded, err)
		info.Err = a.err
	default:
		a.link = l
	}
	if s.opts.onReconnect != nil && !closed {
		s.opts.onReconnect(info)
	}
	close(a.done)
}

// reconnect opens a session on a transport from dial, initialised as
// the first was, and adopts it with its tools.
func (s *Remote) reconnect(ctx context.Context, old *link) (*link, ReconnectInfo, error) {
	t, err := s.opts.dial(ctx)
	if err != nil {
		return nil, ReconnectInfo{}, fmt.Errorf("dial: %w", err)
	}
	session, err := s.client.Connect(ctx, t, s.sessionOpts)
	if err != nil {
		return nil, ReconnectInfo{}, fmt.Errorf("connect: %w", err)
	}
	l := newLink(session, old.gen+1)
	before, after, err := s.adopt(ctx, l)
	if err != nil {
		_ = session.Close()
		return nil, ReconnectInfo{}, err
	}
	return l, ReconnectInfo{
		Server:  l.serverInfo(),
		Changed: !maps.Equal(before, after),
		Before:  slices.Sorted(maps.Keys(before)),
		After:   slices.Sorted(maps.Keys(after)),
	}, nil
}

// adopt lists l's tools and makes l the remote's session together with
// them, so the session, the snapshot and its claims change in one step,
// and settles every notification received so far, since the listing is
// a whole one. It returns the tools before and after, by local name and
// definition. It refuses once the remote is closed.
func (s *Remote) adopt(ctx context.Context, l *link) (before, after map[string]string, err error) {
	s.listing.Lock()
	defer s.listing.Unlock()
	gen := s.seen.Load()
	tools, claims, err := s.list(ctx, l)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, ErrClosed
	}
	before = defs(s.claims)
	s.link, s.tools, s.claims = l, tools, claims
	s.settled = max(s.settled, gen)
	s.settledErr = nil
	close(s.settledCh)
	s.settledCh = make(chan struct{})
	return before, defs(claims), nil
}

// defs maps each tool of a snapshot's claims to its definition.
func defs(claims map[string]claimEntry) map[string]string {
	out := make(map[string]string, len(claims))
	for name, e := range claims {
		out[name] = e.def
	}
	return out
}

// sendOn returns the session to send a request of the tool ref on,
// restarting the server first if it must. When the server has
// restarted since ref was listed, the tool must still be listed as it
// was: otherwise the request is not sent and fails with
// [ErrToolChanged].
func (s *Remote) sendOn(ctx context.Context, ref toolRef) (*link, error) {
	if _, err := s.live(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	l := s.link
	e, ok := s.claims[ref.name]
	s.mu.RUnlock()
	switch {
	case l.gen == ref.gen:
		return l, nil
	case !ok:
		return nil, fmt.Errorf("%w: it no longer lists %q; the request was not sent", ErrToolChanged, ref.name)
	case e.def != ref.def:
		return nil, fmt.Errorf("%w: %q is listed differently; the request was not sent, and is to be decided again", ErrToolChanged, ref.name)
	}
	return l, nil
}

// cut reports whether a request on l that failed with err, its context
// still live, failed because the session ended rather than because the
// server answered with an error. Unless the server answered, it waits
// up to [endGrace] for the session to report its end, or until ctx
// ends, so that a request it cut returns only once the next request
// will find the session ended and restart the server. A request
// refused because the connection closed is cut whether or not the end
// is reported in time. It takes no lock of its own, and requests cut
// together wait on the one channel the session's end closes. It is only
// asked with [WithReconnect] on.
func cut(ctx context.Context, l *link, err error) bool {
	closed := errors.Is(err, sdk.ErrConnectionClosed)
	var werr *jsonrpc.Error
	if !closed && errors.As(err, &werr) {
		return false
	}
	return l.endedWithin(ctx, endGrace) || closed
}

// resend reports whether a side-effect-free request on l that failed
// with err is to be sent once more, on a restarted session: it was cut,
// and its context did not end while that was being told.
func (s *Remote) resend(ctx context.Context, l *link, err error) bool {
	return s.opts.dial != nil && ctx.Err() == nil && cut(ctx, l, err) && ctx.Err() == nil
}
