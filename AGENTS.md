# Repository Guide

## Scope And Layout

- This repository is one Go module (`go 1.23.1`) containing one CLI.
- `cmd/compiledb/main.go` owns CLI flags, config construction, `--build-dir` handling, logger setup, versioning, and process exit status.
- `internal/parser.go` parses build logs and writes compilation entries. `internal/make_wrap.go` wraps GNU Make. `internal/util.go` owns path, encoding, stream, and shell-argument helpers.
- Keep changes surgical. Do not redesign parser regexes, command output shape, exit semantics, or Make wrapping while fixing an unrelated issue.

## Parser And Make Wrapper Contracts

- Parser behavior depends on working-directory tracking. It follows Make enter/leave messages, `make -C`, and inline `cd`. Parsing a file starts relative to that log file's directory; `--parse -` reads stdin and starts at the current working directory.
- Strict mode is the default: a parsed source is omitted unless it resolves to a regular file relative to the tracked directory. Symlinks to regular files are accepted. Tests commonly use `NoStrict: true` because `tests/build.log` contains `/opt/compiledb_test/...` paths.
- The default option-aware scanner records actual compiler-driver commands containing source inputs even without `-c`. Multi-source commands generate one entry per source; link-only commands and noncompiler tools such as `clang-format`/`clang-tidy` remain omitted.
- In `--no-strict` mode, a simple inline `cd` updates the tracked directory without consulting the local filesystem. This supports logs captured on another machine; strict mode validates that the directory exists before following a conditional branch.
- `compiledb make` first runs the requested real Make command and only runs `make -Bnkw` discovery after it succeeds; `--no-build` runs discovery directly. The parser consumes discovery output; real Make stdout/stderr is forwarded to the caller.
- `compiledb make --cmd/-c COMMAND` selects the GNU Make-compatible executable for both real and discovery invocations. Before the first `--`, the subcommand consumes this option and `--help/-h` with the original Click short-cluster semantics; the first `--` is a wrapper delimiter and is not forwarded, while all following arguments are. Other Make arguments preserve their order and value outside recognized short-option clusters. Top-level `--command-style/-c` is parsed before `make` and remains a separate option.
- Real Make failure takes precedence over dry-run failure. If the real build succeeds but the dry run fails, the dry-run status is returned. Under `--no-build`, dry-run failure is returned directly. A failed discovery never creates or updates the database, even when it produced partial stdout.
- Discovery parsing consumes stdout only. Discovery stderr is forwarded to stderr as user-visible diagnostics and must never be parsed as compiler commands or written to JSON stdout.
- Discovery-only recursive `$(MAKE)` calls use an internal proxy that removes child `--no-print-directory`, requests `--print-directory`, and delegates to the same resolved Make executable. The real build is never proxied. Explicit `MAKE` selection and `-e/--environment-overrides` retain user semantics. Hard-coded Make paths, wrappers that replace themselves with a hard-coded Make, Makefile `override MAKEFLAGS` that disables markers, and direct build-log parsing remain outside its scope.
- Backtick expressions in build-log lines are executed by an embedded POSIX shell interpreter before command extraction. Do not feed untrusted logs to the parser. Preserve or explicitly test this behavior when changing parser flow.
- Shell builtins in backtick expressions do not require an external shell executable. Programs explicitly invoked by the expression must still be available through `PATH`; execution failure emits a recoverable Error, skips only that command, and keeps status 0.
- On Windows, the supported wrapper/parser combination is a GNU Make-compatible executable with POSIX/MSYS-style recipe output. `--cmd/-c` can select `gmake` or `mingw32-make`; native `cmd.exe` recipe grammar and `nmake` are out of scope.
- Compiler arguments are parsed with the repository's POSIX-like tokenizer. Preserve quoting, escaped paths, command-style output, and repeated macro arguments.
- GCC and Clang GNU-mode response files are expanded relative to the compiler process cwd before normalization and source scanning. Nested files use the same cwd, output is flattened, and malformed, missing, recursive, non-regular, unsupported-encoding, or over-limit files skip only that compiler command with a recoverable Error. CL-mode response files remain opaque.
- Relevant malformed compiler, `cd`, and Make commands emit a recoverable Error diagnostic containing the build-log line, tracked cwd, reason, and byte offset without the full command. Unrelated malformed output remains silent; parser status stays successful unless canceled.
- Complex shell groups, functions, and control structures are skipped as a whole. Backslash-newline continuation follows shell joining semantics, does not insert whitespace, and drops malformed or unterminated fragments without changing the successful parser status.
- A top-level redirected simple command that cannot change tracked state is skipped without discarding independent `;` siblings. Redirected state-changing commands and redirections inside conditionals or complex structures remain fail closed for the logical line.

## Logging And Output

- The CLI explicitly routes its logrus logger and fatal diagnostics to stderr. Non-verbose mode uses `ErrorLevel`; verbose mode uses `DebugLevel`.
- Make output drain timeouts preserve a successful Make exit status, but the incomplete-output diagnostic must remain visible at `ErrorLevel`.
- `--output -` writes JSON to stdout. Keep all diagnostics on stderr and add an explicit CLI test before introducing or changing non-debug logs.
- File and stdout compilation database output both end with one newline.
- During a normal `compiledb make`, discovery parser logging uses an independent logger restricted to ErrorLevel. Verify diagnostics through both direct parsing and the Make wrapper path.

## Verification

- Full local verification:
  1. `gofmt -l .` must print nothing.
  2. `go vet ./...`
  3. `go test ./...`
  4. `go test -race ./...` for parser, wrapper, CLI, logging, or process-global changes.
- Focus a test with `go test ./internal -run '^TestName$'` or `go test ./cmd/compiledb -run '^TestName$'`.
- Build without leaving a repository artifact using `go build -o /tmp/compiledb-go ./cmd/compiledb`. The root `compiledb` binary is ignored, but `/tmp` is preferred for manual verification.
- Do not treat `just` as a normal build check. `.justfile` requires Nushell, and its default `build` recipe runs the CLI against `tests/build.log`, generating `compile_commands.json` rather than only compiling the binary.
- The release workflow gates Linux amd64 on gofmt, vet, tests, and race tests. Windows amd64 also runs the native Make proxy path regression. Linux amd64/arm64, Windows amd64, and macOS arm64 artifacts are built natively and must run `--help` before publication. Local verification remains required.

## Test And Artifact Gotchas

- Tests mutate process globals including `os.Args`, the current directory, stdin/stdout, and `internal.makePath`. Do not add `t.Parallel()` around such tests until that state is isolated.
- Tests that replace cwd, stdio, environment variables, logger output, or `makePath` must restore them with `t.Cleanup` or `defer`.
- `TestParser` writes ignored `cmd/compiledb/compile_commands.json`. It is a generated test artifact, not source or a golden file.
- Root `compiledb`, `compile_commands.json`, and `*.exe` are ignored artifacts. Do not stage generated binaries or compilation databases.
- When changing parser diagnostics, cover at least: the triggering compiler command, a standard `-c` command, a link-only command, unrelated shell output, direct `Parse`, and `MakeWrap`.

## Release Gotchas

- `Version` in `cmd/compiledb/main.go` is parsed directly by `.github/workflows/release.yml`. On `main`, a version different from the latest tag triggers a tagged release; CI injects `dev-...` for non-release artifacts through a linker flag.
- Do not change `Version` as part of unrelated work. When changing it intentionally, review the release workflow and tag state together.
- The Linux release gate runs `go mod tidy` and rejects changes to `go.mod` or `go.sum`. Dependency changes must leave both files tidy locally rather than relying on CI to rewrite them.
