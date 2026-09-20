package agenttool

import (
	"context"
	"fmt"
	"iter"
	"runtime/debug"
	"sync"
)

// DefaultMaxParallel bounds concurrent tool calls in a batch when the
// caller sets no limit.
const DefaultMaxParallel = 8

// Job is one call of a batch: the tool to run and the call to run it
// with.
type Job struct {
	Tool Tool
	Call Call
}

// Event is one step of a batch: a progress update when Final is false,
// otherwise the completion of the job at Index. Completions arrive in
// completion order, not job order.
type Event struct {
	Index  int
	Final  bool
	Result Result
	Err    error
}

// Executor runs batches of tool calls.
type Executor struct {
	// MaxParallel bounds concurrency; zero means [DefaultMaxParallel].
	MaxParallel int
	// Sequential forces every batch to run one job at a time in order.
	// A batch containing a [Sequential] tool runs that way regardless.
	Sequential bool
}

// Execute runs jobs and yields their events from the caller's
// goroutine, so a consumer never sees two events at once. Progress
// updates from a tool that calls Call.OnUpdate are forwarded as
// non-final events; the executor installs its own OnUpdate and chains
// to the one on the job, if any, from the yielding goroutine. A tool
// that panics completes with an error. Breaking out of the loop cancels
// the batch and waits for running tools to return.
func (e Executor) Execute(ctx context.Context, jobs []Job) iter.Seq[Event] {
	return func(yield func(Event) bool) {
		if len(jobs) == 0 {
			return
		}
		limit := e.MaxParallel
		if limit <= 0 {
			limit = DefaultMaxParallel
		}
		if e.Sequential || anySequential(jobs) {
			limit = 1
		}

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// events carries progress and completions. A remote tool may
		// report progress after its call returned, from a goroutine of
		// its own, so every send goes through send, which refuses once
		// the channel is closed.
		events := make(chan Event)
		var gate sync.RWMutex
		closed := false
		send := func(ev Event) {
			gate.RLock()
			defer gate.RUnlock()
			if closed {
				return
			}
			if ev.Final {
				// Completions are always delivered: the consumer reads
				// until every job has one.
				events <- ev
				return
			}
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		}

		var wg sync.WaitGroup
		done := make(chan struct{})
		go func() {
			defer close(done)
			sem := make(chan struct{}, limit)
			for i, job := range jobs {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					// Jobs that never started still complete, with the
					// cancellation as their error, so the consumer sees
					// one final event per job.
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						send(Event{Index: i, Final: true, Err: ctx.Err()})
					}(i)
					continue
				}
				wg.Add(1)
				go func(i int, job Job) {
					defer wg.Done()
					defer func() { <-sem }()
					res, err := run(ctx, job, func(r Result) {
						send(Event{Index: i, Result: r})
					})
					send(Event{Index: i, Final: true, Result: res, Err: err})
				}(i, job)
			}
			wg.Wait()
			// Late progress senders are released by the cancellation
			// below before the gate is taken, so this never waits on a
			// send nobody receives.
			gate.Lock()
			closed = true
			close(events)
			gate.Unlock()
		}()

		stopped := false
		remaining := len(jobs)
		finished := make([]bool, len(jobs))
		for ev := range events {
			if ev.Final {
				remaining--
				finished[ev.Index] = true
			} else if finished[ev.Index] {
				// A progress update that arrives after the job's result
				// is dropped so nothing follows a completion.
				continue
			}
			if stopped {
				continue
			}
			if !ev.Final && jobs[ev.Index].Call.OnUpdate != nil {
				jobs[ev.Index].Call.OnUpdate(ev.Result)
			}
			if !yield(ev) {
				stopped = true
				cancel()
			}
			if remaining == 0 {
				break
			}
		}
		// Every job has completed or the consumer left. Cancel so a
		// sender still blocked on a send returns, drain, and wait for
		// the producer to close the channel.
		cancel()
		for range events {
		}
		<-done
	}
}

// PanicError is the error a job completes with when its tool panicked.
// Error reports the tool and the panic value in one line, which is what
// the model sees; the stack is kept for hosts and subscribers that
// recover it with errors.As, and never reaches the conversation.
type PanicError struct {
	Tool  string
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("tool %q panicked: %v", e.Tool, e.Value)
}

// run executes one job, turning a panic into a [PanicError].
func run(ctx context.Context, job Job, onUpdate func(Result)) (res Result, err error) {
	if job.Tool == nil {
		return Result{}, fmt.Errorf("no tool for call %q", job.Call.ID)
	}
	name := job.Tool.Name()
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Tool: name, Value: r, Stack: debug.Stack()}
		}
	}()
	call := job.Call
	call.OnUpdate = onUpdate
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return job.Tool.Execute(ctx, call)
}

func anySequential(jobs []Job) bool {
	for _, j := range jobs {
		if j.Tool != nil && IsSequential(j.Tool) {
			return true
		}
	}
	return false
}

// Results collects the final results of a batch in job order. It is the
// convenience over [Executor.Execute] for callers that do not need
// progress.
func (e Executor) Results(ctx context.Context, jobs []Job) ([]Result, []error) {
	results := make([]Result, len(jobs))
	errs := make([]error, len(jobs))
	for ev := range e.Execute(ctx, jobs) {
		if ev.Final {
			results[ev.Index] = ev.Result
			errs[ev.Index] = ev.Err
		}
	}
	return results, errs
}
