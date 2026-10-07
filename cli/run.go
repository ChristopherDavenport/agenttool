package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agenttool"
)

// The exit statuses of [Runner.Run]. They carry the contract's error
// convention through a shell, which shows a model the status as well as
// the output.
const (
	// ExitOK is a call that succeeded; its output is on stdout.
	ExitOK = 0
	// ExitFailed is the tool's error, written to stderr as the model
	// would see it in process, "Error: <message>". Invalid arguments
	// are the tool's to report, so they end here too: a tool from
	// [agenttool.New] validates them against its schema, and one from
	// [agenttool.NewFunc] checks only what its own function checks, as
	// in process, where mcpserver would validate them for it.
	ExitFailed = 1
	// ExitUsage is a call that did not run: a command line the program
	// did not understand, such as an unknown command or option, a flag
	// value of the wrong type or a JSON argument that is not an object,
	// or a call the program could not start, such as a --record file it
	// cannot open or a set [Commands] refuses.
	ExitUsage = 2
	// ExitNeedsAnswer is a call that asked a question no --answer
	// answered and nobody was there to ask. The tool was answered
	// [agenttool.ActionCancel], as the contract says a harness with
	// nobody to ask answers, and did what it does with that; stdout
	// holds the question as JSON, with the tool's output or error. A
	// caller that has the answer runs the call again, which is safe for
	// a tool that asks before it acts and repeats what was done for one
	// that does not.
	ExitNeedsAnswer = 3
)

// Runner runs one command line against a set of tools: one call of one
// tool, or the program's own help. The zero value of each stream is the
// process's own.
//
// The command line is the program's options, then the command, then
// the command's flags, then at most one JSON argument:
//
//	program [--answer <reply>]... [--record <file>] [--out <dir>] <command> [--<param> <value>]... [<json> | -]
//
// The program's options come before the command, so that a tool's
// flags are named by its schema alone and no parameter is shadowed.
// --answer answers the questions the call asks, in order; --record
// appends every [agenttool.Record] the call writes to a file as JSON
// lines; --out is where files the output carries are written. The
// commands help and schema print a command's usage and its JSON
// Schema.
//
// Run is one call, so it closes nothing: the program that built the
// tools closes them when Run returns, with [agenttool.Set.Close].
type Runner struct {
	// Name is the program's name, as usage and errors print it.
	Name  string
	Tools agenttool.Set
	// Ask answers a question no --answer answered: [Prompt] for a
	// person at a terminal. Nil, the question is answered
	// [agenttool.ActionCancel] and the call ends with [ExitNeedsAnswer]
	// and the question on stdout, for the caller to run again with the
	// answer, which is what a model calling through a shell can do. A
	// run again is a second call, so the protocol suits a tool that asks
	// before it acts.
	// A program run by people and models alike sets it only when it
	// knows a person is there, since a terminal does not say who is
	// at it.
	Ask agenttool.Elicitor

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run runs args, the command line without the program's name, and
// returns the exit status. ctx is the call's: cancelling it, as
// signal.NotifyContext does on an interrupt, is the interrupt the tool
// receives.
func (r Runner) Run(ctx context.Context, args []string) int {
	if r.Stdin == nil {
		r.Stdin = os.Stdin
	}
	if r.Stdout == nil {
		r.Stdout = os.Stdout
	}
	if r.Stderr == nil {
		r.Stderr = os.Stderr
	}
	cmds, err := Commands(r.Tools)
	if err != nil {
		fmt.Fprintln(r.Stderr, err)
		return ExitUsage
	}

	opts := flag.NewFlagSet(r.Name, flag.ContinueOnError)
	opts.SetOutput(io.Discard)
	var answers answerList
	opts.Var(&answers, "answer", "")
	record := opts.String("record", "", "")
	out := opts.String("out", "", "")
	if err := opts.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			r.help(r.Stdout, cmds)
			return ExitOK
		}
		return r.usage(err.Error())
	}
	rest := opts.Args()
	if len(rest) == 0 {
		r.help(r.Stderr, cmds)
		return ExitUsage
	}

	switch name := rest[0]; name {
	case "help", "schema":
		return r.own(name, rest[1:], cmds)
	}
	c, ok := find(cmds, rest[0])
	if !ok {
		return r.usage(fmt.Sprintf("unknown command %q", rest[0]))
	}
	tool, _ := r.Tools.Lookup(c.Name)
	callArgs, help, err := r.parse(c, rest[1:])
	switch {
	case help:
		var b strings.Builder
		writeCommand(&b, r.Name, c)
		io.WriteString(r.Stdout, b.String())
		return ExitOK
	case err != nil:
		return r.usage(c.Name + ": " + err.Error())
	}
	return r.call(ctx, tool, callArgs, answers, *record, *out)
}

