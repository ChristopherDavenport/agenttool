package cli_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/cli"
	"github.com/ChristopherDavenport/openresponses"
)

var update = flag.Bool("update", false, "rewrite golden files")

type readArgs struct {
	Path     string `json:"path" desc:"Absolute path to read"`
	MaxBytes int    `json:"max_bytes,omitempty" desc:"Stop after this many bytes"`
}

type item struct {
	Name string `json:"name"`
}

type tagArgs struct {
	Tags    []string          `json:"tags" desc:"Tags to set"`
	Level   string            `json:"level,omitempty" enum:"low,high"`
	Verbose bool              `json:"verbose,omitempty"`
	Ratio   float64           `json:"ratio,omitempty"`
	Labels  map[string]string `json:"labels,omitempty" desc:"Labels by key"`
	Items   []item            `json:"items,omitempty"`
}

type started struct {
	PID int `json:"pid"`
}

func (started) RecordNS() string { return "test:started" }

type finished struct {
	Code int `json:"code"`
}

func (finished) RecordNS() string { return "test:finished" }

var png = []byte("\x89PNG\r\n\x1a\nfake")

func tools() agenttool.Set {
	return agenttool.Set{
		agenttool.New("read_file", "Read a file from disk.",
			func(ctx context.Context, a readArgs) (string, error) {
				return "read " + a.Path, nil
			}, agenttool.WithAnnotations(agenttool.Annotations{ReadOnly: true})),
		agenttool.New("tag", "Set tags.\nSecond line.",
			func(ctx context.Context, a tagArgs) (tagArgs, error) { return a, nil },
			agenttool.WithAnnotations(agenttool.Annotations{Destructive: true, OpenWorld: true})),
		agenttool.New("ping", "",
			func(ctx context.Context, _ agenttool.NoArgs) (string, error) { return "pong", nil }),
		agenttool.New("fail", "Always fails.",
			func(ctx context.Context, _ agenttool.NoArgs) (string, error) { return "", errors.New("no such file") }),
		agenttool.New("boom", "Panics.",
			func(ctx context.Context, _ agenttool.NoArgs) (string, error) { panic("kaboom") }),
		agenttool.New("delete", "Ask, then delete.",
			func(ctx context.Context, _ agenttool.NoArgs) (string, error) {
				ask, ok := agenttool.ElicitorFrom(ctx)
				if !ok {
					return "", errors.New("nobody to ask")
				}
				a, err := ask(ctx, agenttool.Elicitation{Message: "Delete the branch?"})
				if err != nil {
					return "", err
				}
				if a.Action != agenttool.ActionAccept {
					return "kept", nil
				}
				b, err := ask(ctx, agenttool.Elicitation{
					Message: "Why?",
					Schema:  json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"},"days":{"type":"integer"}},"required":["reason"]}`),
				})
				if err != nil {
					return "", err
				}
				return "deleted: " + string(b.Content), nil
			}),
		agenttool.New("shot", "Take a screenshot.",
			func(ctx context.Context, _ agenttool.NoArgs) (openresponses.Contents, error) {
				return openresponses.Contents{
					&openresponses.InputText{Text: "here it is"},
					&openresponses.InputImage{ImageURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)},
					&openresponses.InputImage{ImageURL: "https://example.com/a.png"},
				}, nil
			}),
		agenttool.New("work", "Report progress and records.",
			func(ctx context.Context, _ agenttool.NoArgs) (agenttool.Result, error) {
				agenttool.Progress(ctx, agenttool.Result{Details: agenttool.ProgressInfo{Progress: 1, Total: 2, Message: "half"}})
				if err := agenttool.WriteRecord(ctx, started{PID: 42}); err != nil {
					return agenttool.Result{}, err
				}
				return agenttool.Result{Output: agenttool.Text("done").Output, Details: finished{Code: 0}}, nil
			}),
		agenttool.NewFunc("raw", "A schema from elsewhere.",
			json.RawMessage(`{"type":"object","properties":{"count":{"type":["integer","null"]},"mixed":{"type":["string","integer"]},"a=b":{"type":"string"},"-x":{"type":"string"}}}`),
			func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
				return agenttool.Text(string(call.Args)), nil
			}),
	}
}

