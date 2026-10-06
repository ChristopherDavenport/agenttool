package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

// Markdown renders how to call program and each of cmds, for a model
// to read: the body of a skill, or a section of AGENTS.md. Headings
// start at level two, so it nests under the title of whatever holds
// it. It is rendered from the same [Command] values [Runner] parses
// against, so what it teaches is what the program accepts.
func Markdown(program string, cmds []Command) string {
	var b strings.Builder
	writeCalling(&b, program)
	b.WriteString("\n## Commands\n")
	for _, c := range cmds {
		b.WriteString("\n")
		writeCommand(&b, program, c)
	}
	return b.String()
}

// writeCalling writes the part of the usage that is the same for every
// command: the forms of a call, the exit status and how to answer a
// question.
func writeCalling(b *strings.Builder, program string) {
	fmt.Fprintf(b, "## Calling `%s`\n\n", program)
	fmt.Fprintf(b, "Run one command per call. A command takes its arguments as flags, as one JSON object, or both:\n\n")
	fmt.Fprintf(b, "```sh\n")
	fmt.Fprintf(b, "%s <command> --name value\n", program)
	fmt.Fprintf(b, "%s <command> '{\"name\": \"value\"}'\n", program)
	fmt.Fprintf(b, "%s <command> - <<'EOF'\n", program)
	fmt.Fprintf(b, "{\"name\": \"value\"}\n")
	fmt.Fprintf(b, "EOF\n")
	fmt.Fprintf(b, "```\n\n")
	fmt.Fprintf(b, "Use the last form for any value with quotes, newlines or nesting: a quoted heredoc reaches the command untouched by the shell. Flags go before the JSON argument, and a property is given once, as a flag or in the JSON. A boolean flag stands alone for true, or takes `=false`.\n\n")
	fmt.Fprintf(b, "The exit status says what happened:\n\n")
	fmt.Fprintf(b, "- 0: the command succeeded, and its output is on stdout.\n")
	fmt.Fprintf(b, "- 1: the tool failed or refused the arguments, and says why on stderr. Correct the call and run it again.\n")
	fmt.Fprintf(b, "- 2: the command line was not understood, and stderr says why.\n")
	fmt.Fprintf(b, "- 3: the tool needs the user's answer before it can go on, and stdout holds the question as JSON. Ask the user, then run the same command again with `--answer` before the command name: `--answer accept`, `--answer decline`, or `--answer '{...}'` with the fields a form asks for. A command that asks again needs every earlier answer again, in the order given.\n\n")
	fmt.Fprintf(b, "A file a command produces, such as an image, is written to disk and its path printed. `%s help <command>` prints one command's usage and `%s schema <command>` its JSON Schema.\n", program, program)
}

// writeCommand writes one command's section: its description and
// hints, its synopsis, and each parameter.
func writeCommand(b *strings.Builder, program string, c Command) {
	fmt.Fprintf(b, "### `%s`\n\n", c.Name)
	desc := strings.TrimSpace(c.Description)
	if hints := hintsOf(c.Annotations); hints != "" {
		if desc != "" {
			desc += "\n\n"
		}
		desc += hints
	}
	if desc != "" {
		b.WriteString(desc)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(b, "```sh\n%s\n```\n", synopsis(program, c))
	if len(c.Params) == 0 {
		return
	}
	b.WriteString("\n")
	for _, p := range c.Params {
		fmt.Fprintf(b, "- %s\n", paramLine(p))
	}
}

// writeIndex writes the list of commands with the first line of each
// description, for help with no command named.
func writeIndex(b *strings.Builder, cmds []Command) {
	b.WriteString("\n## Commands\n\n")
	for _, c := range cmds {
		line, _, _ := strings.Cut(strings.TrimSpace(c.Description), "\n")
		if line == "" {
			fmt.Fprintf(b, "- `%s`\n", c.Name)
			continue
		}
		fmt.Fprintf(b, "- `%s`: %s\n", c.Name, line)
	}
}

// synopsis is the command line with every flag: required ones bare,
// optional ones in brackets, repeated ones with an ellipsis, and the
// JSON argument when a parameter has no flag.
func synopsis(program string, c Command) string {
	parts := []string{program, c.Name}
	jsonOnly := false
	for _, p := range c.Params {
		if !p.Flag() {
			jsonOnly = true
			continue
		}
		f := fmt.Sprintf("--%s <%s>", p.Name, p.value())
		if p.Type == "boolean" {
			f = "--" + p.Name
		}
		if p.Repeated() {
			f += " ..."
		}
		if !p.Required {
			f = "[" + f + "]"
		}
		parts = append(parts, f)
	}
	if jsonOnly {
		parts = append(parts, "[<json> | -]")
	}
	return strings.Join(parts, " ")
}

// paramLine is one parameter: its flag or JSON name, its type, whether
// it is required, its description and its enum.
func paramLine(p Param) string {
	var b strings.Builder
	if p.Flag() {
		fmt.Fprintf(&b, "`--%s`", p.Name)
	} else {
		fmt.Fprintf(&b, "`%s`", p.Name)
	}
	var facts []string
	switch {
	case p.Type == "":
		facts = append(facts, "any JSON value")
	case p.Type == "array" && p.Items != "":
		facts = append(facts, "array of "+p.Items)
	default:
		facts = append(facts, p.Type)
	}
	if p.Repeated() {
		facts = append(facts, "repeatable")
	}
	if p.Required {
		facts = append(facts, "required")
	}
	if !p.Flag() {
		facts = append(facts, "JSON only")
	}
	fmt.Fprintf(&b, " (%s)", strings.Join(facts, ", "))
	if d := strings.TrimSpace(p.Description); d != "" {
		b.WriteString(": ")
		b.WriteString(strings.Join(strings.Fields(d), " "))
	}
	if len(p.Enum) > 0 {
		vals := make([]string, 0, len(p.Enum))
		for _, v := range p.Enum {
			data, err := json.Marshal(v)
			if err != nil {
				continue
			}
			vals = append(vals, "`"+string(data)+"`")
		}
		fmt.Fprintf(&b, ". One of %s", strings.Join(vals, ", "))
	}
	return b.String()
}

// hintsOf states a tool's annotations, or nothing when it carries
// none. Destructive and idempotent are meaningful only for a tool that
// is not read-only, as in MCP. They are the tool's own word, and the
// usage says so.
func hintsOf(a agenttool.Annotations) string {
	if a == (agenttool.Annotations{}) {
		return ""
	}
	var hints []string
	switch {
	case a.ReadOnly:
		hints = append(hints, "read-only")
	case a.Destructive:
		hints = append(hints, "may be destructive")
	default:
		hints = append(hints, "makes only additive changes")
	}
	if !a.ReadOnly && a.Idempotent {
		hints = append(hints, "idempotent")
	}
	if a.OpenWorld {
		hints = append(hints, "reaches outside systems")
	}
	return "Hints from the tool: " + strings.Join(hints, "; ") + "."
}