// own runs the program's own commands: help, with or without a command
// named, and schema.
func (r Runner) own(name string, args []string, cmds []Command) int {
	if name == "help" && len(args) == 0 {
		r.help(r.Stdout, cmds)
		return ExitOK
	}
	if len(args) != 1 {
		return r.usage(name + " takes one command")
	}
	c, ok := find(cmds, args[0])
	if !ok {
		return r.usage(fmt.Sprintf("unknown command %q", args[0]))
	}
	if name == "help" {
		var b strings.Builder
		writeCommand(&b, r.Name, c)
		io.WriteString(r.Stdout, b.String())
		return ExitOK
	}
	data, err := json.MarshalIndent(c.Schema, "", "  ")
	if err != nil {
		fmt.Fprintf(r.Stderr, "%s: schema of %s: %v\n", r.Name, c.Name, err)
		return ExitFailed
	}
	fmt.Fprintf(r.Stdout, "%s\n", data)
	return ExitOK
}

func (r Runner) help(w io.Writer, cmds []Command) {
	var b strings.Builder
	writeCalling(&b, r.Name)
	writeIndex(&b, cmds)
	io.WriteString(w, b.String())
}

func (r Runner) usage(msg string) int {
	fmt.Fprintf(r.Stderr, "%s: %s\nRun `%s help` for usage.\n", r.Name, msg, r.Name)
	return ExitUsage
}

func find(cmds []Command, name string) (Command, bool) {
	for _, c := range cmds {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// parse reads a command's flags and JSON argument into the arguments
// object of a call. help reports a -h or --help the tool has no
// parameter of its own for.
func (r Runner) parse(c Command, args []string) (json.RawMessage, bool, error) {
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var given []*value
	for _, p := range c.Params {
		if !p.Flag() {
			continue
		}
		v := &value{param: p}
		fs.Var(v, p.Name, "")
		given = append(given, v)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, true, nil
		}
		return nil, false, err
	}

	obj := map[string]json.RawMessage{}
	var text []byte // the JSON argument as given
	switch rest := fs.Args(); len(rest) {
	case 0:
	case 1:
		data := []byte(rest[0])
		if rest[0] == "-" {
			var err error
			if data, err = io.ReadAll(r.Stdin); err != nil {
				return nil, false, fmt.Errorf("reading the JSON argument from stdin: %w", err)
			}
		}
		text = bytes.TrimSpace(data)
		if err := checkObject(text); err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal(text, &obj); err != nil {
			return nil, false, errors.New("the JSON argument is not a JSON object")
		}
	default:
		// A single quote inside a single-quoted object ends the quoting,
		// and the shell splits the object into words. Saying so is what
		// lets the caller fix the call rather than mangle the value.
		if strings.HasPrefix(rest[0], "{") && !json.Valid([]byte(rest[0])) {
			return nil, false, fmt.Errorf("the JSON argument reached the program as %d words: a single quote inside the single quotes ended them, and the shell split the rest. Give JSON that holds a single quote on stdin in a quoted heredoc instead: %s %s - <<'EOF'", len(rest), r.Name, c.Name)
		}
		return nil, false, fmt.Errorf("one JSON argument at most, after the flags; got %q", rest)
	}

	flagged := false
	for _, v := range given {
		if len(v.raw) == 0 {
			continue
		}
		flagged = true
		name := v.param.Name
		if _, ok := obj[name]; ok {
			return nil, false, fmt.Errorf("--%s and the JSON argument both give %q", name, name)
		}
		if v.param.Repeated() {
			data, err := json.Marshal(v.raw)
			if err != nil {
				return nil, false, err
			}
			obj[name] = data
		} else {
			obj[name] = v.raw[0]
		}
	}
	if !flagged && text != nil {
		// The arguments as the caller wrote them, which is what
		// Call.Args is, rather than re-encoded through a map.
		return json.RawMessage(text), false, nil
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, false, err
	}
	return data, false, nil
}

// checkObject refuses a JSON argument that is not an object, is not
// UTF-8, or names a property twice, which a decoder would settle
// silently by keeping the last.
func checkObject(text []byte) error {
	if !utf8.Valid(text) {
		return errors.New("the JSON argument is not valid UTF-8")
	}
	if len(text) == 0 || text[0] != '{' || !json.Valid(text) {
		return errors.New("the JSON argument is not a JSON object")
	}
	seen := map[string]bool{}
	dup := ""
	members(text, func(name string, _ json.RawMessage) {
		if seen[name] && dup == "" {
			dup = name
		}
		seen[name] = true
	})
	if dup != "" {
		return fmt.Errorf("the JSON argument gives %q twice", dup)
	}
	return nil
}

// value is one parameter's flag. Each value given is converted by the
// parameter's type as it is parsed, so a wrong one is reported against
// its flag.
type value struct {
	param Param
	raw   []json.RawMessage
}

func (v *value) String() string { return "" }

func (v *value) Set(s string) error {
	if len(v.raw) > 0 && !v.param.Repeated() {
		return errors.New("given more than once")
	}
	data, err := convert(v.param.value(), s)
	if err != nil {
		return err
	}
	v.raw = append(v.raw, data)
	return nil
}

// IsBoolFlag lets a boolean flag stand alone, --verbose for
// --verbose=true, as the flag package allows; a repeated one too.
func (v *value) IsBoolFlag() bool {
	return v.param.value() == "boolean"
}

// answerList is the --answer option: each one an [agenttool.Answer],
// given as an action alone or as the JSON object a form asks for.
type answerList []agenttool.Answer

func (a *answerList) String() string { return "" }

func (a *answerList) Set(s string) error {
	switch act := agenttool.Action(s); act {
	case agenttool.ActionAccept, agenttool.ActionDecline, agenttool.ActionCancel:
		*a = append(*a, agenttool.Answer{Action: act})
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
		return errors.New("an answer is accept, decline, cancel or a JSON object")
	}
	*a = append(*a, agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(s)})
	return nil
}

