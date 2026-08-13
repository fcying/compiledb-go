# Compilation Database Generator  

rewrite [nickdiego/compiledb](https://github.com/nickdiego/compiledb) in Go for speed.  
test using a build log over 2MB size and over 200 valid entries
```
# make -Bnkw > build.log

compiledb-go
# time ~/go/bin/compiledb -p build.log
~/go/bin/compiledb -p build.log  0.21s user 0.02s system 106% cpu 0.210 total

compiledb-python
# time ~/.local/bin/compiledb -p build.log
~/.local/bin/compiledb -p build.log  5.21s user 0.01s system 100% cpu 5.179 total
```

Tool for generating [Clang's JSON Compilation Database][compdb] file for GNU
`make`-based build systems.

It's aimed mainly at non-cmake (cmake already generates compilation database)
large codebases. Inspired by projects like [YCM-Generator][ycm-gen] and [Bear][bear],
but faster (mainly with large projects), since in most cases it **doesn't need a clean
build** (as YCM-Generator does) to generate the compilation database file, to
achieve this it uses the make options such as `--dry-run/-n` and `--keep-going/-k`
to extract the compile commands. Also, it's more **cross-compiling friendly** than
YCM-generator's fake-toolchanin approach.

## Installation

```
# go install github.com/fcying/compiledb-go/cmd/compiledb@latest

# go build ./cmd/compiledb && go install ./cmd/compiledb
```

## Usage
```
compiledb-go

USAGE: compiledb [options] command [command options] [args]...

  Clang's Compilation Database generator for make-based build systems.
  When no subcommand is used it will parse build log/commands and generates
  its corresponding Compilation datAbase.

OPTIONS:
   --parse/-p file         Build log file to parse compilation commands, or '-' for stdin. (default: "-")
   --output/-o file        Output file, Use '-' to output to stdout (default: "compile_commands.json")
   --overwrite/-f          Overwrite compile_commands.json instead of just updating it.
   --build-dir/-d Path     Path to be used as initial build dir.
   --exclude/-e value      Regular expression matched from the start of the source path (repeat for multiple expressions).
   --encoding value        Encoding used when printing wrapped make output: raw or gb18030 (default: "raw", env: COMPILEDB_ENCODING)
   --no-build/-n           Only generates compilation db file.
   --verbose/-v            Print verbose messages.
   --no-strict/-S          Do not check if source files exist in the file system.
   --macros/-m             Add predefined compiler macros to the compilation database. Compilers must be available from their working directory or PATH.
   --add-arg/-a value      Add an argument to each compiler command (repeat flag for multiple arguments).
   --command-style/-c      Output compilation database with single "command" string rather than the default "arguments" list of strings.
   --full-path             Write full path to the compiler executable.
   --regex-compile value   Regular expressions to find compile (default: (?i)^(?:.*[/\\])?(?:[A-Za-z0-9_.+]+-)*(?:(?:gcc|g\+\+)(?:(?:-?[0-9]+(?:\.[0-9]+)*(?:-(?:posix|win32))?)|-(?:posix|win32)|-mp-[0-9]+(?:\.[0-9]+)*)?|clang(?:\+\+|-cl)?(?:-[0-9]+(?:\.[0-9]+)*)?|(?:cc|c\+\+)(?:-[0-9]+(?:\.[0-9]+)*)?)(?:\.exe)?$)
   --regex-file value      Regular expressions to find file (default: ^.*\s+-c.*\s(?:(?:"|')(.*?\.(?i:c|cpp|cc|cxx|c\+\+|s|m|mm|cu))(?:"|')|([^\s"']+\.(?i:c|cpp|cc|cxx|c\+\+|s|m|mm|cu)))(\s|$))
   --help/-h               show help
   
COMMANDS:
   make                     Generates compilation database file for an arbitrary GNU Make...
      --cmd/-c value        Command to be used as make executable.
```

### Make Wrapper

Run Make and update `compile_commands.json` after a successful build:

```bash
$ compiledb make
```

Arguments after `make` are forwarded to GNU Make. Use `make --cmd/-c` to select another compatible
executable, and `make --` when a Make argument must bypass wrapper option parsing:

```bash
$ compiledb make --cmd gmake -f core/main.mk -C build
```

The top-level `--command-style/-c` is separate from `make --cmd/-c`. Use `--no-build/-n` to skip
the real build and run discovery only:

```bash
$ compiledb -n make
```

Only successful discovery output updates the database. Make output and diagnostics go to stderr
when `--output -` is used, keeping stdout valid JSON.

### Parse A Build Log

Parse a saved build log, read stdin, or pipe Make output directly:

```bash
$ compiledb --parse build-log.txt
$ compiledb < build-log.txt
$ make -Bnwk | compiledb --output -
```

The initial working directory is selected in this order:

1. An explicit `--build-dir/-d` always wins.
2. When parsing a build-log file, its containing directory is used by default.
3. When parsing stdin, the directory where `compiledb` was started is used.

```bash
$ compiledb --build-dir /path/to/project/build --parse /tmp/build-log.txt
```

Backtick expressions are executed by an embedded POSIX interpreter. Their output is used as
argument data, but explicitly invoked programs must still be available through `PATH`. Do not parse
untrusted build logs.

GCC and Clang GNU-style `@response-file` arguments are expanded relative to the compiler working
directory. Nested response files are supported, and the emitted database contains flattened
arguments so it does not depend on the files afterward. Parsing uses a conservative common GNU
syntax and rejects malformed, recursive, non-regular, non-UTF-8, or oversized files; only the
affected compiler command is skipped. `clang-cl`, Clang CL driver mode, Windows response quoting,
and response-file discovery through custom file regexes are not supported.

### Output Options

By default, new entries update the existing database. Use `--overwrite/-f` to replace it:

```bash
$ compiledb --overwrite make
```

Useful output options:

```bash
$ compiledb --command-style make
$ compiledb --add-arg='-DCSV=a,b' -a=-m32 make
$ compiledb --macros make
$ PATH=/opt/buildroot/bin:$PATH compiledb --full-path make
$ compiledb --encoding gb18030 make
```

`--macros/-m` requires the compiler to be available from its working directory or `PATH`; unsafe or
unsupported probes are skipped. Repeat `--exclude/-e` and `--add-arg/-a` as needed.

### Memory Use

Build-log processing is buffered and uses `O(build-log size + entries)` memory. Each physical line
is limited to 100 MiB. In synthetic Linux amd64 tests, direct parsing peaked at about 311 MiB/1.53
GiB RSS for 100 MiB/500 MiB logs; Make discovery peaked at about 490 MiB/1.95 GiB.
Each response file is limited to 8 MiB, with per-command limits on recursion, aggregate input,
argument count, and flattened output size.

### Windows Support

Use a GNU Make-compatible executable and POSIX/MSYS recipe output, typically through MSYS2 or Git
Bash. Select `gmake` or `mingw32-make` with `make --cmd` when necessary. Native `cmd.exe` recipe
syntax and `nmake` are not supported.

## Testing / Contributing

I've implemented this tool because I needed to index some [AOSP][aosp]'s modules for navigating
and studying purposes (after having no satisfatory results with current tools available by the
time such as [YCM-Generator][ycm] and [Bear][bear]). So I've reworked YCM-Generator, which resulted
in the initial version of [compiledb/parser.py](compiledb/parser.py) and used successfully to generate
`compile_commands.json` for some AOSP modules in ~1min running in a [Docker][docker] container and then
could use it with some great tools, such as:

- [Vim][vim] + [YouCompleteMe][ycm] + [rtags][rtags] + [chromatica.nvim][chrom]
- [Neovim][neovim] + [LanguageClient-neovim][lsp] + [cquery][cquery] + [deoplete][deoplete]
- [Neovim][neovim] + [ALE][ale] + [ccls][ccls]

## License
GNU GPLv3

[compdb]: https://clang.llvm.org/docs/JSONCompilationDatabase.html
[ycm]: https://github.com/Valloric/YouCompleteMe
[rtags]: https://github.com/Andersbakken/rtags
[chrom]: https://github.com/arakashic/chromatica.nvim
[ycm-gen]: https://github.com/rdnetto/YCM-Generator
[bear]: https://github.com/rizsotto/Bear
[aosp]: https://source.android.com/
[docker]: https://www.docker.com/
[vim]: https://www.vim.org/
[neovim]: https://neovim.io/
[lsp]: https://github.com/autozimu/LanguageClient-neovim
[cquery]: https://github.com/cquery-project/cquery
[deoplete]: https://github.com/Shougo/deoplete.nvim
[ccls]: https://github.com/MaskRay/ccls
[ale]: https://github.com/w0rp/ale
