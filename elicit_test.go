package agenttool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestElicitorOnTheContext(t *testing.T) {
	if _, ok := ElicitorFrom(context.Background()); ok {
		t.Error("a bare context should carry no elicitor")
	}
	var asked Elicitation
	ctx := ContextWithElicitor(context.Background(), func(ctx context.Context, q Elicitation) (Answer, error) {
		asked = q
		return Answer{Action: ActionAccept, Content: json.RawMessage(`{"ok":true}`)}, nil
	})
	fn, ok := ElicitorFrom(WithCall(ctx, Call{ID: "c"}))
	if !ok {
		t.Fatal("the elicitor should survive the call being put on the context")
	}
	ans, err := fn(ctx, Elicitation{Message: "delete the branch?"})
	if err != nil || ans.Action != ActionAccept || string(ans.Content) != `{"ok":true}` || asked.Message != "delete the branch?" {
		t.Errorf("answer = %+v, %v; asked %+v", ans, err, asked)
	}
	if _, ok := ElicitorFrom(ContextWithElicitor(ctx, nil)); ok {
		t.Error("a nil elicitor should remove the one beneath it")
	}
}

// TestCheckAnswer: an answer's content is an object of the members the
// question's form defines, on accept alone; a confirmation's answer has
// none, and a note goes with any action (#85).
func TestCheckAnswer(t *testing.T) {
	form := Elicitation{Message: "Why?", Schema: json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"},"days":{"type":"integer"}},"required":["reason"]}`)}
	confirm := Elicitation{Message: "Delete the branch?"}
	empty := Elicitation{Message: "Go on?", Schema: json.RawMessage(`{"type":"object","properties":{}}`)}
	cases := []struct {
		name    string
		q       Elicitation
		a       Answer
		wantErr string
	}{
		{"form content", form, Answer{Action: ActionAccept, Content: json.RawMessage(`{"reason":"merged"}`)}, ""},
		{"a member the form does not define", form, Answer{Action: ActionAccept, Content: json.RawMessage(`{"reason":"merged","why":"x"}`)}, `"why"`},
		{"content not an object", form, Answer{Action: ActionAccept, Content: json.RawMessage(`["merged"]`)}, "not a JSON object"},
		{"accept with no content", form, Answer{Action: ActionAccept}, ""},
		{"content on decline", form, Answer{Action: ActionDecline, Content: json.RawMessage(`{"reason":"x"}`)}, "only on accept"},
		{"a confirmation", confirm, Answer{Action: ActionAccept}, ""},
		{"a confirmation with a note", confirm, Answer{Action: ActionDecline, Note: "not before Friday"}, ""},
		{"a confirmation's empty object", confirm, Answer{Action: ActionAccept, Content: json.RawMessage(`{}`)}, ""},
		{"content for a confirmation", confirm, Answer{Action: ActionAccept, Content: json.RawMessage(`{"note":"not before Friday"}`)}, `"note"`},
		{"a form with no fields", empty, Answer{Action: ActionAccept, Content: json.RawMessage(`{"x":1}`)}, `"x"`},
		{"an unknown action", confirm, Answer{Action: "maybe"}, "unknown action"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.q.Check(tc.a)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Check = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Check = %v, want an error naming %s", err, tc.wantErr)
			}
		})
	}
}
