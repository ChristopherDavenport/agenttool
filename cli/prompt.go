package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agenttool"
)

// Prompt returns an elicitor that asks a person at a terminal, reading
// from in and writing to out, for [Runner.Ask]. A question with a URL
// shows it and waits for Enter; a form asks for each field in turn,
// converting each answer by the field's type; any other question is
// yes or no. An end of input is [agenttool.ActionCancel], since nobody
// answered.
//
// Questions are asked one at a time, in the order they arrive. A
// question whose context ends while it waits for a line, as it does on
// an interrupt, returns the context's error without waiting.
func Prompt(in io.Reader, out io.Writer) agenttool.Elicitor {
	p := &prompt{in: bufio.NewReader(in), out: out}
	return p.ask
}

type prompt struct {
	mu  sync.Mutex
	in  *bufio.Reader
	out io.Writer
	// reading is the line being read, when an earlier question stopped
	// waiting for it; the next question takes it rather than starting a
	// second read of in.
	reading chan read
}

type read struct {
	line string
	err  error
}

func (p *prompt) ask(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if msg := strings.TrimSpace(q.Message); msg != "" {
		fmt.Fprintln(p.out, msg)
	}
	switch {
	case q.URL != "":
		fmt.Fprintf(p.out, "Open %s\nPress Enter when done, or type n to decline: ", q.URL)
		return p.yes(ctx, true)
	case len(q.Schema) == 0:
		fmt.Fprint(p.out, "Continue? [y/N] ")
		return p.yes(ctx, false)
	}
	form := map[string]json.RawMessage{}
	for _, f := range paramsOf(q.Schema) {
		for {
			fmt.Fprint(p.out, fieldPrompt(f))
			line, ok, err := p.line(ctx)
			if err != nil {
				return agenttool.Answer{}, err
			}
			if !ok {
				return agenttool.Answer{Action: agenttool.ActionCancel}, nil
			}
			if line == "" {
				if f.Required {
					fmt.Fprintln(p.out, "An answer is required.")
					continue
				}
				break
			}
			data, err := fieldValue(f.Type, line)
			if err != nil {
				fmt.Fprintln(p.out, err)
				continue
			}
			form[f.Name] = data
			break
		}
	}
	data, err := json.Marshal(form)
	if err != nil {
		return agenttool.Answer{}, err
	}
	return agenttool.Answer{Action: agenttool.ActionAccept, Content: data}, nil
}

// yes reads a yes or no. An empty line is dflt.
func (p *prompt) yes(ctx context.Context, dflt bool) (agenttool.Answer, error) {
	line, ok, err := p.line(ctx)
	if err != nil {
		return agenttool.Answer{}, err
	}
	if !ok {
		return agenttool.Answer{Action: agenttool.ActionCancel}, nil
	}
	accept := dflt
	switch strings.ToLower(line) {
	case "y", "yes":
		accept = true
	case "n", "no":
		accept = false
	}
	if accept {
		return agenttool.Answer{Action: agenttool.ActionAccept}, nil
	}
	return agenttool.Answer{Action: agenttool.ActionDecline}, nil
}

// line reads one line, trimmed; false at the end of input with nothing
// read. The read runs apart from the caller, so that the end of ctx is
// an error at once rather than after the next Enter.
func (p *prompt) line(ctx context.Context) (string, bool, error) {
	if p.reading == nil {
		ch := make(chan read, 1)
		p.reading = ch
		go func() {
			s, err := p.in.ReadString('\n')
			ch <- read{s, err}
		}()
	}
	select {
	case r := <-p.reading:
		p.reading = nil
		if r.err != nil && (!errors.Is(r.err, io.EOF) || r.line == "") {
			return "", false, nil
		}
		return strings.TrimSpace(r.line), true, nil
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

// fieldValue converts one answer by the field's type. A field of no
// single scalar type, which a flat form should not have, takes the
// line as JSON when it is JSON and as a string otherwise.
func fieldValue(typ, s string) (json.RawMessage, error) {
	if scalar(typ) {
		return convert(typ, s)
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s), nil
	}
	return json.Marshal(s)
}

func fieldPrompt(f Param) string {
	var b strings.Builder
	b.WriteString(f.Name)
	var facts []string
	if f.Type != "" {
		facts = append(facts, f.Type)
	}
	if !f.Required {
		facts = append(facts, "optional")
	}
	if len(facts) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(facts, ", "))
	}
	if d := strings.TrimSpace(f.Description); d != "" {
		b.WriteString(", " + d)
	}
	b.WriteString(": ")
	return b.String()
}
