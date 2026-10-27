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
		{"accept with no content to a form that requires a field", form, Answer{Action: ActionAccept}, `"reason"`},
		{"content on decline", form, Answer{Action: ActionDecline, Content: json.RawMessage(`{"reason":"x"}`)}, "only on accept"},
		{"a decline's empty object", form, Answer{Action: ActionDecline, Content: json.RawMessage(`{}`)}, ""},
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

// TestCheckAnswerAgainstTheForm: an accepted form's content is checked
// against the restricted schema MCP's elicitation allows, flat members
// of a primitive type with their bounds, formats and choices, and the
// required ones present.
func TestCheckAnswerAgainstTheForm(t *testing.T) {
	q := Elicitation{Message: "Details?", Schema: json.RawMessage(`{"type":"object","properties":{
		"name":{"type":"string","minLength":2,"maxLength":5},
		"email":{"type":"string","format":"email"},
		"site":{"type":"string","format":"uri"},
		"day":{"type":"string","format":"date"},
		"at":{"type":"string","format":"date-time"},
		"size":{"type":"string","enum":["s","m","l"]},
		"tier":{"type":"string","oneOf":[{"const":"free","title":"Free"},{"const":"pro","title":"Pro"}]},
		"days":{"type":"integer","minimum":1,"maximum":30},
		"ratio":{"type":"number","maximum":1},
		"agree":{"type":"boolean"},
		"tags":{"type":"array","items":{"type":"string","enum":["a","b"]},"minItems":1,"maxItems":2},
		"picks":{"type":"array","items":{"anyOf":[{"const":"x"},{"const":"y"}]}}
	},"required":["name","days"]}`)}
	valid := `{"name":"Ann","email":"ann@example.com","site":"https://example.com","day":"2026-10-27","at":"2026-10-27T10:00:00Z","size":"m","tier":"pro","days":7,"ratio":0.5,"agree":true,"tags":["a"],"picks":["y"]}`
	if err := q.Check(Answer{Action: ActionAccept, Content: json.RawMessage(valid)}); err != nil {
		t.Fatalf("a valid answer: %v", err)
	}
	if err := q.Check(Answer{Action: ActionAccept, Content: json.RawMessage(`{"name":"Bo","days":1}`)}); err != nil {
		t.Errorf("the required members alone: %v", err)
	}
	for name, tc := range map[string]struct{ content, want string }{
		"a required member missing": {`{"name":"Ann"}`, `"days"`},
		"a string too short":        {`{"name":"A","days":1}`, `"name"`},
		"a string too long":         {`{"name":"Annabel","days":1}`, `"name"`},
		"a number for a string":     {`{"name":5,"days":1}`, `"name"`},
		"not an email":              {`{"name":"Ann","days":1,"email":"ann"}`, `"email"`},
		"not a URI":                 {`{"name":"Ann","days":1,"site":"example"}`, `"site"`},
		"not a date":                {`{"name":"Ann","days":1,"day":"27/10/2026"}`, `"day"`},
		"not a date-time":           {`{"name":"Ann","days":1,"at":"2026-10-27"}`, `"at"`},
		"not in the enum":           {`{"name":"Ann","days":1,"size":"xl"}`, `"size"`},
		"not a titled choice":       {`{"name":"Ann","days":1,"tier":"gold"}`, `"tier"`},
		"a fraction for an integer": {`{"name":"Ann","days":1.5}`, `"days"`},
		"below the minimum":         {`{"name":"Ann","days":0}`, `"days"`},
		"above the maximum":         {`{"name":"Ann","days":1,"ratio":2}`, `"ratio"`},
		"a string for a boolean":    {`{"name":"Ann","days":1,"agree":"yes"}`, `"agree"`},
		"an item not offered":       {`{"name":"Ann","days":1,"tags":["c"]}`, `"tags"`},
		"too many items":            {`{"name":"Ann","days":1,"tags":["a","b","a"]}`, `"tags"`},
		"too few items":             {`{"name":"Ann","days":1,"tags":[]}`, `"tags"`},
		"a titled item not offered": {`{"name":"Ann","days":1,"picks":["z"]}`, `"picks"`},
	} {
		t.Run(name, func(t *testing.T) {
			err := q.Check(Answer{Action: ActionAccept, Content: json.RawMessage(tc.content)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Check = %v, want an error naming %s", err, tc.want)
			}
		})
	}
}
