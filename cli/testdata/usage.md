## Calling `kit`

Run one command per call. A command takes its arguments as flags, as one JSON object, or both:

```sh
kit <command> --name value
kit <command> '{"name": "value"}'
kit <command> - <<'EOF'
{"name": "it's"}
EOF
```

Give a JSON argument in single quotes on one line, as the second form does: the shell passes single-quoted text to the command unchanged. A single quote inside would end the quoting, so JSON that holds one goes on stdin in a quoted heredoc instead, as the last form does. Write a newline inside a JSON string as `\n`. Flags go before the JSON argument, and a property is given once, as a flag or in the JSON. A boolean flag stands alone for true, or takes `=false`.

The exit status says what happened:

- 0: the command succeeded, and its output is on stdout. If stderr says the output could not be written, the command still ran: do not run it again for that.
- 1: the tool failed or refused the arguments, and says why on stderr. Correct the call and run it again.
- 2: the command did not run, because the command line was not understood or the program could not start it, and stderr says why.
- 3: the tool asked the user a question that nobody answered, and was told it was cancelled. Stdout holds the question as JSON, with what the tool returned. Ask the user, then run the same command again with `--answer` before the command name: `--answer accept`, `--answer decline`, or `--answer '{...}'` with the fields a form asks for. A command that asks again needs every earlier answer again, in the order given.

A file a command produces, such as an image, is written to disk and its path printed. `kit help <command>` prints one command's usage and `kit schema <command>` its JSON Schema.

## Commands

### `read_file`

Read a file from disk.

Hints from the tool: read-only.

```sh
kit read_file --path <string> [--max_bytes <integer>]
```

- `--path` (string, required): Absolute path to read
- `--max_bytes` (integer): Stop after this many bytes

### `tag`

Set tags.
Second line.

Hints from the tool: may be destructive; reaches outside systems.

```sh
kit tag --tags <string> ... [--level <string>] [--verbose] [--ratio <number>] [<json> | -]
```

- `--tags` (array of string, repeatable, required): Tags to set
- `--level` (string). One of `"low"`, `"high"`
- `--verbose` (boolean)
- `--ratio` (number)
- `labels` (object, JSON only): Labels by key
- `items` (array of object, JSON only)

### `ping`

```sh
kit ping
```

### `fail`

Always fails.

```sh
kit fail
```

### `boom`

Panics.

```sh
kit boom
```

### `delete`

Ask, then delete.

```sh
kit delete
```

### `shot`

Take a screenshot.

```sh
kit shot
```

### `work`

Report progress and records.

```sh
kit work
```

### `raw`

A schema from elsewhere.

```sh
kit raw [--count <integer>] [<json> | -]
```

- `--count` (integer)
- `mixed` (any JSON value, JSON only)
- `a=b` (string, JSON only)
- `-x` (string, JSON only)

### `odd`

Odd schemas.

```sh
kit odd [--n <integer>] [--bs ...] [<json> | -]
```

- `x` (any JSON value, JSON only)
- `t` (array, JSON only)
- `--n` (integer)
- `--bs` (array of boolean, repeatable)

### `steps`

Report progress two other ways.

```sh
kit steps
```

### `helpful`

A parameter named help.

```sh
kit helpful --help <string>
```

- `--help` (string, required)

### `files`

Return files.

```sh
kit files
```

### `nullschema`

Parameters of null.

```sh
kit nullschema
```
