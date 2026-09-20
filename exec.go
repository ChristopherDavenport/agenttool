package agenttool

import (
	"context"
	"fmt"
	"iter"
	"runtime/debug"
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
		// Cancelling on the way out is what releases a progress sender
		// still blocked after the consumer has left: a remote tool may
		// report from a goroutine of its own after its call returned.
		defer cancel()

		// events is unbuffered and never closed. The consumer reads until
		// it has one final event per job, and every job sends exactly
		// one, so a final send is unconditional and always received. A
		// progress send may find the consumer gone, so it also waits on
		// the context.
		events := make(chan Event)
		final := func(ev Event) { events <- ev }
		progress := func(ev Event) {
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		}

		go func() {
			sem := make(chan struct{}, limit)
			for i, job := range jobs {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					// A job that never started still completes, with the
					// cancellation as its error, so the consumer sees one
					// final event per job.
					final(Event{Index: i, Final: true, Err: ctx.Err()})
					continue
				}
				go func(i int, job Job) {
					defer func() { <-sem }()
					res, err := run(ctx, job, func(r Result) {
						progress(Event{Index: i, Result: r})
					})
					final(Event{Index: i, Final: true, Result: res, Err: err})
				}(i, job)
			}
		}()

		stopped := false
		finished := make([]bool, len(jobs))
		for remaining := len(jobs); remaining > 0; {
			ev := <-events
			if ev.Final {
				remaining--
				finished[ev.Index] = true
			} else if finished[ev.Index] {
				// A progress update that arrives after the job's result
				// is dropped so nothing follows a completion.
				continue
			}
			if stopped {
				// The consumer left; keep receiving so every job's
				// final event is taken and running tools are waited for.
				continue
			}
			if !ev.Final && jobs[ev.Index].Call.OnUpdate != nil {
				jobs[ev.Index].Call.OnUpdate(ev.Result)
			}
			if !yield(ev) {
				stopped = true
				cancel()
			}
		}
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
