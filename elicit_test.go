package agenttool

import (
	"context"
	"encoding/json"
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