// call runs one call of tool through an [agenttool.Executor], so a panic
// is an error and a record is written as it would be in process, and
// reports its result.
func (r Runner) call(ctx context.Context, tool agenttool.Tool, args json.RawMessage, answers answerList, recordTo, outDir string) int {
	id := callID()
	exec := agenttool.Executor{}
	if recordTo != "" {
		f, err := os.OpenFile(recordTo, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintf(r.Stderr, "%s: --record: %v\n", r.Name, err)
			return ExitUsage
		}
		defer f.Close()
		rec := &recorder{f: f, tool: tool.Name()}
		exec.Recorder = rec.write
		defer func() {
			if err := rec.err(); err != nil {
				fmt.Fprintf(r.Stderr, "%s: --record: %v\n", r.Name, err)
			}
		}()
	}
	ask := &elicitor{answers: answers, ask: r.Ask}
	ctx = agenttool.ContextWithElicitor(ctx, ask.elicit)

	job := agenttool.Job{Tool: tool, Call: agenttool.Call{ID: id, Args: args, OnUpdate: r.progress}}
	var res agenttool.Result
	var err error
	for ev := range exec.Execute(ctx, []agenttool.Job{job}) {
		if ev.Final {
			res, err = ev.Result, ev.Err
		}
	}

	// The record of the result, as a recorder in process files the
	// Details of a completed call. A record that cannot be written is
	// reported and does not change the status: the call has happened.
	if exec.Recorder != nil {
		if rec, rerr := agenttool.RecordOf(res.Details); rerr != nil {
			fmt.Fprintf(r.Stderr, "%s: --record: %v\n", r.Name, rerr)
		} else if rec != nil {
			// A failed write is kept by the recorder and reported once,
			// on the way out.
			_ = exec.Recorder(agenttool.WithCall(ctx, job.Call), rec)
		}
	}

	if q, given, ok := ask.unanswered(); ok {
		out := question{Message: q.Message, Schema: q.Schema, URL: q.URL, AnswersGiven: given}
		if err != nil {
			out.Error = err.Error()
		} else {
			out.Output = res.Output.String()
		}
		data, merr := json.Marshal(out)
		if merr != nil {
			fmt.Fprintf(r.Stderr, "%s: %v\n", r.Name, merr)
			return ExitFailed
		}
		fmt.Fprintf(r.Stdout, "%s\n", data)
		fmt.Fprintf(r.Stderr, "%s: the tool asked a question nobody answered, and was told it was cancelled; to answer it, run the call again with --answer before the command\n", r.Name)
		return ExitNeedsAnswer
	}
	if unused := ask.unused(); unused > 0 {
		fmt.Fprintf(r.Stderr, "%s: %d --answer not used: the call asked fewer questions\n", r.Name, unused)
	}
	if err != nil {
		fmt.Fprintf(r.Stderr, "Error: %s\n", err)
		return ExitFailed
	}
	o := &output{w: r.Stdout, dir: outDir, program: r.Name}
	if err := o.write(res.Output); err != nil {
		// The call has happened, so the status says it succeeded: a
		// failure here read as the tool's would have a model correct
		// the call and run a side effect twice.
		fmt.Fprintf(r.Stderr, "%s: the call succeeded, but writing its output failed, so do not run it again for that: %v\n", r.Name, err)
	}
	return ExitOK
}

