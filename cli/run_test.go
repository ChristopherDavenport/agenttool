package cli_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
				if b.Action != agenttool.ActionAccept {
					return "", errors.New("not deleted: no reason given")
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
		// Legal JSON Schema the reader does not expect: a property that
		// is a boolean schema and an array of tuple items. Neither may
		// cost another property its flag, or another command its run.
		agenttool.NewFunc("odd", "Odd schemas.",
			json.RawMessage(`{"properties":{"x":true,"t":{"type":"array","items":[{"type":"string"}]},"n":{"type":"integer"},"bs":{"type":"array","items":{"type":"boolean"}}}}`),
			func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
				return agenttool.Text(string(call.Args)), nil
			}),
		agenttool.New("steps", "Report progress two other ways.",
			func(ctx context.Context, _ agenttool.NoArgs) (string, error) {
				agenttool.Progress(ctx, agenttool.Result{Details: agenttool.ProgressInfo{Progress: 3, Message: "counting"}})
				agenttool.Progress(ctx, agenttool.Text("halfway"))
				return "done", nil
			}),
		agenttool.New("helpful", "A parameter named help.",
			func(ctx context.Context, a struct {
				Help string `json:"help"`
			}) (string, error) {
				return "help=" + a.Help, nil
			}),
		agenttool.New("files", "Return files.",
			func(ctx context.Context, _ agenttool.NoArgs) (openresponses.Contents, error) {
				return openresponses.Contents{
					&openresponses.InputFile{FileURL: "https://example.com/f.pdf"},
					&openresponses.InputFile{FileID: "f_1"},
					&openresponses.InputFile{FileData: "not base64!"},
				}, nil
			}),
		// Objects whose fields have flags, maps, and a field whose name
		// holds the separator.
		agenttool.NewFunc("deploy", "Deploy somewhere.",
			json.RawMessage(`{"type":"object","properties":{`+
				`"target":{"type":"object","description":"Where to deploy","properties":{"host":{"type":"string","description":"Host name"},"port":{"type":"integer"},"tls":{"type":"object","properties":{"on":{"type":"boolean"}}},"a.b":{"type":"string"},"hops":{"type":"array","items":{"type":"object"}}},"required":["host"]},`+
				`"backup":{"type":["object","null"],"properties":{"host":{"type":"string"}},"required":["host"]},`+
				`"limits":{"type":"object","additionalProperties":{"type":"integer"}},`+
				`"switches":{"type":"object","additionalProperties":{"type":"boolean"}},`+
				`"extra":{"type":"object","additionalProperties":{"type":"object"}}},`+
				`"required":["target"]}`),
			func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
				return agenttool.Text(string(call.Args)), nil
			}),
		agenttool.NewFunc("nullschema", "Parameters of null.", json.RawMessage("null"),
			func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
				return agenttool.Text("ok"), nil
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
		{name: "JSON passed through as written", args: []string{"raw", `{"mixed": "<b>&", "count": 1}`}, stdout: `{"mixed": "<b>&", "count": 1}` + "\n"},
		{name: "a key twice in the JSON", args: []string{"raw", `{"mixed":"a","mixed":"b"}`}, code: cli.ExitUsage, stderrHas: `gives "mixed" twice`},
		{name: "JSON not UTF-8", args: []string{"raw", "{\"mixed\":\"\xff\"}"}, code: cli.ExitUsage, stderrHas: "not valid UTF-8"},
		{name: "string flag not UTF-8", args: []string{"read_file", "--path", "\xff"}, code: cli.ExitUsage, stderrHas: "not valid UTF-8"},
		{name: "a number keeps its digits", args: []string{"tag", "--tags", "a", "--ratio", "12345678901234567890"}, stdoutHas: "12345678901234567000"},
		{name: "an integer in exponent form", args: []string{"odd", "--n", "1e3"}, stdout: `{"n":1e3}` + "\n"},
		{name: "an integer past 64 bits", args: []string{"odd", "--n", "123456789012345678901234567890"}, stdout: `{"n":123456789012345678901234567890}` + "\n"},
		{name: "a fraction is not an integer", args: []string{"odd", "--n", "1.5"}, code: cli.ExitUsage, stderrHas: "not an integer"},
		{name: "odd schemas keep their other flags", args: []string{"odd", "--n", "+5", `{"x":1,"t":["a"]}`}, stdoutJSON: `{"n":5,"x":1,"t":["a"]}`},
		{name: "integer written as JSON", args: []string{"odd", "--n", "007"}, stdout: `{"n":7}` + "\n"},
		{name: "repeated boolean stands alone", args: []string{"odd", "--bs", "--bs=false"}, stdout: `{"bs":[true,false]}` + "\n"},
		{name: "a null schema is no arguments", args: []string{"schema", "nullschema"}, stdout: "{\n  \"type\": \"object\",\n  \"properties\": {},\n  \"required\": []\n}\n"},
		{name: "a property in both", args: []string{"read_file", "--path", "/a", `{"path":"/b"}`}, code: cli.ExitUsage, stderrHas: `both give "path"`},
		{name: "bad integer", args: []string{"read_file", "--path", "/a", "--max_bytes", "ten"}, code: cli.ExitUsage, stderrHas: `"ten" is not an integer`},
		{name: "scalar given twice", args: []string{"read_file", "--path", "/a", "--path", "/b"}, code: cli.ExitUsage, stderrHas: "given more than once"},
		{name: "JSON not an object", args: []string{"read_file", `["x"]`}, code: cli.ExitUsage, stderrHas: "not a JSON object"},
		{name: "two positionals", args: []string{"read_file", "{}", "{}"}, code: cli.ExitUsage, stderrHas: "one JSON argument at most"},
		{name: "JSON split by a quote", args: []string{"read_file", `{"path": "Its`, `here"}`}, code: cli.ExitUsage, stderrHas: `as 2 words: a single quote inside the single quotes ended them`},
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
			stdoutJSON: `{"message":"Delete the branch?","answers_given":0,"output":"kept"}`,
			stderrHas:  "--answer",
		},
		{
			name:       "second question unanswered",
			args:       []string{"--answer", "accept", "delete"},
			code:       cli.ExitNeedsAnswer,
			stdoutJSON: `{"message":"Why?","schema":{"type":"object","properties":{"reason":{"type":"string"},"days":{"type":"integer"}},"required":["reason"]},"answers_given":1,"error":"not deleted: no reason given"}`,
		},
		{name: "answered", args: []string{"--answer", "accept", "--answer", `{"reason":"merged"}`, "delete"}, stdout: "deleted: {\"reason\":\"merged\"}\n"},
		{name: "declined", args: []string{"--answer", "decline", "delete"}, stdout: "kept\n"},
		{name: "cancelled", args: []string{"--answer", "cancel", "delete"}, stdout: "kept\n"},
		{name: "unused answers are reported", args: []string{"--answer", "decline", "--answer", "accept", "delete"}, stdout: "kept\n", stderrHas: "1 --answer not used"},
		{name: "bad answer", args: []string{"--answer", "maybe", "delete"}, code: cli.ExitUsage, stderrHas: "an answer is accept"},
		{name: "progress on stderr", args: []string{"work"}, stdout: "done\n", stderrHas: "[1/2] half\n"},
		{name: "progress without a total, and as text", args: []string{"steps"}, stdout: "done\n", stderrHas: "[3] counting\nhalfway\n"},
		{name: "a parameter named help is a flag", args: []string{"helpful", "--help", "yes"}, stdout: "help=yes\n"},
		{name: "file parts", args: []string{"files"}, stdout: "https://example.com/f.pdf\nfile_id: f_1\nnot base64!\n"},
		{name: "record file that cannot open", args: []string{"--record", "/", "ping"}, code: cli.ExitUsage, stderrHas: "--record"},
		{name: "fields as flags", args: []string{"deploy", "--target.host", "O'Brien", "--target.port", "80", "--target.tls.on"}, stdoutJSON: `{"target":{"host":"O'Brien","port":80,"tls":{"on":true}}}`},
		{name: "a nullable object's fields", args: []string{"deploy", "--target.host", "a", "--backup.host", "b"}, stdoutJSON: `{"target":{"host":"a"},"backup":{"host":"b"}}`},
		{name: "map entries", args: []string{"deploy", "--target.host", "a", "--limits", "cpu=2", "--limits", "mem=4", "--switches", "dry=true"}, stdoutJSON: `{"target":{"host":"a"},"limits":{"cpu":2,"mem":4},"switches":{"dry":true}}`},
		{name: "a map value with an equals sign", args: []string{"deploy", "--switches", "a=b=true"}, code: cli.ExitUsage, stderrHas: `"b=true" is not true or false`},
		{name: "a map entry of the wrong type", args: []string{"deploy", "--limits", "cpu=lots"}, code: cli.ExitUsage, stderrHas: `"lots" is not an integer`},
		{name: "a map entry without a key", args: []string{"deploy", "--limits", "=2"}, code: cli.ExitUsage, stderrHas: "is not key=value"},
		{name: "a map entry without a value", args: []string{"deploy", "--limits", "cpu"}, code: cli.ExitUsage, stderrHas: "is not key=value"},
		{name: "a map key twice", args: []string{"deploy", "--limits", "cpu=1", "--limits", "cpu=2"}, code: cli.ExitUsage, stderrHas: `gives the key "cpu" twice`},
		{name: "a field given twice", args: []string{"deploy", "--target.host", "a", "--target.host", "b"}, code: cli.ExitUsage, stderrHas: "given more than once"},
		{name: "flags add to the JSON", args: []string{"deploy", "--target.port", "80", "--limits", "cpu=2", `{"target":{"host":"h","a.b":"x"},"limits":{"mem":4}}`}, stdoutJSON: `{"target":{"host":"h","a.b":"x","port":80},"limits":{"cpu":2,"mem":4}}`},
		{name: "a field in both", args: []string{"deploy", "--target.host", "a", `{"target":{"host":"h"}}`}, code: cli.ExitUsage, stderrHas: `--target.host and the JSON argument both give "target.host"`},
		{name: "a map key in both", args: []string{"deploy", "--limits", "cpu=2", `{"target":{"host":"h"},"limits":{"cpu":1}}`}, code: cli.ExitUsage, stderrHas: `--limits and the JSON argument both give "limits.cpu"`},
		{name: "a field under a value that is not an object", args: []string{"deploy", "--target.host", "a", `{"target":"h"}`}, code: cli.ExitUsage, stderrHas: `the JSON argument gives "target", which is not an object`},
		{name: "a field under null", args: []string{"deploy", "--backup.host", "a", `{"target":{"host":"h"},"backup":null}`}, code: cli.ExitUsage, stderrHas: `"backup", which is not an object`},
		{name: "a field whose name holds a dot has no flag", args: []string{"deploy", "--target.a.b", "x"}, code: cli.ExitUsage, stderrHas: "flag provided but not defined"},
		{name: "a map of objects has no flag", args: []string{"deploy", "--extra", "a=b"}, code: cli.ExitUsage, stderrHas: "flag provided but not defined"},
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
		{name: "a form with no fields confirms", in: "n\n", q: agenttool.Elicitation{Message: "Go?", Schema: json.RawMessage(`{"type":"object","properties":{}}`)}, action: agenttool.ActionDecline},
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

