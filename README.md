# compiledb-go

`compiledb-go` generates a [Clang JSON Compilation Database][compdb], usually
named `compile_commands.json`, from GNU Make dry-run output or an existing build
log.

It is intended for Make-based projects that cannot export a compilation
database directly. It can run a real build followed by controlled Make
discovery, or parse an existing log without running the build. The resulting
database can be used by clangd, clang-tidy, IDEs, and other Clang tools.

This project started as a Go rewrite of the Python
[nickdiego/compiledb][python-compiledb] project to make large build-log parsing
substantially faster. Its CLI, shell parser, compiler argument scanner, and Make
wrapper are now implemented and maintained independently in Go.

## Why Go?

Parsing speed was the original reason for the rewrite. The historical benchmark
that motivated the project used a build log larger than 2 MiB with more than 200
valid compilation entries:

| Implementation | User time | System time | Elapsed time |
| --- | ---: | ---: | ---: |
| `compiledb-go` | 0.21 s | 0.02 s | 0.210 s |
| Python `compiledb` | 5.21 s | 0.01 s | 5.179 s |

In that test, the Go implementation completed in about 1/25 of the elapsed
time:

```sh
make -Bnkw > build.log
time compiledb --parse build.log
```

These numbers are retained as the original project benchmark, not as a promise
for every project or machine. Current performance depends on log structure,
filesystem access, enabled options, and the number of generated entries.

## Features

- Parses existing build logs or discovers commands through GNU Make.
- Tracks working directories across recursive Make, `make -C`, and inline
  `cd` commands.
- Detects GCC and Clang commands with multiple source inputs and GNU response
  files.
- Merges or overwrites databases and emits either `arguments` or `command`
  entries.
- Supports GNU Make-compatible workflows on Linux, macOS, and Windows.

## Installation

Download a binary for a supported platform from [Releases][releases], or
install with Go:

```sh
go install github.com/fcying/compiledb-go/cmd/compiledb@latest
```

To build from source:

```sh
go build -o compiledb ./cmd/compiledb
```

The required Go version is declared in [`go.mod`](go.mod).

## Quick Start

Run Make and update `compile_commands.json` after the real build succeeds:

```sh
compiledb make
```

Skip the real build and run Make discovery only:

```sh
compiledb --no-build make
```

Parse an existing build log:

```sh
compiledb --parse build.log
```

Parse Make output from stdin:

```sh
make -Bnkw -j1 --print-directory | compiledb --parse -
```

Write the compilation database to stdout:

```sh
compiledb --output - --parse build.log
```

## Command Reference

```text
USAGE:
  compiledb [options] command [command options] [args]...

OPTIONS:
  --parse file, -p file      Build log to parse, or '-' for stdin.
                             Default: -
  --output file, -o file     Output file, or '-' for stdout.
                             Default: compile_commands.json
  --overwrite, -f            Replace the database instead of updating it.
  --build-dir path, -d path  Initial build directory.
  --exclude regex            Exclude source paths matching from the start.
                             Repeat the option for multiple expressions.
  -e regex                   Alias for --exclude.
  --encoding value           Wrapped Make output encoding: raw or gb18030.
                             Can also be set with COMPILEDB_ENCODING.
                             Default: raw
  --no-build, -n             Skip the real build and run discovery only.
  --verbose, -v              Print verbose diagnostics.
  --no-strict, -S            Do not require source files to exist locally.
  --macros, -m               Add predefined compiler macros.
  --add-arg value            Add an argument to every compiler command.
                             Repeat the option for multiple arguments.
  -a value                   Alias for --add-arg.
  --command-style, -c        Emit a single "command" string instead of an
                             "arguments" array.
  --full-path                Emit the full compiler executable path.
  --regex-compile regex      Override the built-in compiler-name regex.
  --regex-file regex         Override the built-in source-file regex.
  --help, -h                 Show help.

COMMANDS:
  make [MAKE_ARGS]...        Run GNU Make and generate the database.
    --cmd value, -c value    Select the GNU Make-compatible executable.
    --help, -h               Show make subcommand help.
```

The top-level options must appear before the `make` subcommand. Arguments not
consumed by the subcommand wrapper are forwarded to Make. Run the installed
binary for the generated help, including the current default regular
expressions:

```sh
compiledb --help
compiledb make --help
```

