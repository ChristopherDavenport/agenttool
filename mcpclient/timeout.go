package mcpclient

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// callTimeout is the limits a call runs under; see [WithCallTimeout]. A
// zero limit is none.
type callTimeout struct {
	idle, max time.Duration
}

func (t callTimeout) none() bool { return t.idle <= 0 && t.max <= 0 }

// WithCallTimeout bounds a call of the named tools, or of every tool of
// the server when no name is given, by an idle time and a maximum time,
// either of which may be zero for none. The idle time runs from the
// call's start and from each progress notification the server sends for
// it, so a long call that reports progress runs on while one that has
// gone quiet is stopped; the maximum holds whatever progress says. Time
// the call spends waiting on the user, a question of its put to the
// call's elicitor with [WithElicitation], counts toward neither.
//
// On expiry the call is stopped as its caller's cancellation would stop
// it: its request is cancelled, which sends the server
// notifications/cancelled, and the call returns a [*TimeoutError]
// naming the limit. The server may have acted on the call before it
// was stopped, so its outcome is ambiguous, and it is not sent again.
//
// Names are matched before and after prefixing, as [WithResource]'s
// are, and the last option naming a tool wins over an earlier one and
// over the server's default; WithCallTimeout(0, 0, "build") lifts the
// limits from one tool. MCP asks a client to set timeouts, and the
// harness's own deadline still holds: whichever ends first stops the
// call.
//
//	mcpclient.WithCallTimeout(30*time.Second, 10*time.Minute)
//	mcpclient.WithCallTimeout(0, time.Hour, "build")
func WithCallTimeout(idle, max time.Duration, names ...string) Option {
	return func(o *options) {
		t := callTimeout{idle: idle, max: max}
		if len(names) == 0 {
			o.timeout = t
			return
		}
		claimOn(o, &o.timeouts, t, names)
	}
}

// TimeoutError is what a call stopped by [WithCallTimeout] returns. Idle
// says which limit ran out, the idle time or the maximum, and Limit is
// its length.
type TimeoutError struct {
	Tool  string
	Idle  bool
	Limit time.Duration
}

func (e *TimeoutError) Error() string {
	if e.Idle {
		return fmt.Sprintf("the call sent no progress for %s, its idle limit, and was stopped; it may or may not have taken effect", e.Limit)
	}
	return fmt.Sprintf("the call ran for %s, its time limit, and was stopped; it may or may not have taken effect", e.Limit)
}

// limiter stops one call when a limit of its runs out. Its clocks stop
// while a question of the call is with the user and start again when the
// last is answered, the idle time afresh and the maximum with what was
// left of it.
type limiter struct {
	tool  string
	limit callTimeout
	stop  context.CancelCauseFunc

	mu      sync.Mutex
	idle    *time.Timer
	max     *time.Timer
	maxLeft time.Duration // of the maximum, when maxFrom is zero
	maxFrom time.Time     // when the maximum's clock last started
	asking  int
	done    bool
	// gen numbers the clocks, so a timer that fired as it was replaced
	// or stopped, and waits on mu, finds itself stale and does nothing.
	gen int
}

// startLimiter starts the clocks of a call that stop ends, and returns
// nil when there are no limits.
func startLimiter(tool string, limit callTimeout, stop context.CancelCauseFunc) *limiter {
	if limit.none() {
		return nil
	}
	l := &limiter{tool: tool, limit: limit, stop: stop, maxLeft: limit.max}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.run()
	return l
}

// run starts the clocks; the caller holds mu.
func (l *limiter) run() {
	l.gen++
	gen := l.gen
	if l.limit.idle > 0 {
		l.idle = time.AfterFunc(l.limit.idle, func() { l.expire(gen, true) })
	}
	if l.limit.max > 0 {
		l.maxFrom = time.Now()
		l.max = time.AfterFunc(l.maxLeft, func() { l.expire(gen, false) })
	}
}

// halt stops the clocks, keeping what is left of the maximum; the caller
// holds mu.
func (l *limiter) halt() {
	l.gen++
	if l.idle != nil {
		l.idle.Stop()
		l.idle = nil
	}
	if l.max != nil {
		l.max.Stop()
		l.max = nil
		l.maxLeft -= time.Since(l.maxFrom)
		if l.maxLeft < 0 {
			l.maxLeft = 0
		}
	}
}

// expire stops the call with the limit that ran out, unless the clocks
// of gen have since been stopped or replaced.
func (l *limiter) expire(gen int, idle bool) {
	l.mu.Lock()
	if l.done || gen != l.gen {
		l.mu.Unlock()
		return
	}
	l.done = true
	l.halt()
	l.mu.Unlock()
	limit := l.limit.max
	if idle {
		limit = l.limit.idle
	}
	l.stop(&TimeoutError{Tool: l.tool, Idle: idle, Limit: limit})
}

// progress restarts the idle clock.
func (l *limiter) progress() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done || l.asking > 0 || l.idle == nil {
		return
	}
	// Both clocks start again under a new generation, the maximum with
	// what is left of it, so the idle timer replaced here is stale.
	l.halt()
	l.run()
}

// ask stops the clocks while a question of the call is with the user,
// and returns the function that starts them again once it is answered.
func (l *limiter) ask() (answered func()) {
	if l == nil {
		return func() {}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asking++
	if l.asking == 1 && !l.done {
		l.halt()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.asking--
			if l.asking == 0 && !l.done {
				l.run()
			}
		})
	}
}

// end stops the clocks of a call that returned.
func (l *limiter) end() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.done = true
	l.halt()
}
