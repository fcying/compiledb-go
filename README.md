# Compilation Database Generator

[nickdiego/compiledb](https://github.com/nickdiego/compiledb) go version.

Tool for generating [Clang's JSON Compilation Database][compdb] file for GNU
`make`-based build systems.

It's aimed mainly at non-cmake (cmake already generates compilation database)
large codebases. Inspired by projects like [YCM-Generator][ycm-gen] and [Bear][bear],
but faster (mainly with large projects), since in most cases it **doesn't need a clean
build** (as the mentioned tools do) to generate the compilation database file, to
achieve this it uses the make options such as `-n`/`--dry-run` and `-k`/`--keep-going`
to extract the compile commands. Also, it's more **cross-compiling friendly** than
YCM-generator's fake-toolchanin approach.

## Installation

```
# go install github.com/fcying/compiledb-go/cmd/compiledb@latest

## Usage

`compiledb` provides a `make` python wrapper script which, besides to execute the make
build command, updates the JSON compilation database file corresponding to that build,
resulting in a command-line interface similar to [Bear][bear].

To generate `compile_commands.json` file using compiledb's "make wrapper" script,
executing Makefile target `all`:
```bash
$ compiledb make
```

`compiledb` forwards all the options/arguments passed after `make` subcommand to GNU Make,
so one can, for example, generate `compile_commands.json` using `core/main.mk`
as main makefile (`-f` flag), starting the build from `build` directory (`-C` flag):
```bash
$ compiledb make -f core/main.mk -C build
```

By default, `compiledb make` generates the compilation database and runs the actual build
command requested (acting as a make wrapper), the build step can be skipped using the `-n`
or `--no-build` options.
```bash
$ compiledb -n make
```

`compiledb` base command has been designed so that it can be used to parse compile commands
from arbitrary text files (or stdin), assuming it has a build log (ideally generated using
`make -Bnwk` command), and generates the corresponding JSON Compilation database.

For example, to generate the compilation database from `build-log.txt` file, use the following
command.
```bash
$ compiledb --parse build-log.txt
```

or its equivalent:
```bash
$ compiledb < build-log.txt
```

Or even, to pipe make's output and print the compilation database to the standard output:
```bash
$ make -Bnwk | compiledb -o-
```

By default `compiledb` generates a JSON compilation database in the "arguments" list
[format](https://clang.llvm.org/docs/JSONCompilationDatabase.html). The "command" string
format is also supported through the use of the `--command-style` flag:
```bash
$ compiledb --command-style make
```

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