## Make Mode

By default, `compiledb make` runs two phases:

1. Run the requested real Make command with the user's arguments.
2. After the real build succeeds, run a serialized dry-run discovery and parse
   its stdout.

Discovery does not run if the real build fails. If discovery fails, its partial
output is not written to the database. `--no-build/-n` skips the first phase and
runs discovery directly. Make stderr is always forwarded as diagnostics and is
never parsed as compiler output.

Ordinary arguments after `make` are forwarded to GNU Make:

```sh
compiledb make -C build all
compiledb make -f core/main.mk CC=clang
```

Use `make --cmd/-c` to select another GNU Make-compatible executable. This `-c`
belongs to the `make` subcommand and is separate from the top-level
`--command-style/-c` option:

```sh
compiledb make --cmd gmake -C build
compiledb --command-style make --cmd mingw32-make
```

Before the first `--`, the wrapper handles `make --cmd/-c` and
`make --help/-h`. The first `--` is a wrapper delimiter and is not forwarded;
all later arguments are passed to Make unchanged.

### Recursive Make

Discovery relies on GNU Make Entering/Leaving directory markers to determine
the working directory of each compiler command. To handle recipes containing
`$(MAKE) --no-print-directory`, discovery temporarily uses an internal Make
proxy that:

- Removes `--no-print-directory` from recursive Make arguments.
- Requests `--print-directory`.
- Delegates to the same resolved Make executable used by the top-level command.
- Affects discovery only, never the real build.
- Preserves explicit `MAKE` selection and `-e/--environment-overrides`
  semantics.

The proxy intercepts standard `$(MAKE)` recursion only. It cannot correct the
following cases:

- Recipes that invoke a hard-coded path such as `/usr/bin/make`.
- Wrappers that replace themselves with a different hard-coded Make.
- Makefiles that use `override MAKEFLAGS` to disable directory markers again.
- Existing build logs that were captured without directory markers.

## Build-Log Mode

Without a subcommand, `compiledb` reads the file selected by `--parse/-p`.
`--parse -` is the default and reads stdin.

The initial working directory is selected in this order:

1. An explicit `--build-dir/-d`.
2. The directory containing the build-log file.
3. The directory in which `compiledb` was started when reading stdin.

For example, parse a copied log from an explicit local starting directory:

```sh
compiledb --no-strict \
  --build-dir /opt/project/build \
  --parse /tmp/build.log
```

An explicit `--build-dir` must exist as a directory on the local machine, even
with `--no-strict`. After parsing starts, `--no-strict` allows tracked paths and
source files from the original host to remain absent locally.

The parser follows Make directory markers, `make -C`, and simple inline `cd`
commands. Supported shell command lists include simple commands, `;`, `&&`, and
`||`. Complex groups, functions, and control structures whose execution or
working directory cannot be determined safely are skipped as a whole rather
than guessed.

A markerless text log cannot reliably distinguish nested submakes from sibling
submakes. When the log generation can be controlled, use
`-j1 --print-directory`.

## Compiler and Source Detection

The default scanner parses compiler options and their operands instead of only
searching for words with source-file suffixes. It can therefore:

- Record compiler-driver commands that contain source inputs without `-c`.
- Emit one compilation database entry per source in a multi-source command.
- Handle `--`, `-x LANG`, and compiler options that consume another argument.
- Exclude link-only commands, `clang-format`, `clang-tidy`, and other
  non-compiler invocations.

Known source suffixes are `.c`, `.C`, `.cc`, `.cpp`, `.cxx`, `.c++`, `.s`,
`.S`, `.m`, `.mm`, and `.cu`. An explicit `-x` also permits a source without a
known suffix.

`--regex-compile` and `--regex-file` are available for custom log formats. A
custom file regular expression still matches the original build-log command;
it does not inspect response-file contents.

## Response Files

For recognized GCC and Clang GNU-mode commands, `@file` arguments after the
compiler token are expanded before source detection. Response-file paths are
resolved relative to the compiler process working directory. Nested files use
the same working directory. The generated database contains the flattened
arguments and no longer depends on the response files afterward.

Expansion accepts GNU-style UTF-8 input without a byte order mark. A missing
file, malformed quote, recursive include, non-regular file, unsupported
encoding, or resource-limit violation produces a recoverable error and skips
only the affected compiler command. Parsing continues with later commands.

