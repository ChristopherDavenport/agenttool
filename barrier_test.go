package agenttool

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A barrier is read as one, through Wrap too; a sequential tool that is
// also a barrier is sequential and not a barrier; a barrier's resource
// reads as none, and its raw Resource is still forwarded.
func TestBarrierReaders(t *testing.T) {
	noop := func(context.Context, Call) (Result, error) { return Result{}, nil }
	barrier := NewFunc("bash", "", nil, noop, WithBarrier(), WithResource("shell:session"))
	typed := New("bash", "", func(context.Context, NoArgs) (string, error) { return "", nil }, WithBarrier())
	both := NewFunc("both", "", nil, noop, WithBarrier(), WithSequential())
	plain := NewFunc("read", "", nil, noop)
	for _, tc := range []struct {
		name       string
		tool       Tool
		barrier    bool
		sequential bool
	}{
		{"func barrier", barrier, true, false},
		{"typed barrier", typed, true, false},
		{"wrapped barrier", Wrap(barrier, noop), true, false},
		{"barrier and sequential", both, false, true},
		{"wrapped barrier and sequential", Wrap(both, noop), false, true},
		{"plain", plain, false, false},
	} {
		if got := IsBarrier(tc.tool); got != tc.barrier {
			t.Errorf("%s: IsBarrier = %v, want %v", tc.name, got, tc.barrier)
		}
		if got := IsSequential(tc.tool); got != tc.sequential {
			t.Errorf("%s: IsSequential = %v, want %v", tc.name, got, tc.sequential)
		}
	}
	if r := ResourceOf(barrier); r != "" {
		t.Errorf("ResourceOf(barrier) = %q, want none", r)
	}
	if r := Wrap(barrier, noop).(Resource).Resource(); r != "shell:session" {
		t.Errorf("wrapped raw resource = %q, want the declared one", r)
	}
}

// jobsOf builds a batch from a pattern: r a plain tool, b a barrier, s a
// sequential tool, x a tool of resource "x", n a job with no tool.
func jobsOf(pattern string, run func(i int) Tool) []Job {
	var jobs []Job
	for i, c := range strings.Fields(pattern) {
		if run != nil {
			jobs = append(jobs, Job{Tool: run(i), Call: Call{ID: strconv.Itoa(i)}})
			continue
		}
		noop := func(context.Context, Call) (Result, error) { return Result{}, nil }
		var tool Tool
		switch c {
		case "r":
			tool = NewFunc("read", "", nil, noop)
		case "b":
			tool = NewFunc("bash", "", nil, noop, WithBarrier())
		case "s":
			tool = NewFunc("seq", "", nil, noop, WithSequential())
		case "x":
			tool = NewFunc("x", "", nil, noop, WithResource("x"))
		case "n":
		}
		jobs = append(jobs, Job{Tool: tool, Call: Call{ID: strconv.Itoa(i)}})
	}
	return jobs
}

func TestPrecedingAndChains(t *testing.T) {
	all := func(n int) [][]int {
		c := make([]int, n)
		for i := range c {
			c[i] = i
		}
		return [][]int{c}
	}
	for _, tc := range []struct {
		pattern   string
		exec      Executor
		preceding [][]int
		chains    [][]int
	}{
		// No barrier: Preceding agrees with Chains.
		{"r r r", Executor{}, [][]int{nil, nil, nil}, [][]int{{0}, {1}, {2}}},
		{"x r x", Executor{}, [][]int{nil, nil, {0}}, [][]int{{0, 2}, {1}}},
		// Barriers at the start, the middle and the end.
		{"b r r", Executor{}, [][]int{nil, {0}, {0}}, all(3)},
		{"r r b r r", Executor{}, [][]int{nil, nil, {0, 1}, {2}, {2}}, all(5)},
		{"r r b", Executor{}, [][]int{nil, nil, {0, 1}}, all(3)},
		// Two adjacent barriers: the second waits on the first alone.
		{"r b b r", Executor{}, [][]int{nil, {0}, {1}, {2}}, all(4)},
		// A resource's chain inside a segment keeps its order, and only
		// its first job waits on the barrier.
		{"b x r x", Executor{}, [][]int{nil, {0}, {0}, {1}}, all(4)},
		{"x x b x", Executor{}, [][]int{nil, {0}, {0, 1}, {2}}, all(4)},
		// A job with no tool is an ordinary job of its segment.
		{"n b n", Executor{}, [][]int{nil, {0}, {1}}, all(3)},
		// Sequential wins: one job at a time, whatever the barriers.
		{"r b s r", Executor{}, [][]int{nil, {0}, {1}, {2}}, all(4)},
		{"r b r", Executor{Sequential: true}, [][]int{nil, {0}, {1}}, all(3)},
		{"r b r", Executor{MaxParallel: 1}, [][]int{nil, {0}, {1}}, all(3)},
	} {
		jobs := jobsOf(tc.pattern, nil)
		if got := tc.exec.Preceding(jobs); !reflect.DeepEqual(got, tc.preceding) {
			t.Errorf("%q %+v: Preceding = %v, want %v", tc.pattern, tc.exec, got, tc.preceding)
		}
		if got := tc.exec.Chains(jobs); !reflect.DeepEqual(got, tc.chains) {
			t.Errorf("%q %+v: Chains = %v, want %v", tc.pattern, tc.exec, got, tc.chains)
		}
	}
	if got := (Executor{}).Preceding(nil); got != nil {
		t.Errorf("Preceding of no jobs = %v, want nil", got)
	}
}

// span is when a job ran, by the batch's own clock.
type span struct{ start, end int }

