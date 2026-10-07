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
//
// The JSON argument is taught single-quoted on one line, and in a
// heredoc only when it holds a single quote, which would end the
// quoting. Claude Code asks about a heredoc whose body holds a brace
// followed by a quote, as every JSON object's does, even when a rule
// allows the command, so a skill that pre-approves a command is asked
// about each call made that way. Models do not escape a single quote
// as \u0027 when told to, and a call that is asked about arrives
// intact, which one the model mangles to get past a refusal does not.
func writeCalling(b *strings.Builder, program string) {
	fmt.Fprintf(b, "## Calling `%s`\n\n", program)
	fmt.Fprintf(b, "Run one command per call. A command takes its arguments as flags, as one JSON object, or both. Use flags wherever the command has them:\n\n")
	fmt.Fprintf(b, "```sh\n")
	fmt.Fprintf(b, "%s <command> --name \"it's two words\" --object.field value --map \"key=O'Brien\"\n", program)
	fmt.Fprintf(b, "```\n\n")
	fmt.Fprintf(b, "A field of an object is a flag named by its path, `--object.field`, and an entry of a map is `--map key=value`, given once per entry. Put a value in double quotes whenever it holds a space, a single quote or a line break, a map entry's included, and escape `\\\"`, `\\$`, `` \\` `` and `\\\\` inside them. Type a line break as a line break inside the quotes: `\\n` there is a backslash and an n. A boolean flag stands alone for true, or takes `=false`.\n\n")
	fmt.Fprintf(b, "A value no flag can give, such as an array of objects, goes in a JSON argument after the flags, which adds to them: each value is given once, as a flag or in the JSON. Give it in single quotes on one line, writing a newline inside a string as `\\n`:\n\n")
	fmt.Fprintf(b, "```sh\n")
	fmt.Fprintf(b, "%s <command> --name value '{\"items\": [{\"name\": \"value\"}]}'\n", program)
	fmt.Fprintf(b, "```\n\n")
	fmt.Fprintf(b, "Only when the JSON holds a single quote, which would end the quoting, give it on stdin in a quoted heredoc instead:\n\n")
	fmt.Fprintf(b, "```sh\n")
	fmt.Fprintf(b, "%s <command> - <<'EOF'\n", program)
	fmt.Fprintf(b, "{\"items\": [{\"name\": \"it's\"}]}\n")
	fmt.Fprintf(b, "EOF\n")
	fmt.Fprintf(b, "```\n\n")
	fmt.Fprintf(b, "The exit status says what happened:\n\n")
	fmt.Fprintf(b, "- 0: the command succeeded, and its output is on stdout. If stderr says the output could not be written, the command still ran: do not run it again for that.\n")
	fmt.Fprintf(b, "- 1: the tool failed or refused the arguments, and says why on stderr. Correct the call and run it again.\n")
	fmt.Fprintf(b, "- 2: the command did not run, because the command line was not understood or the program could not start it, and stderr says why.\n")
	fmt.Fprintf(b, "- 3: the tool asked the user a question that nobody answered, and was told it was cancelled. Stdout holds the question as JSON, with what the tool returned. Ask the user, then run the same command again with `--answer` before the command name: `--answer accept`, `--answer decline`, or `--answer '{...}'` with the fields a form asks for. A command that asks again needs every earlier answer again, in the order given.\n\n")
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
		writeParam(b, p, nil, "")
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
// JSON argument when a value has no flag.
func synopsis(program string, c Command) string {
	parts := []string{program, c.Name}
	for _, f := range flagsOf(c.Params) {
		name, p := strings.Join(f.path, "."), f.param
		var u string
		switch {
		case p.Map():
			u = fmt.Sprintf("--%s <key>=<%s> ...", name, p.Values)
		case p.value() == "boolean":
			u = "--" + name
		default:
			u = fmt.Sprintf("--%s <%s>", name, p.value())
		}
		if p.Repeated() {
			u += " ..."
		}
		if !f.required {
			u = "[" + u + "]"
		}
		parts = append(parts, u)
	}
	jsonOnly, jsonRequired := false, false
	for _, p := range c.Params {
		jsonOnly = jsonOnly || p.needsJSON(nil, false)
		jsonRequired = jsonRequired || p.needsJSON(nil, true)
	}
	switch {
	case jsonRequired:
		parts = append(parts, "(<json> | -)")
	case jsonOnly:
		parts = append(parts, "[<json> | -]")
	}
	return strings.Join(parts, " ")
}

// writeParam writes one parameter's line, under the path prefix, and
// below it a line for each field of an object whose fields have flags.
func writeParam(b *strings.Builder, p Param, prefix []string, indent string) {
	fmt.Fprintf(b, "%s- %s\n", indent, paramLine(p, prefix))
	if p.nests() {
		path := append(append([]string(nil), prefix...), p.Name)
		for _, f := range p.Fields {
			writeParam(b, f, path, indent+"  ")
		}
	}
}

// paramLine is one parameter: its flag or JSON path, its type, whether
// it is required, its description and its enum.
func paramLine(p Param, prefix []string) string {
	var b strings.Builder
	name := strings.Join(append(append([]string(nil), prefix...), p.Name), ".")
	flag := p.flagAt(prefix)
	if flag {
		fmt.Fprintf(&b, "`--%s`", name)
	} else {
		fmt.Fprintf(&b, "`%s`", name)
	}
	var facts []string
	switch {
	case p.Type == "":
		facts = append(facts, "any JSON value")
	case p.Type == "array" && p.Items != "":
		facts = append(facts, "array of "+p.Items)
	case p.Type == "object" && p.Values != "":
		facts = append(facts, "map of "+p.Values)
	default:
		facts = append(facts, p.Type)
	}
	if flag && (p.Repeated() || p.Map()) {
		facts = append(facts, "repeatable")
	}
	if p.Required {
		facts = append(facts, "required")
	}
	if !flag && !p.nests() {
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

// hintsOf states the annotations a tool set, or nothing when it set
// none of them. A hint left unset says nothing, as in MCP, where an
// unset destructive hint even defaults to true, so the usage never
// claims more than the tool did. Destructive and idempotent are
// meaningful only for a tool that is not read-only. The title is a
// display name and not a hint.
func hintsOf(a agenttool.Annotations) string {
	var hints []string
	switch {
	case a.ReadOnly:
		hints = append(hints, "read-only")
	case a.Destructive:
		hints = append(hints, "may be destructive")
	}
	if !a.ReadOnly && a.Idempotent {
		hints = append(hints, "idempotent")
	}
	if a.OpenWorld {
		hints = append(hints, "reaches outside systems")
	}
	if len(hints) == 0 {
		return ""
	}
	return "Hints from the tool: " + strings.Join(hints, "; ") + "."
}
