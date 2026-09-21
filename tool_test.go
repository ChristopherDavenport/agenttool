package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// recordInfo is a plain tagged struct: the common case, recorded as
// json.Marshal renders it.
type recordInfo struct {
	Container string `json:"container"`
}

func (recordInfo) RecordNS() string { return "shell:container" }

// recordShaped shapes its own JSON through MarshalJSON.
type recordShaped struct{ n int }

func (recordShaped) RecordNS() string { return "shaped" }

func (r recordShaped) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]int{"n": r.n})
}

// recordPtr implements Recordable on the pointer only.
type recordPtr struct {
	N int `json:"n"`
}

func (*recordPtr) RecordNS() string { return "ptr" }

type recordEmptyNS struct{}

func (recordEmptyNS) RecordNS() string { return "" }

type recordUnmarshalable struct {
	C chan int `json:"c"`
}

func (recordUnmarshalable) RecordNS() string { return "bad" }

func TestRecordOf(t *testing.T) {
	cases := []struct {
		name    string
		details any
		want    *Record
		wantErr string
	}{
		{name: "nil", details: nil},
		{name: "progress is in-process only", details: ProgressInfo{Progress: 1, Total: 2}},
		{name: "plain value", details: "text"},
		{name: "pointer-receiver type held by value", details: recordPtr{N: 1}},
		{
			name:    "tagged struct",
			details: recordInfo{Container: "c1"},
			want:    &Record{NS: "shell:container", Data: json.RawMessage(`{"container":"c1"}`)},
		},
		{
			name:    "pointer to tagged struct",
			details: &recordInfo{Container: "c2"},
			want:    &Record{NS: "shell:container", Data: json.RawMessage(`{"container":"c2"}`)},
		},
		{
			name:    "custom MarshalJSON",
			details: recordShaped{n: 3},
			want:    &Record{NS: "shaped", Data: json.RawMessage(`{"n":3}`)},
		},
		{
			name:    "pointer receiver",
			details: &recordPtr{N: 4},
			want:    &Record{NS: "ptr", Data: json.RawMessage(`{"n":4}`)},
		},
		{name: "empty namespace", details: recordEmptyNS{}, wantErr: "record agenttool.recordEmptyNS: empty namespace"},
		{name: "unmarshalable", details: recordUnmarshalable{}, wantErr: "record bad: json: unsupported type: chan int"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RecordOf(tc.details)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if got != nil {
					t.Errorf("record = %+v, want nil on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("record = %+v, want %+v", got, tc.want)
			}
			if got == nil {
				return
			}
			if got.NS != tc.want.NS || string(got.Data) != string(tc.want.Data) {
				t.Errorf("record = {%s %s}, want {%s %s}", got.NS, got.Data, tc.want.NS, tc.want.Data)
			}
		})
	}
}

// handle is what a tool that owns a process writes as soon as it has
// one: the case a tool killed mid-call must leave behind.
type handle struct {
	PGID int `json:"pgid"`
}

func (handle) RecordNS() string { return "shell:process-group" }

func TestWriteRecord(t *testing.T) {
	cases := []struct {
		name    string
		install bool
		recErr  error // what the recorder answers
		details Recordable
		want    []Record
		wantErr string
	}{
		{
			name:    "no recorder is a no-op",
			details: handle{PGID: 7},
		},
		{
			name:    "recorded before the call ends",
			install: true,
			details: handle{PGID: 7},
			want:    []Record{{NS: "shell:process-group", Data: json.RawMessage(`{"pgid":7}`)}},
		},
		{
			name:    "nil details",
			install: true,
			details: nil,
		},
		{
			name:    "an unmarshalable value never reaches the recorder",
			install: true,
			details: recordUnmarshalable{},
			wantErr: "record bad:",
		},
		{
			name:    "the recorder's error reaches the tool",
			install: true,
			recErr:  errRecorder,
			details: handle{PGID: 7},
			want:    []Record{{NS: "shell:process-group", Data: json.RawMessage(`{"pgid":7}`)}},
			wantErr: "recorder broke",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []Record
			ctx := context.Background()
			if tc.install {
				ctx = ContextWithRecorder(ctx, func(_ context.Context, rec *Record) error {
					got = append(got, *rec)
					return tc.recErr
				})
			}
			if _, ok := RecorderFrom(ctx); ok != tc.install {
				t.Errorf("RecorderFrom = %v, want %v", ok, tc.install)
			}
			err := WriteRecord(ctx, tc.details)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("records = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i].NS != tc.want[i].NS || string(got[i].Data) != string(tc.want[i].Data) {
					t.Errorf("record[%d] = {%s %s}, want {%s %s}", i, got[i].NS, got[i].Data, tc.want[i].NS, tc.want[i].Data)
				}
			}
		})
	}
}

var errRecorder = errors.New("recorder broke")

// TestNilRecorderRemoves covers a harness that clears the recorder for a
// nested call it does not want recorded.
func TestNilRecorderRemoves(t *testing.T) {
	ctx := ContextWithRecorder(context.Background(), func(context.Context, *Record) error { return nil })
	ctx = ContextWithRecorder(ctx, nil)
	if _, ok := RecorderFrom(ctx); ok {
		t.Error("a nil recorder is still installed")
	}
	if err := WriteRecord(ctx, handle{PGID: 1}); err != nil {
		t.Errorf("WriteRecord with a nil recorder = %v", err)
	}
}

func TestAnnotations(t *testing.T) {
	read := Annotations{Title: "Read a file", ReadOnly: true}
	del := Annotations{Title: "Delete a repository", Destructive: true, OpenWorld: true}
	cases := []struct {
		name string
		tool Tool
		want Annotations
	}{
		{
			name: "a tool that says nothing",
			tool: New("plain", "", func(context.Context, NoArgs) (string, error) { return "", nil }),
		},
		{
			name: "a typed tool",
			tool: New("read", "", func(context.Context, NoArgs) (string, error) { return "", nil }, WithAnnotations(read)),
			want: read,
		},
		{
			name: "a raw tool",
			tool: NewFunc("delete", "", nil, func(context.Context, Call) (Result, error) { return Result{}, nil }, WithAnnotations(del)),
			want: del,
		},
		{
			name: "a tool from elsewhere",
			tool: bareTool{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AnnotationsOf(tc.tool); got != tc.want {
				t.Errorf("annotations = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// bareTool implements the contract and none of the optional interfaces.
type bareTool struct{}

func (bareTool) Name() string                { return "bare" }
func (bareTool) Description() string         { return "" }
func (bareTool) Parameters() json.RawMessage { return nil }
func (bareTool) Execute(context.Context, Call) (Result, error) {
	return Result{}, nil
}
