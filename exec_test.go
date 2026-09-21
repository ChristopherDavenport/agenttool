package agenttool

import (
	"context"
	"errors"
	"fmt"
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

// TestExecuteRecorderReachesTheTool covers the durable-record seam from
// the executor's side: the recorder is installed for every job, it runs
// while the tool is still in Execute, which is the case a tool killed
// mid-call has to survive, and it can name the call it writes for.
func TestExecuteRecorderReachesTheTool(t *testing.T) {
	type written struct {
		callID string
		ns     string
		data   string
	}
	var mu sync.Mutex
	var got []written
	recorded := make(chan struct{})
	proceed := make(chan struct{})

	recording := New("recording", "", func(ctx context.Context, _ NoArgs) (string, error) {
		if err := WriteRecord(ctx, handle{PGID: 4242}); err != nil {
			return "", err
		}
		<-proceed // the record is durable before the call can end
		return "done", nil
	})
	plain := NewFunc("plain", "", nil, func(ctx context.Context, c Call) (Result, error) {
		if _, ok := CallFrom(ctx); !ok {
			return Result{}, errors.New("no call on the context")
		}
		return Text("plain"), nil
	})

	exec := Executor{Recorder: func(ctx context.Context, rec *Record) error {
		call, _ := CallFrom(ctx)
		mu.Lock()
		got = append(got, written{callID: call.ID, ns: rec.NS, data: string(rec.Data)})
		mu.Unlock()
		close(recorded)
		return nil
	}}
	jobs := []Job{
		{Tool: recording, Call: Call{ID: "call_1"}},
		{Tool: plain, Call: Call{ID: "call_2"}},
	}
	done := make(chan []Result, 1)
	go func() {
		results, errs := exec.Results(context.Background(), jobs)
		for i, err := range errs {
			if err != nil {
				t.Errorf("errs[%d] = %v", i, err)
			}
		}
		done <- results
	}()
	select {
	case <-recorded:
	case <-time.After(5 * time.Second):
		t.Fatal("nothing recorded while the tool ran")
	}
	close(proceed)
	results := <-done
	if results[0].Output.Text != "done" || results[1].Output.Text != "plain" {
		t.Errorf("results = %+v", results)
	}
	want := written{callID: "call_1", ns: "shell:process-group", data: `{"pgid":4242}`}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("records = %+v, want [%+v]", got, want)
	}
}

// TestExecuteKeepsAnInstalledRecorder: an executor with no recorder of
// its own leaves the caller's on the context.
func TestExecuteKeepsAnInstalledRecorder(t *testing.T) {
	var calls atomic.Int64
	ctx := ContextWithRecorder(context.Background(), func(context.Context, *Record) error {
		calls.Add(1)
		return nil
	})
	tool := New("t", "", func(ctx context.Context, _ NoArgs) (string, error) {
		return "ok", WriteRecord(ctx, handle{PGID: 1})
	})
	if _, errs := (Executor{}).Results(ctx, []Job{{Tool: tool, Call: Call{ID: "c"}}}); errs[0] != nil {
		t.Fatalf("err = %v", errs[0])
	}
	if calls.Load() != 1 {
		t.Errorf("recorder calls = %d, want 1", calls.Load())
	}
}

// TestExecuteResourceChains is the shape a persistent shell needs: two
// calls of the shell never overlap and keep the model's order, while
// the reads beside them run together. The reads prove their
// parallelism by meeting at a barrier rather than by a clock.
func TestExecuteResourceChains(t *testing.T) {
	const reads = 3
	var arrived atomic.Int32
	barrier := make(chan struct{})
	read := New("read", "", func(ctx context.Context, _ NoArgs) (string, error) {
		if arrived.Add(1) == reads {
			close(barrier)
		}
		select {
		case <-barrier:
			return "read", nil
		case <-time.After(5 * time.Second):
			return "", errors.New("reads did not run together")
		}
	})

	var mu sync.Mutex
	var log []string
	var inShell int
	shell := func(name string) Tool {
		return New(name, "", func(ctx context.Context, _ NoArgs) (string, error) {
			mu.Lock()
			inShell++
			overlap := inShell > 1
			log = append(log, name)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inShell--
			mu.Unlock()
			if overlap {
				return "", errors.New("two calls in the shell at once")
			}
			return name, nil
		}, WithResource("shell:session"))
	}

	jobs := []Job{
		{Tool: shell("cd_a"), Call: Call{ID: "1"}},
		{Tool: read, Call: Call{ID: "2"}},
		{Tool: shell("cd_b"), Call: Call{ID: "3"}},
		{Tool: read, Call: Call{ID: "4"}},
		{Tool: read, Call: Call{ID: "5"}},
	}
	results, errs := (Executor{}).Results(context.Background(), jobs)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("errs[%d] = %v", i, err)
		}
	}
	if results[0].Output.Text != "cd_a" || results[2].Output.Text != "cd_b" {
		t.Errorf("results = %+v", results)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(log, ",") != "cd_a,cd_b" {
		t.Errorf("shell order = %v, want the model's order", log)
	}
}

// TestExecuteResourceGrouping covers which jobs share a chain, without
// running them: two tools naming one resource share it, a sequential
// tool takes the batch whatever it names, and a tool that names nothing
// is a chain of its own.
func TestExecuteResourceGrouping(t *testing.T) {
	plain := New("plain", "", func(context.Context, NoArgs) (string, error) { return "", nil })
	shell := New("shell", "", func(context.Context, NoArgs) (string, error) { return "", nil }, WithResource("shell:session"))
	restart := NewFunc("restart", "", nil, func(context.Context, Call) (Result, error) { return Result{}, nil }, WithResource("shell:session"))
	other := New("other", "", func(context.Context, NoArgs) (string, error) { return "", nil }, WithResource("container:47"))
	both := New("both", "", func(context.Context, NoArgs) (string, error) { return "", nil }, WithSequential(), WithResource("shell:session"))

	cases := []struct {
		name  string
		tools []Tool
		limit int
		want  [][]int
	}{
		{"no resources", []Tool{plain, plain, plain}, 8, [][]int{{0}, {1}, {2}}},
		{"one resource twice", []Tool{shell, plain, shell}, 8, [][]int{{0, 2}, {1}}},
		{"two tools, one resource", []Tool{shell, restart, other}, 8, [][]int{{0, 1}, {2}}},
		{"a serial batch ignores resources", []Tool{shell, plain, shell}, 1, [][]int{{0, 1, 2}}},
		{"sequential beats resource", []Tool{both, plain}, 1, [][]int{{0, 1}}},
		{"a nil tool has no resource", []Tool{nil, shell}, 8, [][]int{{0}, {1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobs := make([]Job, len(tc.tools))
			for i, tl := range tc.tools {
				jobs[i] = Job{Tool: tl}
			}
			got := chainsOf(jobs, tc.limit)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("chains = %v, want %v", got, tc.want)
			}
		})
	}
	if res := ResourceOf(both); res != "" {
		t.Errorf("ResourceOf(a sequential tool) = %q, want the empty string", res)
	}
	if res := ResourceOf(shell); res != "shell:session" {
		t.Errorf("ResourceOf = %q", res)
	}
}
