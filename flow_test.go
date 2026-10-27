package agenttool

import (
	"context"
	"reflect"
	"testing"
)

// TestFlowPresence: a tool that says nothing about its flow is told
// apart from one that declared it reads nothing and sends nothing, and
// an unknown label reads as untrusted (#92).
func TestFlowPresence(t *testing.T) {
	fn := func(context.Context, Call) (Result, error) { return Result{}, nil }
	if f, ok := FlowOf(NewFunc("plain", "", nil, fn)); ok || !reflect.DeepEqual(f, Flow{}) {
		t.Errorf("FlowOf(plain) = %+v, %v; want unstated", f, ok)
	}
	if f, ok := FlowOf(NewFunc("pure", "", nil, fn, WithFlow(Flow{}))); !ok || len(f.Reads) != 0 || f.Sends {
		t.Errorf("FlowOf(declared nothing) = %+v, %v; want stated, reading and sending nothing", f, ok)
	}
	mail := Flow{Reads: []Label{LabelPrivate}, Sends: true, To: []string{"to"}, Carries: []string{"subject", "body"}}
	if f, ok := FlowOf(New("send_email", "", func(context.Context, NoArgs) (string, error) { return "", nil }, WithFlow(mail))); !ok || !reflect.DeepEqual(f, mail) {
		t.Errorf("FlowOf(send_email) = %+v, %v; want %+v", f, ok, mail)
	}
	got, _ := FlowOf(NewFunc("odd", "", nil, fn, WithFlow(Flow{Reads: []Label{"secret", LabelPrivate, "other"}})))
	if want := []Label{LabelUntrusted, LabelPrivate}; !reflect.DeepEqual(got.Reads, want) {
		t.Errorf("unknown labels read as %v, want %v", got.Reads, want)
	}
	if !got.ReadsLabel(LabelUntrusted) || got.ReadsLabel("other") {
		t.Errorf("ReadsLabel on %+v", got)
	}
}
