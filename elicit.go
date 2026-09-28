package agenttool

import (
	"context"
	"encoding/json"
)

// Elicitation is a question a tool asks the user in the middle of a
// call, and needs answered before the call can go on: "delete the
// branch?", or a form for the details a request left out. It reaches
// the harness through the [Elicitor] on the call's context, so the
// harness can show it, apply its policy to it and record it under the
// call that asked, rather than meeting it as a callback inside a tool
// that is running.
type Elicitation struct {
	// Message is the question, for the user.
	Message string
	// Schema is the JSON Schema of the answer a form asks for, an
	// object of flat properties; nil when the question is not a form.
	Schema json.RawMessage
	// URL is the page the user is sent to when the question is answered
	// out of band, which is how a tool asks for a credential without
	// seeing it; empty for a form.
	URL string
}

// Action is what the user did with an [Elicitation].
type Action string

const (
	// ActionAccept is an answer: the user submitted the form, or
	// confirmed, and [Answer.Content] holds what they gave.
	ActionAccept Action = "accept"
	// ActionDecline is an explicit no.
	ActionDecline Action = "decline"
	// ActionCancel is no answer: the user dismissed the question, or
	// nobody was asked. A harness that cannot put a question to anyone
	// answers it, rather than [ActionDecline], which claims a choice.
	ActionCancel Action = "cancel"
)

// Answer is the user's reply to an [Elicitation].
type Answer struct {
	Action Action
	// Content is the submitted form, a JSON object matching the
	// question's Schema, when the action is [ActionAccept]; nil
	// otherwise.
	Content json.RawMessage
}

// Elicitor answers a question a tool asks the user mid-call. A harness
// installs one with [ContextWithElicitor] around a call, as it does a
// recorder, and a tool that needs an answer calls it; mcpclient calls
// it for a server's elicitation. ctx is the call's, with the call on it
// through [CallFrom], so the harness can file the question and the
// answer under the call that asked and say who answered.
//
// An error is the harness failing to ask, not the user saying no, and
// the tool sees it as an error; a refusal is an [Answer] with
// [ActionDecline] or [ActionCancel].
type Elicitor func(ctx context.Context, q Elicitation) (Answer, error)

type elicitorKey struct{}

// ContextWithElicitor returns ctx carrying fn as the elicitor a tool
// asks through. A nil fn removes any elicitor already on the context,
// so a tool under it asks nobody.
func ContextWithElicitor(ctx context.Context, fn Elicitor) context.Context {
	return context.WithValue(ctx, elicitorKey{}, fn)
}

// ElicitorFrom returns the elicitor on ctx, if any. A tool without one
// has nobody to ask, and treats the question as [ActionCancel].
func ElicitorFrom(ctx context.Context) (Elicitor, bool) {
	fn, ok := ctx.Value(elicitorKey{}).(Elicitor)
	return fn, ok && fn != nil
}
