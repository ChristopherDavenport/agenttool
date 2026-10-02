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
	// Serialising one tool against itself is [Resource], which costs the
	// rest of the batch nothing.
	Sequential bool
	// Recorder, when set, is installed on every job's context with
	// [ContextWithRecorder], so a tool that calls [WriteRecord] while it
	// runs reaches the host without the host threading a context per
	// job. It is left unset by a caller that has no record to write, and
	// a recorder already on the context passed to [Executor.Execute] is
	// then used as it is.
	Recorder RecordFunc
	// OnStart, when set, is called the moment a job is handed to its
	// tool: after the job has taken a slot in the batch's bound and its
	// turn in its chain, with the [Call] on ctx, and before Execute. A
	// batch is handed to the executor all at once and a job whose turn
	// has not come has not started, so this is where a harness records
	// that a call was dispatched: a call cut off before OnStart never
	// reached its tool, and one cut off after may have.
	//
	// An error stops the job: it completes with that error and the
	// tool does not run, so a dispatch the harness could not make
	// durable is never followed by a side effect the record cannot
	// see, which is the direction that matters. A panic in it is the
	// harness's and completes the job with an error naming OnStart,
	// not a [PanicError] blamed on the tool.
	//
	// It runs on the job's goroutine, holding the job's slot in the
	// bound and its turn in its chain while it runs, so a durable write
	// there delays that job and whatever waits behind it, every later
	// job in a serial batch, and nothing else. Jobs start concurrently,
	// so it must be safe to call concurrently. It is not called for a
	// job cancelled before its turn, which completes with the
	// cancellation as its error. The job it receives is the job as the
	// tool will see it, its Call.OnUpdate the executor's own forwarder,
	// so an update sent from here reaches the consumer as progress
	// before the tool has run.
	OnStart func(ctx context.Context, job Job) error
}

// Execute runs jobs and yields their events from the caller's
// goroutine, so a consumer never sees two events at once. Progress
// updates from a tool that calls Call.OnUpdate are forwarded as
// non-final events; the executor installs its own OnUpdate and chains
// to the one on the job, if any, from the yielding goroutine. A tool
// that panics completes with an error. Breaking out of the loop cancels
// the batch and waits for running tools to return. Each job's tool
// finds its [Call] on the context with [CallFrom], and [Executor.Recorder]
// on it with [RecorderFrom].
//
// Jobs whose tools name the same [Resource] run one after the other in
// the model's order and alongside the rest of the batch; jobs that name
// none run in parallel up to MaxParallel. A serial batch, from
// [Executor.Sequential] or a [Sequential] tool, runs every job in the
// model's order and ignores resources, since it is already stricter.
// Both orderings are within the batch given here: two Execute calls,
// in one executor or two, order nothing against each other, so a tool
// whose state outlives a batch guards it itself.
func (e Executor) Execute(ctx context.Context, jobs []Job) iter.Seq[Event] {
	return func(yield func(Event) bool) {
		if len(jobs) == 0 {
			return
		}
		limit := e.limit(jobs)

		if e.Recorder != nil {
			ctx = ContextWithRecorder(ctx, e.Recorder)
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

		// One goroutine per chain: the jobs of a chain run one after the
		// other in the model's order, chains run alongside each other,
		// and the semaphore bounds the batch. A slot is held across the
		// final send, so the next job of a chain starts once the
		// consumer has taken the last one's result.
		sem := make(chan struct{}, limit)
		for _, chain := range e.Chains(jobs) {
			go func(chain []int) {
				for _, i := range chain {
					select {
					case sem <- struct{}{}:
					case <-ctx.Done():
						// A job that never started still completes, with
						// the cancellation as its error, so the consumer
						// sees one final event per job.
						final(Event{Index: i, Final: true, Err: ctx.Err()})
						continue
					}
					res, err := run(ctx, jobs[i], e.OnStart, func(r Result) {
						progress(Event{Index: i, Result: r})
					})
					final(Event{Index: i, Final: true, Result: res, Err: err})
					<-sem
				}
			}(chain)
		}

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

// Chains returns the chains [Executor.Execute] runs jobs in, each a
// list of indices into jobs in the model's order, together covering
// every job once. The jobs of one chain run one after the other, each
// handed to its tool once the one before it has completed and the
// consumer has taken its final event; chains run alongside each other,
// within MaxParallel. A serial batch, from [Executor.Sequential], a
// [Sequential] tool or a MaxParallel of one, is one chain of every job
// in order. Otherwise the jobs whose tools name one [Resource] form a
// chain, placed where its first job is, and every other job, one with a
// nil tool included, is a chain of its own. A batch of no jobs has no
// chains.
//
// It is the grouping Execute uses, exposed so that a harness which
// must know the job before a given one in its chain, to wait for that
// job to be settled before dispatching the next, reads it here rather
// than restating the rules and drifting from them (#66). It reads the
// tools' properties and runs nothing.
func (e Executor) Chains(jobs []Job) [][]int {
	if len(jobs) == 0 {
		return nil
	}
	return chainsOf(jobs, e.limit(jobs))
}

// limit is the bound Execute runs jobs under: one for a serial batch,
// and otherwise MaxParallel, or [DefaultMaxParallel] when it is unset.
func (e Executor) limit(jobs []Job) int {
	limit := e.MaxParallel
	if limit <= 0 {
		limit = DefaultMaxParallel
	}
	if e.Sequential || anySequential(jobs) {
		limit = 1
	}
	return limit
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

// run executes one job, turning a panic into a [PanicError]. onStart,
// when set, is told the job has started once its context is confirmed
// live and carries the call, and before the tool runs; its refusal is
// the job's result and the tool is not run.
func run(ctx context.Context, job Job, onStart func(context.Context, Job) error, onUpdate func(Result)) (res Result, err error) {
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
	// Every tool the executor runs finds its call on the context, not
	// only a [New] one, so a tool built by [NewFunc] and a recorder
	// installed for the batch can both name the call they are serving.
	ctx = WithCall(ctx, call)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if onStart != nil {
		if err := start(ctx, onStart, Job{Tool: job.Tool, Call: call}); err != nil {
			return Result{}, err
		}
	}
	return job.Tool.Execute(ctx, call)
}

// start calls the harness's OnStart with a recover of its own, so a
// panic there is reported as the harness's rather than as the tool's.
func start(ctx context.Context, onStart func(context.Context, Job) error, job Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("agenttool: OnStart for call %q panicked: %v", job.Call.ID, r)
		}
	}()
	return onStart(ctx, job)
}

// chainsOf groups the jobs of a batch into the sequences that must run
// one after the other: every job when the batch is serial, the jobs of
// each [Resource] together, and a chain of its own for every job that
// names no resource. Order inside a chain is the model's, which is what
// a tool that owns state is told it can rely on.
func chainsOf(jobs []Job, limit int) [][]int {
	if limit == 1 {
		all := make([]int, len(jobs))
		for i := range jobs {
			all[i] = i
		}
		return [][]int{all}
	}
	chains := make([][]int, 0, len(jobs))
	byResource := make(map[string]int) // resource -> index in chains
	for i, job := range jobs {
		res := ""
		if job.Tool != nil {
			res = ResourceOf(job.Tool)
		}
		if res == "" {
			chains = append(chains, []int{i})
			continue
		}
		if at, ok := byResource[res]; ok {
			chains[at] = append(chains[at], i)
			continue
		}
		byResource[res] = len(chains)
		chains = append(chains, []int{i})
	}
	return chains
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
