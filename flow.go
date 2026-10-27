package agenttool

import "slices"

// Label names a kind of content a tool's output holds; see [Flow].
type Label string

const (
	// LabelUntrusted is content an outside party wrote, a web page, an
	// inbox, an issue: text that may carry instructions the user never
	// gave.
	LabelUntrusted Label = "untrusted"
	// LabelPrivate is content the user would not have leave the
	// session: an inbox, a vault, a private repository.
	LabelPrivate Label = "private"
)

// Flow says how data moves through a call of a tool, which is what a
// policy that defends against prompt injection decides on: whether the
// output brings in text an outside party wrote, whether it holds what
// must not leave, and whether a call carries data out, and through which
// arguments.
//
// It is the author's statement, as [Annotations] are. A declared label
// or Sends may make a policy stricter; a declaration that a tool reads
// nothing untrusted or private, sends nothing, or sends only through To
// and Carries loosens a policy, and is taken only from a tool the host
// trusts. mcpclient reads a server's open-world hint as reading
// untrusted content and sending, and takes a server's own flow as stated
// only from a server whose claims the host takes.
//
// It is per tool: a tool whose calls differ declares what its widest
// call does.
type Flow struct {
	// Reads labels the tool's output. Empty says the output holds
	// neither untrusted nor private content.
	Reads []Label
	// Sends says a call carries data out of the session: it mails, posts,
	// pushes or fetches a URL it was given.
	Sends bool
	// To names the arguments, by their top-level member names, that say
	// where a call sends data; empty is every argument.
	To []string
	// Carries names the arguments whose values a call sends; empty is
	// every argument.
	Carries []string
}

// ReadsLabel reports whether f's output holds content of label l.
func (f Flow) ReadsLabel(l Label) bool { return slices.Contains(f.Reads, l) }

// Flowing is implemented by a tool that states its [Flow]. The boolean
// is presence: false is a tool that says nothing, which a reader must
// tell apart from one that declared it reads and sends nothing.
type Flowing interface {
	Flow() (Flow, bool)
}

// FlowOf returns t's flow and whether t states one. A tool that does
// not report false with the zero Flow, which says nothing rather than
// that the tool is harmless. A label this package does not know reads
// as [LabelUntrusted], so a label added later is the strict mistake;
// the labels are returned without repeats, in the order first given.
func FlowOf(t Tool) (Flow, bool) {
	f, ok := t.(Flowing)
	if !ok {
		return Flow{}, false
	}
	flow, ok := f.Flow()
	if !ok {
		return Flow{}, false
	}
	var reads []Label
	for _, l := range flow.Reads {
		if l != LabelUntrusted && l != LabelPrivate {
			l = LabelUntrusted
		}
		if !slices.Contains(reads, l) {
			reads = append(reads, l)
		}
	}
	flow.Reads = reads
	return flow, true
}

// WithFlow has the tool state f as its flow; see [Flow]. A tool built
// without it says nothing, and [FlowOf] reports false for it.
func WithFlow(f Flow) Option {
	return func(o *options) { o.flow = &f }
}

// flowBy answers for a tool built with an optional flow.
func flowBy(f *Flow) (Flow, bool) {
	if f == nil {
		return Flow{}, false
	}
	return *f, true
}