func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	r := cli.Runner{
		Name:   "kit",
		Tools:  tools(),
		Stdin:  strings.NewReader(stdin),
		Stdout: &stdout,
		Stderr: &stderr,
	}
	code := r.Run(context.Background(), args)
	return code, stdout.String(), stderr.String()
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		stdin      string
		args       []string
		code       int
		stdout     string // exact, unless empty
		stdoutHas  string
		stderrHas  string
		stdoutJSON string // compared as JSON
	}{
		{name: "flags", args: []string{"read_file", "--path", "/etc/hosts"}, stdout: "read /etc/hosts\n"},
		{name: "single dash", args: []string{"read_file", "-path=/x"}, stdout: "read /x\n"},
		{name: "inline JSON", args: []string{"read_file", `{"path":"/y"}`}, stdout: "read /y\n"},
		{name: "stdin JSON", stdin: `{"path": "/z"}`, args: []string{"read_file", "-"}, stdout: "read /z\n"},
		{name: "no arguments", args: []string{"ping"}, stdout: "pong\n"},
		{
			name:       "types and repeats",
			args:       []string{"tag", "--tags", "a", "--tags", "b", "--level", "high", "--verbose", "--ratio", "0.5"},
			stdoutJSON: `{"tags":["a","b"],"level":"high","verbose":true,"ratio":0.5}`,
		},
		{
			name:       "flags and JSON together",
			args:       []string{"tag", "--tags", "a", `{"labels":{"k":"v"},"items":[{"name":"n"}]}`},
			stdoutJSON: `{"tags":["a"],"labels":{"k":"v"},"items":[{"name":"n"}]}`,
		},
		{name: "nullable union has a flag", args: []string{"raw", "--count", "3"}, stdout: `{"count":3}` + "\n"},
		{name: "a union of two types has none", args: []string{"raw", "--mixed", "x"}, code: cli.ExitUsage, stderrHas: "flag provided but not defined: -mixed"},
		{name: "names a flag cannot carry arrive as JSON", args: []string{"raw", `{"a=b":"1","-x":"2"}`}, stdoutJSON: `{"a=b":"1","-x":"2"}`},
		{name: "boolean false", args: []string{"tag", "--tags", "a", "--verbose=false"}, stdoutJSON: `{"tags":["a"]}`},
		{name: "a property in both", args: []string{"read_file", "--path", "/a", `{"path":"/b"}`}, code: cli.ExitUsage, stderrHas: `both give "path"`},
		{name: "bad integer", args: []string{"read_file", "--path", "/a", "--max_bytes", "ten"}, code: cli.ExitUsage, stderrHas: `"ten" is not an integer`},
		{name: "scalar given twice", args: []string{"read_file", "--path", "/a", "--path", "/b"}, code: cli.ExitUsage, stderrHas: "given more than once"},
		{name: "JSON not an object", args: []string{"read_file", `["x"]`}, code: cli.ExitUsage, stderrHas: "not a JSON object"},
		{name: "two positionals", args: []string{"read_file", "{}", "{}"}, code: cli.ExitUsage, stderrHas: "one JSON argument at most"},
		{name: "unknown command", args: []string{"nope"}, code: cli.ExitUsage, stderrHas: `unknown command "nope"`},
		{name: "no command", args: nil, code: cli.ExitUsage, stderrHas: "## Commands"},
		{name: "unknown option", args: []string{"--bogus", "ping"}, code: cli.ExitUsage, stderrHas: "-bogus"},
		{name: "missing required is the tool's", args: []string{"read_file"}, code: cli.ExitFailed, stderrHas: "Error: invalid arguments: missing required property \"path\""},
		{name: "enum is the tool's", args: []string{"tag", "--tags", "a", "--level", "mid"}, code: cli.ExitFailed, stderrHas: "Error: invalid arguments: level"},
		{name: "tool error", args: []string{"fail"}, code: cli.ExitFailed, stderrHas: "Error: no such file"},
		{name: "panic", args: []string{"boom"}, code: cli.ExitFailed, stderrHas: `Error: tool "boom" panicked: kaboom`},
		{name: "help", args: []string{"help"}, stdoutHas: "- `tag`: Set tags.\n"},
		{name: "--help", args: []string{"--help"}, stdoutHas: "## Calling `kit`"},
		{name: "help command", args: []string{"help", "read_file"}, stdoutHas: "kit read_file --path <string> [--max_bytes <integer>]"},
		{name: "command --help", args: []string{"read_file", "--help"}, stdoutHas: "### `read_file`"},
		{name: "schema", args: []string{"schema", "ping"}, stdout: "{\n  \"type\": \"object\",\n  \"properties\": {},\n  \"required\": []\n}\n"},
		{name: "schema of unknown", args: []string{"schema", "nope"}, code: cli.ExitUsage, stderrHas: `unknown command "nope"`},
		{
			name:       "unanswered question",
			args:       []string{"delete"},
			code:       cli.ExitNeedsAnswer,
			stdoutJSON: `{"message":"Delete the branch?","answers_given":0}`,
			stderrHas:  "--answer",
		},
		{
			name:       "second question unanswered",
			args:       []string{"--answer", "accept", "delete"},
			code:       cli.ExitNeedsAnswer,
			stdoutJSON: `{"message":"Why?","schema":{"type":"object","properties":{"reason":{"type":"string"},"days":{"type":"integer"}},"required":["reason"]},"answers_given":1}`,
		},
		{name: "answered", args: []string{"--answer", "accept", "--answer", `{"reason":"merged"}`, "delete"}, stdout: "deleted: {\"reason\":\"merged\"}\n"},
		{name: "declined", args: []string{"--answer", "decline", "delete"}, stdout: "kept\n"},
		{name: "bad answer", args: []string{"--answer", "maybe", "delete"}, code: cli.ExitUsage, stderrHas: "an answer is accept"},
		{name: "progress on stderr", args: []string{"work"}, stdout: "done\n", stderrHas: "[1/2] half\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := run(t, tt.stdin, tt.args...)
			if code != tt.code {
				t.Errorf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tt.code, stdout, stderr)
			}
			if tt.stdout != "" && stdout != tt.stdout {
				t.Errorf("stdout = %q, want %q", stdout, tt.stdout)
			}
			if tt.stdoutHas != "" && !strings.Contains(stdout, tt.stdoutHas) {
				t.Errorf("stdout lacks %q:\n%s", tt.stdoutHas, stdout)
			}
			if tt.stderrHas != "" && !strings.Contains(stderr, tt.stderrHas) {
				t.Errorf("stderr lacks %q:\n%s", tt.stderrHas, stderr)
			}
			if tt.stdoutJSON != "" {
				assertJSON(t, stdout, tt.stdoutJSON)
			}
		})
	}
}

func assertJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Errorf("stdout = %s, want %s", gb, wb)
	}
}

// The question a call stops on is the one it asks again when run with
// the earlier answers: the protocol a model follows through a shell.
func TestRunAnswerRoundTrip(t *testing.T) {
	code, stdout, _ := run(t, "", "delete")
	if code != cli.ExitNeedsAnswer {
		t.Fatalf("exit %d", code)
	}
	var q struct {
		Message      string `json:"message"`
		AnswersGiven int    `json:"answers_given"`
	}
	if err := json.Unmarshal([]byte(stdout), &q); err != nil {
		t.Fatal(err)
	}
	if q.AnswersGiven != 0 {
		t.Fatalf("answers_given = %d", q.AnswersGiven)
	}
	code, stdout, _ = run(t, "", "--answer", "accept", "delete")
	if code != cli.ExitNeedsAnswer || !strings.Contains(stdout, `"answers_given":1`) {
		t.Fatalf("exit %d, stdout %s", code, stdout)
	}
	code, stdout, _ = run(t, "", "--answer", "accept", "--answer", `{"reason":"done"}`, "delete")
	if code != cli.ExitOK || !strings.HasPrefix(stdout, "deleted") {
		t.Fatalf("exit %d, stdout %s", code, stdout)
	}
}

func TestRunAsk(t *testing.T) {
	var stdout, stderr, asked strings.Builder
	r := cli.Runner{
		Name:   "kit",
		Tools:  tools(),
		Ask:    cli.Prompt(strings.NewReader("y\n\nmerged\n7\n"), &asked),
		Stdout: &stdout,
		Stderr: &stderr,
	}
	code := r.Run(context.Background(), []string{"delete"})
	if code != cli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	assertJSON(t, strings.TrimPrefix(stdout.String(), "deleted: "), `{"reason":"merged","days":7}`)
	for _, want := range []string{"Delete the branch?", "Continue? [y/N]", "reason (string): An answer is required.", "days (integer, optional): "} {
		if !strings.Contains(asked.String(), want) {
			t.Errorf("prompt lacks %q:\n%s", want, asked.String())
		}
	}
}

