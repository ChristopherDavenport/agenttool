package agenttool

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sleeper(name string, d time.Duration, opts ...Option) Tool {
	return New(name, "", func(ctx context.Context, _ NoArgs) (string, error) {
		select {
		case <-time.After(d):
			return name, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}, opts...)
}

func TestExecuteParallelCompletionOrder(t *testing.T) {
	jobs := []Job{
		{Tool: sleeper("slow", 60*time.Millisecond), Call: Call{ID: "1"}},
		{Tool: sleeper("fast", 1*time.Millisecond), Call: Call{ID: "2"}},
	}
	var order []int
	for ev := range (Executor{}).Execute(context.Background(), jobs) {
		if ev.Final {
			order = append(order, ev.Index)
		}
	}
	if len(order) != 2 || order[0] != 1 {
		t.Errorf("completion order = %v, want fast first", order)
	}
	results, errs := (Executor{}).Results(context.Background(), jobs)
	if results[0].Output.Text != "slow" || results[1].Output.Text != "fast" || errs[0] != nil || errs[1] != nil {
		t.Errorf("results = %+v errs = %v", results, errs)
	}
}

func TestExecuteBoundedAndSequential(t *testing.T) {
	var running, peak atomic.Int32
	track := func(name string, opts ...Option) Tool {
		return New(name, "", func(ctx context.Context, _ NoArgs) (string, error) {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			running.Add(-1)
			return name, nil
		}, opts...)
	}
	cases := []struct {
		name     string
		exec     Executor
		jobs     []Job
		wantPeak int32
	}{
		{"default limit", Executor{}, []Job{{Tool: track("a")}, {Tool: track("b")}, {Tool: track("c")}}, 3},
		{"limit two", Executor{MaxParallel: 2}, []Job{{Tool: track("a")}, {Tool: track("b")}, {Tool: track("c")}, {Tool: track("d")}}, 2},
		{"executor sequential", Executor{Sequential: true}, []Job{{Tool: track("a")}, {Tool: track("b")}, {Tool: track("c")}}, 1},
		{"one sequential tool", Executor{}, []Job{{Tool: track("a")}, {Tool: track("b", WithSequential())}, {Tool: track("c")}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			peak.Store(0)
			var order []int
			for ev := range tc.exec.Execute(context.Background(), tc.jobs) {
				if ev.Final {
					order = append(order, ev.Index)
				}
			}
			if tc.wantPeak == 1 && !sort.IntsAreSorted(order) {
				t.Errorf("sequential batch completed out of order: %v", order)
			}
			if tc.wantPeak == 1 && peak.Load() != 1 {
				t.Errorf("peak = %d, want 1", peak.Load())
			}
			if tc.wantPeak > 1 && peak.Load() > tc.wantPeak {
				t.Errorf("peak = %d, want at most %d", peak.Load(), tc.wantPeak)
			}
		})
	}
}

func TestExecuteProgressAndPanic(t *testing.T) {
	progress := New("progress", "", func(ctx context.Context, _ NoArgs) (string, error) {
		Progress(ctx, Text("1"))
		Progress(ctx, Text("2"))
		return "done", nil
	})
	panics := New("panics", "", func(context.Context, NoArgs) (string, error) { panic("oh no") })
	var mu sync.Mutex
	var chained []string
	jobs := []Job{
		{Tool: progress, Call: Call{ID: "p", OnUpdate: func(r Result) {
			mu.Lock()
			chained = append(chained, r.Output.Text)
			mu.Unlock()
		}}},
		{Tool: panics, Call: Call{ID: "x"}},
		{Tool: nil, Call: Call{ID: "missing"}},
	}
	var updates []string
	finals := map[int]Event{}
	for ev := range (Executor{Sequential: true}).Execute(context.Background(), jobs) {
		if ev.Final {
			finals[ev.Index] = ev
		} else {
			updates = append(updates, ev.Result.Output.Text)
		}
	}
	if strings.Join(updates, ",") != "1,2" || strings.Join(chained, ",") != "1,2" {
		t.Errorf("updates = %v chained = %v", updates, chained)
	}
	if finals[0].Result.Output.Text != "done" {
		t.Errorf("progress result = %+v", finals[0])
	}
	var pe *PanicError
	if !errors.As(finals[1].Err, &pe) {
		t.Fatalf("panic err = %T %v, want *PanicError", finals[1].Err, finals[1].Err)
	}
	if pe.Tool != "panics" || pe.Value != "oh no" || len(pe.Stack) == 0 {
		t.Errorf("panic error = %+v", pe)
	}
	// The model sees one line; the stack stays behind the type.
	if msg := finals[1].Err.Error(); msg != `tool "panics" panicked: oh no` || strings.Contains(msg, "goroutine") {
		t.Errorf("panic message = %q", msg)
	}
	if finals[2].Err == nil || !strings.Contains(finals[2].Err.Error(), "no tool") {
		t.Errorf("missing tool err = %v", finals[2].Err)
	}
}

func TestExecuteBreakCancels(t *testing.T) {
	jobs := []Job{
		{Tool: sleeper("a", time.Millisecond)},
		{Tool: sleeper("b", time.Second)},
		{Tool: sleeper("c", time.Second)},
	}
	start := time.Now()
	for ev := range (Executor{}).Execute(context.Background(), jobs) {
		if ev.Final {
			break
		}
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("break did not cancel running tools")
	}
}

func TestExecuteContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, errs := (Executor{Sequential: true}).Results(ctx, []Job{{Tool: sleeper("a", time.Second)}, {Tool: sleeper("b", time.Second)}})
	for i, err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("errs[%d] = %v", i, err)
		}
	}
	if results, _ := (Executor{}).Results(context.Background(), nil); len(results) != 0 {
		t.Error("empty batch")
	}
}

func TestExecuteLateProgressIsDropped(t *testing.T) {
	// A tool that keeps reporting progress from its own goroutine after
	// it returned, as a remote tool's asynchronous notifications can.
	release := make(chan struct{})
	late := NewFunc("late", "", nil, func(ctx context.Context, c Call) (Result, error) {
		go func() {
			<-release
			for i := 0; i < 3; i++ {
				c.Update(Text("late"))
			}
		}()
		return Text("done"), nil
	})
	var updates int
	for ev := range (Executor{}).Execute(context.Background(), []Job{{Tool: late, Call: Call{ID: "1"}}}) {
		if !ev.Final {
			updates++
		}
	}
	close(release)
	time.Sleep(20 * time.Millisecond)
	if updates != 0 {
		t.Errorf("updates = %d", updates)
	}
}
