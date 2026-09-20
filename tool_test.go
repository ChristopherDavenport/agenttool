package agenttool

import (
	"encoding/json"
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