func TestPrompt(t *testing.T) {
	form := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)
	tests := []struct {
		name   string
		in     string
		q      agenttool.Elicitation
		action agenttool.Action
		body   string
	}{
		{name: "yes", in: "yes\n", q: agenttool.Elicitation{Message: "Go?"}, action: agenttool.ActionAccept},
		{name: "default no", in: "\n", q: agenttool.Elicitation{Message: "Go?"}, action: agenttool.ActionDecline},
		{name: "end of input", in: "", q: agenttool.Elicitation{Message: "Go?"}, action: agenttool.ActionCancel},
		{name: "url default yes", in: "\n", q: agenttool.Elicitation{URL: "https://x"}, action: agenttool.ActionAccept},
		{name: "url declined", in: "n\n", q: agenttool.Elicitation{URL: "https://x"}, action: agenttool.ActionDecline},
		{name: "form retries a bad value", in: "x\n4\n", q: agenttool.Elicitation{Schema: form}, action: agenttool.ActionAccept, body: `{"n":4}`},
		{name: "form cut off", in: "x\n", q: agenttool.Elicitation{Schema: form}, action: agenttool.ActionCancel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			a, err := cli.Prompt(strings.NewReader(tt.in), &out)(context.Background(), tt.q)
			if err != nil {
				t.Fatal(err)
			}
			if a.Action != tt.action {
				t.Errorf("action %q, want %q", a.Action, tt.action)
			}
			if tt.body != "" && string(a.Content) != tt.body {
				t.Errorf("content %s, want %s", a.Content, tt.body)
			}
		})
	}
}

func TestRunFiles(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := run(t, "", "--out", dir, "shot")
	if code != cli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	sc := bufio.NewScanner(strings.NewReader(stdout))
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	want := []string{"here it is", filepath.Join(dir, "output-1.png"), "https://example.com/a.png"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("stdout = %q, want %q", lines, want)
	}
	data, err := os.ReadFile(want[1])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(png) {
		t.Errorf("file holds %q", data)
	}
}

func TestRunRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	for range 2 {
		if code, _, stderr := run(t, "", "--record", path, "work"); code != cli.ExitOK {
			t.Fatalf("exit %d: %s", code, stderr)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	type line struct {
		Tool string          `json:"tool"`
		Call string          `json:"call"`
		NS   string          `json:"ns"`
		Data json.RawMessage `json:"data"`
	}
	var got []line
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ln line
		if err := json.Unmarshal([]byte(l), &ln); err != nil {
			t.Fatalf("%v: %s", err, l)
		}
		got = append(got, ln)
	}
	if len(got) != 4 {
		t.Fatalf("%d lines, want 4:\n%s", len(got), data)
	}
	for i, want := range []struct{ ns, data string }{
		{"test:started", `{"pid":42}`}, {"test:finished", `{"code":0}`},
		{"test:started", `{"pid":42}`}, {"test:finished", `{"code":0}`},
	} {
		if got[i].Tool != "work" || got[i].NS != want.ns || string(got[i].Data) != want.data {
			t.Errorf("line %d = %+v, want %s %s", i, got[i], want.ns, want.data)
		}
	}
	if got[0].Call == "" || got[0].Call != got[1].Call {
		t.Errorf("one call's records name call %q and %q", got[0].Call, got[1].Call)
	}
	if got[0].Call == got[2].Call {
		t.Errorf("two runs share call ID %q", got[0].Call)
	}
}

func TestCommandsRefuses(t *testing.T) {
	ping := func(name string) agenttool.Tool {
		return agenttool.New(name, "", func(ctx context.Context, _ agenttool.NoArgs) (string, error) { return "", nil })
	}
	bad := agenttool.NewFunc("bad", "", json.RawMessage(`{"properties":[]}`), func(context.Context, agenttool.Call) (agenttool.Result, error) { return agenttool.Result{}, nil })
	for name, set := range map[string]agenttool.Set{
		"duplicate":      {ping("a"), ping("a")},
		"reserved":       {ping("help")},
		"bad schema":     {bad},
		"empty name":     {ping("")},
		"schema named":   {ping("schema")},
		"property twice": {agenttool.NewFunc("twice", "", json.RawMessage(`{"properties":{"a":{"type":"string"},"a":{"type":"string"}}}`), func(context.Context, agenttool.Call) (agenttool.Result, error) { return agenttool.Result{}, nil })},
	} {
		if _, err := cli.Commands(set); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestMarkdown(t *testing.T) {
	cmds, err := cli.Commands(tools())
	if err != nil {
		t.Fatal(err)
	}
	got := cli.Markdown("kit", cmds)
	path := filepath.Join("testdata", "usage.md")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("Markdown differs from %s; run go test ./cli -update and review the diff\n%s", path, got)
	}
}