// clocked records each job's start and end as ticks of one counter, so
// "started after it ended" is an exact comparison, and how many jobs ran
// at once.
type clocked struct {
	mu      sync.Mutex
	tick    int
	spans   map[int]*span
	running int
	peak    int
}

func (c *clocked) tool(i int, opts ...Option) Tool {
	return NewFunc("job", "", nil, func(ctx context.Context, _ Call) (Result, error) {
		c.mu.Lock()
		c.tick++
		c.spans[i] = &span{start: c.tick}
		c.running++
		c.peak = max(c.peak, c.running)
		c.mu.Unlock()
		time.Sleep(15 * time.Millisecond)
		c.mu.Lock()
		c.tick++
		c.spans[i].end = c.tick
		c.running--
		c.mu.Unlock()
		return Result{}, nil
	}, opts...)
}

// Execute orders a batch with barriers as Preceding says, and runs the
// jobs between barriers together, within MaxParallel.
func TestExecuteBarrierSegments(t *testing.T) {
	for _, tc := range []struct {
		pattern  string
		exec     Executor
		wantPeak int // the most jobs at once; 0 means "more than one"
	}{
		{"r r b r r", Executor{}, 0},
		{"b r r r", Executor{}, 0},
		{"r r r b", Executor{}, 0},
		{"r b b r", Executor{}, 1},
		{"r r r r b r r r r", Executor{MaxParallel: 2}, 2},
		{"x x b r x", Executor{}, 0},
		{"r b s r", Executor{}, 1},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			c := &clocked{spans: map[int]*span{}}
			fields := strings.Fields(tc.pattern)
			jobs := jobsOf(tc.pattern, func(i int) Tool {
				switch fields[i] {
				case "b":
					return c.tool(i, WithBarrier())
				case "s":
					return c.tool(i, WithSequential())
				case "x":
					return c.tool(i, WithResource("x"))
				}
				return c.tool(i)
			})
			for ev := range tc.exec.Execute(context.Background(), jobs) {
				if ev.Final && ev.Err != nil {
					t.Fatalf("job %d: %v", ev.Index, ev.Err)
				}
			}
			for j, pre := range tc.exec.Preceding(jobs) {
				for _, i := range pre {
					if c.spans[j].start < c.spans[i].end {
						t.Errorf("job %d started at %d, before job %d it waits on ended at %d", j, c.spans[j].start, i, c.spans[i].end)
					}
				}
			}
			// Every barrier ran alone: nothing else overlapped it.
			for i, f := range fields {
				if f != "b" || strings.Contains(tc.pattern, "s") {
					continue
				}
				for j := range fields {
					if j != i && c.spans[j].start < c.spans[i].end && c.spans[i].start < c.spans[j].end {
						t.Errorf("job %d overlapped barrier %d", j, i)
					}
				}
			}
			switch {
			case tc.wantPeak == 0 && c.peak < 2:
				t.Errorf("peak = %d: the jobs between barriers did not run together", c.peak)
			case tc.wantPeak > 0 && c.peak > tc.wantPeak:
				t.Errorf("peak = %d, want at most %d", c.peak, tc.wantPeak)
			}
		})
	}
}

// A batch with a barrier cancelled while its first segment runs: the
// running job ends with the cancellation, and every job of a segment
// not yet started completes with it too, in the model's order, without
// reaching its tool.
func TestExecuteBarrierCancelledOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	var reached sync.Map
	blocking := NewFunc("block", "", nil, func(ctx context.Context, _ Call) (Result, error) {
		close(started)
		<-ctx.Done()
		return Result{}, ctx.Err()
	})
	mark := func(i int, opts ...Option) Tool {
		return NewFunc("mark", "", nil, func(context.Context, Call) (Result, error) {
			reached.Store(i, true)
			return Result{}, nil
		}, opts...)
	}
	jobs := []Job{{Tool: blocking}, {Tool: mark(1, WithBarrier())}, {Tool: mark(2)}, {Tool: mark(3, WithBarrier())}}
	go func() {
		<-started
		cancel()
	}()
	var order []int
	for ev := range (Executor{}).Execute(ctx, jobs) {
		if !ev.Final {
			continue
		}
		order = append(order, ev.Index)
		if !errors.Is(ev.Err, context.Canceled) {
			t.Errorf("job %d: err = %v, want the cancellation", ev.Index, ev.Err)
		}
	}
	if !reflect.DeepEqual(order, []int{0, 1, 2, 3}) {
		t.Errorf("completion order = %v, want the model's", order)
	}
	reached.Range(func(k, _ any) bool {
		t.Errorf("job %v reached its tool after the batch was cancelled", k)
		return true
	})
}

// OnStart, a harness's dispatch, sees a barrier batch in Execute's
// order: when a job is handed over, every job Preceding lists for it has
// returned from its tool.
func TestExecuteBarrierOnStartFollowsPreceding(t *testing.T) {
	var mu sync.Mutex
	returned := map[int]bool{}
	fields := strings.Fields("r r b r x x")
	jobs := jobsOf("r r b r x x", func(i int) Tool {
		var opts []Option
		switch fields[i] {
		case "b":
			opts = append(opts, WithBarrier())
		case "x":
			opts = append(opts, WithResource("x"))
		}
		return NewFunc("job", "", nil, func(context.Context, Call) (Result, error) {
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			returned[i] = true
			mu.Unlock()
			return Result{}, nil
		}, opts...)
	})
	e := Executor{}
	pre := e.Preceding(jobs)
	e.OnStart = func(ctx context.Context, job Job) error {
		i, err := strconv.Atoi(job.Call.ID)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, p := range pre[i] {
			if !returned[p] {
				t.Errorf("job %d handed over before job %d it waits on returned", i, p)
			}
		}
		return nil
	}
	for range e.Execute(context.Background(), jobs) {
	}
}
