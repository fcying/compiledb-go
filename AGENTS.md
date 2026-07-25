# Repository Guide

## Scope And Layout

- This repository is one Go module (`go 1.23.1`) containing one CLI.
- `cmd/compiledb/main.go` owns CLI flags, config construction, `--build-dir` handling, logger setup, versioning, and process exit status.
- `internal/parser.go` parses build logs and writes compilation entries. `internal/make_wrap.go` wraps GNU Make. `internal/util.go` owns path, encoding, stream, and shell-argument helpers.
- Keep changes surgical. Do not redesign parser regexes, command output shape, exit semantics, or Make wrapping while fixing an unrelated issue.

## Parser And Make Wrapper Contracts

- Parser behavior depends on working-directory tracking. It follows Make enter/leave messages, `make -C`, and inline `cd`. Parsing a file starts relative to that log file's directory; stdin starts at the current working directory.
- Strict mode is the default: a parsed source is omitted unless it exists relative to the tracked directory. Tests commonly use `NoStrict: true` because `tests/build.log` contains `/opt/compiledb_test/...` paths.
- The default file regex recognizes compile-only commands containing `-c`. A compiler command containing source files but no `-c` currently emits an error, generates no entry, and does not by itself change the exit status. Preserve all three behaviors unless the task explicitly changes support for that command form.
- `compiledb make` runs `make -Bnkw` for command discovery and, unless `--no-build` is set, runs the requested real Make command concurrently. The parser consumes dry-run output; real Make stdout/stderr is forwarded to the caller.
- Real Make failure takes precedence over dry-run failure. If the real build succeeds but the dry run fails, the dry-run status is returned. Under `--no-build`, dry-run failure is returned directly.
- Backtick expressions in build-log lines are executed through `sh -c` before command extraction. Do not feed untrusted logs to the parser. Preserve or explicitly test this behavior when changing parser flow.
- Compiler arguments are parsed with `go-shellwords`. Preserve quoting, escaped paths, command-style output, and repeated macro arguments.

## Logging And Output

- The CLI explicitly routes its logrus logger to stdout, not stderr. Do not assume `Error`, `Warn`, or `Info` logs appear on stderr.
- `--output -` writes JSON to stdout. Any new visible diagnostic can share stdout with that JSON, so add an explicit CLI test before introducing or changing non-debug logs.
- During a normal `compiledb make`, parser logging is temporarily restricted while real Make output is streaming, then restored. Verify diagnostics through both direct parsing and the Make wrapper path.

## Verification

- Full local verification:
  1. `gofmt -l .` must print nothing.
  2. `go vet ./...`
  3. `go test ./...`
  4. `go test -race ./...` for parser, wrapper, CLI, logging, or process-global changes.
- Focus a test with `go test ./internal -run '^TestName$'` or `go test ./cmd/compiledb -run '^TestName$'`.
- Build without leaving a repository artifact using `go build -o /tmp/compiledb-go ./cmd/compiledb`. The root `compiledb` binary is ignored, but `/tmp` is preferred for manual verification.
- Do not treat `just` as a normal build check. `.justfile` requires Nushell, and its default `build` recipe runs the CLI against `tests/build.log`, generating `compile_commands.json` rather than only compiling the binary.
- The release workflow builds artifacts but does not run tests, vet, or race checks. Local verification remains required.

## Test And Artifact Gotchas

- Tests mutate process globals including `os.Args`, the current directory, stdin/stdout, and `internal.makePath`. Do not add `t.Parallel()` around such tests until that state is isolated.
- Tests that replace cwd, stdio, environment variables, logger output, or `makePath` must restore them with `t.Cleanup` or `defer`.
- `TestParser` writes ignored `cmd/compiledb/compile_commands.json`. It is a generated test artifact, not source or a golden file.
- Root `compiledb`, `compile_commands.json`, and `*.exe` are ignored artifacts. Do not stage generated binaries or compilation databases.
- When changing parser diagnostics, cover at least: the triggering compiler command, a standard `-c` command, a link-only command, unrelated shell output, direct `Parse`, and `MakeWrap`.

## Release Gotchas

- `Version` in `cmd/compiledb/main.go` is parsed directly by `.github/workflows/release.yml`. On `main`, a version different from the latest tag triggers a tagged release; non-release builds rewrite it to a `dev-...` version during CI.
- Do not change `Version` as part of unrelated work. When changing it intentionally, review the release workflow and tag state together.
- The release workflow runs `go mod tidy` before building. Dependency changes must leave `go.mod` and `go.sum` tidy locally rather than relying on CI to rewrite them.