// The data a file part carries is saved under the name the tool gave,
// in whichever base64 it is written, and a data URL that is not base64
// is percent-decoded.
func TestRunFileData(t *testing.T) {
	set := agenttool.Set{agenttool.New("f", "", func(ctx context.Context, _ agenttool.NoArgs) (openresponses.Contents, error) {
		return openresponses.Contents{
			&openresponses.InputFile{Filename: "../report.txt", FileData: base64.RawURLEncoding.EncodeToString([]byte("raw url"))},
			&openresponses.InputFile{Filename: "report.txt", FileData: "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("data url"))},
			&openresponses.InputImage{ImageURL: "data:text/plain;charset=utf-8,hello%20world"},
			&openresponses.InputImage{ImageURL: "data:image/png;base64," + base64.RawStdEncoding.EncodeToString(png[:7])},
		}, nil
	})}
	dir := t.TempDir()
	var stdout, stderr strings.Builder
	r := cli.Runner{Name: "kit", Tools: set, Stdout: &stdout, Stderr: &stderr}
	if code := r.Run(context.Background(), []string{"--out", dir, "f"}); code != cli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := map[string]string{"report.txt": "raw url", "report-2.txt": "data url", "output-3.txt": "hello world", "output-4.png": string(png[:7])}
	for name, body := range want {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != body {
			t.Errorf("%s = %q, %v; want %q\nstdout: %s", name, data, err, body, stdout.String())
		}
	}
}

type badWriter struct{}

func (badWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// An output that cannot be written does not make a call that ran read
// as one that failed, since a model would correct it and run it again.
func TestRunOutputUnwritable(t *testing.T) {
	var stderr strings.Builder
	r := cli.Runner{Name: "kit", Tools: tools(), Stdout: badWriter{}, Stderr: &stderr}
	if code := r.Run(context.Background(), []string{"ping"}); code != cli.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "the call succeeded") {
		t.Errorf("stderr: %s", stderr.String())
	}
}

func TestRunInvalidSet(t *testing.T) {
	ping := agenttool.New("a", "", func(ctx context.Context, _ agenttool.NoArgs) (string, error) { return "", nil })
	var stderr strings.Builder
	r := cli.Runner{Name: "kit", Tools: agenttool.Set{ping, ping}, Stdout: io.Discard, Stderr: &stderr}
	if code := r.Run(context.Background(), []string{"help"}); code != cli.ExitUsage {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "duplicate") {
		t.Errorf("stderr: %s", stderr.String())
	}
}

type unnamed struct{}

func (unnamed) RecordNS() string { return "" }

// A result record that cannot be made is reported and does not change
// the status, since the call has happened.
func TestRunRecordOfFails(t *testing.T) {
	set := agenttool.Set{agenttool.New("u", "", func(ctx context.Context, _ agenttool.NoArgs) (agenttool.Result, error) {
		return agenttool.Result{Output: agenttool.Text("ok").Output, Details: unnamed{}}, nil
	})}
	var stdout, stderr strings.Builder
	r := cli.Runner{Name: "kit", Tools: set, Stdout: &stdout, Stderr: &stderr}
	path := filepath.Join(t.TempDir(), "r.jsonl")
	if code := r.Run(context.Background(), []string{"--record", path, "u"}); code != cli.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "empty namespace") || stdout.String() != "ok\n" {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

// A record the file refuses reaches the tool, which here fails the call
// on it, and the program reports it once, after the call.
func TestRunRecordWriteFails(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("no /dev/full")
	}
	code, _, stderr := run(t, "", "--record", "/dev/full", "work")
	if code != cli.ExitFailed {
		t.Fatalf("exit %d", code)
	}
	if n := strings.Count(stderr, "kit: --record"); n != 1 {
		t.Errorf("reported %d times:\n%s", n, stderr)
	}
}

func TestCommandsRefuses(t *testing.T) {
	ping := func(name string) agenttool.Tool {
		return agenttool.New(name, "", func(ctx context.Context, _ agenttool.NoArgs) (string, error) { return "", nil })
	}
	for name, set := range map[string]agenttool.Set{
		"duplicate":    {ping("a"), ping("a")},
		"dash":         {ping("-x")},
		"reserved":     {ping("help")},
		"empty name":   {ping("")},
		"schema named": {ping("schema")},
	} {
		if _, err := cli.Commands(set); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDescribeLenient(t *testing.T) {
	nop := func(context.Context, agenttool.Call) (agenttool.Result, error) { return agenttool.Result{}, nil }
	for name, tt := range map[string]struct {
		schema string
		want   []string // parameter names, in order
	}{
		"not JSON":            {schema: `{`, want: nil},
		"properties an array": {schema: `{"properties":[]}`, want: nil},
		"a name twice":        {schema: `{"properties":{"a":{"type":"string"},"b":{},"a":{"type":"integer"}}}`, want: []string{"a", "b"}},
		"a boolean schema":    {schema: `{"properties":{"a":false}}`, want: []string{"a"}},
	} {
		c := cli.Describe(agenttool.NewFunc("t", "", json.RawMessage(tt.schema), nop))
		var got []string
		for _, p := range c.Params {
			got = append(got, p.Name)
		}
		if strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Errorf("%s: params %q, want %q", name, got, tt.want)
		}
		if name == "a name twice" && c.Params[0].Type != "string" {
			t.Errorf("%s: the second a was described", name)
		}
	}
}

// Writing into an --out that already holds a run's files takes new
// names, so a second run neither fails after its tool has run nor
// overwrites the first run's files.
func TestRunOutReused(t *testing.T) {
	dir := t.TempDir()
	for _, want := range []string{"output-1.png", "output-1-2.png"} {
		code, stdout, stderr := run(t, "", "--out", dir, "shot")
		if code != cli.ExitOK {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		if !strings.Contains(stdout, filepath.Join(dir, want)) {
			t.Errorf("stdout lacks %s:\n%s", want, stdout)
		}
	}
}

// A program named by a path, as os.Args[0] is, still makes the
// temporary directory its files go to.
func TestRunNameWithPath(t *testing.T) {
	var stdout, stderr strings.Builder
	r := cli.Runner{Name: "./bin/kit", Tools: tools(), Stdout: &stdout, Stderr: &stderr}
	if code := r.Run(context.Background(), []string{"shot"}); code != cli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	lines := strings.Split(stdout.String(), "\n")
	if len(lines) < 2 || !strings.HasSuffix(lines[1], "output-1.png") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(lines[1])) })
	if !strings.HasPrefix(filepath.Base(filepath.Dir(lines[1])), "kit-") {
		t.Errorf("directory %s", filepath.Dir(lines[1]))
	}
}

// An interrupt while the prompt waits for a line ends the question at
// once, with the context's error.
func TestPromptCancelled(t *testing.T) {
	in, w := io.Pipe()
	defer w.Close()
	ask := cli.Prompt(in, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ask(ctx, agenttool.Elicitation{Message: "Go?"})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt waited for a line after its context ended")
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

// What the usage tells a model about one command: no hint the tool did
// not give, an enum number as the tool will accept it, and the JSON
// argument shown as required when a JSON-only parameter is.
func TestMarkdownDetails(t *testing.T) {
	nop := func(context.Context, agenttool.Call) (agenttool.Result, error) { return agenttool.Result{}, nil }
	tool := agenttool.NewFunc("c", "",
		json.RawMessage(`{"properties":{"id":{"type":"integer","enum":[12345678901234567891]},"cfg":{"type":"object"}},"required":["cfg"]}`),
		nop, agenttool.WithAnnotations(agenttool.Annotations{Title: "Configure"}))
	got := cli.Markdown("kit", []cli.Command{cli.Describe(tool)})
	for _, want := range []string{"kit c [--id <integer>] (<json> | -)", "One of `12345678901234567891`"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Hints") {
		t.Errorf("usage claims a hint the tool did not give:\n%s", got)
	}
}
