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
// Questions are asked one at a time, in the order they arrive.
func Prompt(in io.Reader, out io.Writer) agenttool.Elicitor {
	p := &prompt{in: bufio.NewReader(in), out: out}
	return p.ask
}

type prompt struct {
	mu  sync.Mutex
	in  *bufio.Reader
	out io.Writer
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
		return p.yes(true)
	case len(q.Schema) == 0:
		fmt.Fprint(p.out, "Continue? [y/N] ")
		return p.yes(false)
	}
	params, err := paramsOf(q.Schema)
	if err != nil {
		return agenttool.Answer{}, fmt.Errorf("cli: the question's form: %w", err)
	}
	form := map[string]json.RawMessage{}
	for _, f := range params {
		for {
			if ctx.Err() != nil {
				return agenttool.Answer{}, ctx.Err()
			}
			fmt.Fprint(p.out, fieldPrompt(f))
			line, ok := p.line()
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
func (p *prompt) yes(dflt bool) (agenttool.Answer, error) {
	line, ok := p.line()
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
// read.
func (p *prompt) line() (string, bool) {
	s, err := p.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || s == "") {
		return "", false
	}
	return strings.TrimSpace(s), true
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