Response-file expansion is not currently supported for:

- `clang-cl`.
- `clang --driver-mode=cl`.
- `clang --rsp-quoting=windows`.
- Native `cl.exe` response-file syntax.

## Strict Mode

Strict mode is enabled by default. A source must resolve to a regular file
relative to its compilation database entry directory, otherwise the entry is
omitted. A symlink to a regular file is accepted.

`--no-strict/-S` disables source-existence checks and is useful for logs copied
from another host or container. It does not relax response-file checks;
response files must still be readable local regular files.

## Output

The default output is `compile_commands.json` in the current directory. If the
file already exists, new entries update and deduplicate it by source path. Use
`--overwrite/-f` to replace the existing database:

```sh
compiledb --overwrite --parse build.log
```

The default format uses an `arguments` array. To emit a single
platform-appropriate `command` string instead:

```sh
compiledb --command-style --parse build.log
```

`--output -` writes JSON to stdout. Logs, parser diagnostics, and Make output
are kept on stderr:

```sh
compiledb --output - --no-build make | jq .
```

Common output adjustments include:

```sh
compiledb --add-arg=-m32 --add-arg='-DCSV=a,b' make
compiledb --exclude '^third_party/' make
compiledb --full-path make
compiledb --macros make
```

`--macros/-m` invokes the compiler to collect predefined macros. The compiler
must be available from its working directory or `PATH`. Unsafe or unsupported
probes are skipped while the compilation entry itself can still be retained.

`--encoding gb18030`, or `COMPILEDB_ENCODING=gb18030`, controls decoding when
forwarding wrapped real Make output. Discovery still parses Make stdout only,
and diagnostics never contaminate JSON stdout.

## Security

A build log is not necessarily passive data. Backtick expressions in relevant
compiler candidates and commands used for directory-state tracking, including
simple `cd`, `make -C`, and discovery `mkdir` commands, are executed by an
embedded POSIX shell interpreter. External programs are resolved through
`PATH`. Response-file arguments can also read local files.

Do not parse untrusted build logs.

Recoverable parser errors are written to stderr, skip the affected command,
and allow parsing to continue. Context cancellation and Make process failures
still return a nonzero status.

## Platform Support

The release workflow builds artifacts for:

- Linux amd64.
- Linux arm64.
- Windows amd64.
- macOS arm64.

On Windows, Make mode requires a GNU Make-compatible executable and
POSIX/MSYS-style recipe output, normally through MSYS2 or Git Bash. Select
`gmake` or `mingw32-make` when necessary:

```sh
compiledb make --cmd mingw32-make
```

Native `cmd.exe` recipe syntax and `nmake` are not supported. Windows
compilation database paths and `command` quoting are supported, but CL-mode
response files remain opaque.

## Per-Line and Response-File Limits

The parser applies fixed limits to individual physical lines and response-file
expansion:

- A physical build-log line is limited to 100 MiB.
- One response file is limited to 8 MiB.
- Aggregate response-file input for one compiler command is limited to 32 MiB.
- Response-file nesting is limited to 32 levels and 256 files.
- Expanded compiler arguments are limited to 100000 elements.
- Expanded arguments and their copies across multi-source entries have a
  64 MiB budget.

These are not whole-process resource limits. The total build-log size and the
output produced by programs in backtick expressions do not have fixed limits;
large inputs or unbounded command output can consume substantial memory.

## Related Projects

- [nickdiego/compiledb][python-compiledb] is the Python project that originally
  inspired this Go rewrite.
- [clangd][clangd] and [clang-tidy][clang-tidy] are common consumers of the
  generated `compile_commands.json` file.

## Development

Run the full local verification suite with:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go test ./...
go test -race ./...
go build -o /tmp/compiledb-go ./cmd/compiledb
```

Some tests modify the current directory, standard streams, and process-global
state, so those tests must not run in parallel.

## License

[GNU General Public License v3.0](LICENSE).

[compdb]: https://clang.llvm.org/docs/JSONCompilationDatabase.html
[clang-tidy]: https://clang.llvm.org/extra/clang-tidy/
[clangd]: https://clangd.llvm.org/
[python-compiledb]: https://github.com/nickdiego/compiledb
[releases]: https://github.com/fcying/compiledb-go/releases