// progress writes a progress update to stderr, so that stdout holds the
// result alone.
func (r Runner) progress(res agenttool.Result) {
	if p, ok := res.Details.(agenttool.ProgressInfo); ok {
		switch {
		case p.Total > 0:
			fmt.Fprintf(r.Stderr, "[%g/%g] %s\n", p.Progress, p.Total, p.Message)
		default:
			fmt.Fprintf(r.Stderr, "[%g] %s\n", p.Progress, p.Message)
		}
		return
	}
	if s := res.Output.String(); s != "" {
		fmt.Fprintln(r.Stderr, strings.TrimRight(s, "\n"))
	}
}

// question is what stdout holds when a call stops for an answer.
type question struct {
	Message string          `json:"message"`
	Schema  json.RawMessage `json:"schema,omitempty"`
	URL     string          `json:"url,omitempty"`
	// AnswersGiven is how many --answer the call used before it
	// asked this; they are needed again, in order, when it runs again.
	AnswersGiven int `json:"answers_given"`
	// Output and Error are how the call ended once told the question
	// was cancelled, so that nothing it returned is lost.
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// elicitor answers a call's questions from --answer in the order the
// tool asks them, then from Ask, and otherwise answers
// [agenttool.ActionCancel], as a harness with nobody to ask does, and
// keeps the first question it answered so. Two questions a tool asks
// at once take the answers in whichever order they arrive, so such a
// tool is better answered by Ask.
type elicitor struct {
	mu      sync.Mutex
	answers answerList
	next    int
	ask     agenttool.Elicitor
	pending *agenttool.Elicitation
	given   int
}

func (e *elicitor) elicit(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
	e.mu.Lock()
	if e.next < len(e.answers) {
		a := e.answers[e.next]
		e.next++
		e.mu.Unlock()
		return a, nil
	}
	ask := e.ask
	if ask == nil && e.pending == nil {
		e.pending = &q
		e.given = e.next
	}
	e.mu.Unlock()
	if ask == nil {
		return agenttool.Answer{Action: agenttool.ActionCancel}, nil
	}
	return ask(ctx, q)
}

// unused is how many --answer the call never asked for.
func (e *elicitor) unused() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.answers) - e.next
}

func (e *elicitor) unanswered() (agenttool.Elicitation, int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending == nil {
		return agenttool.Elicitation{}, 0, false
	}
	return *e.pending, e.given, true
}

// recorder appends each record of a call to a file as one line of JSON,
// synced before it returns, as [agenttool.RecordFunc] asks.
type recorder struct {
	mu    sync.Mutex
	f     *os.File
	tool  string
	first error
}

// recordLine is one line of a --record file.
type recordLine struct {
	Time time.Time       `json:"time"`
	Tool string          `json:"tool"`
	Call string          `json:"call,omitempty"`
	NS   string          `json:"ns"`
	Data json.RawMessage `json:"data"`
}

func (r *recorder) write(ctx context.Context, rec *agenttool.Record) error {
	line := recordLine{Time: time.Now().UTC(), Tool: r.tool, NS: rec.NS, Data: rec.Data}
	if call, ok := agenttool.CallFrom(ctx); ok {
		line.Call = call.ID
	}
	data, err := json.Marshal(line)
	if err != nil {
		return r.fail(err)
	}
	data = append(data, '\n')
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.f.Write(data); err != nil {
		return r.failLocked(err)
	}
	if err := r.f.Sync(); err != nil {
		return r.failLocked(err)
	}
	return nil
}

func (r *recorder) fail(err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failLocked(err)
}

func (r *recorder) failLocked(err error) error {
	if r.first == nil {
		r.first = err
	}
	return err
}

// err is the first write that failed during the call. The tool saw it
// too and chose what to do; the program says so on stderr either way.
func (r *recorder) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.first
}

// callID is a fresh call ID, unique enough that two runs' records do
// not collide in one --record file.
func callID() string {
	return "cli_" + rand.Text()
}
