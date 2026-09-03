package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/fcying/compiledb-go/internal"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

var Version = "v1.7.1"

const encodingEnvVar = "COMPILEDB_ENCODING"

func displayVersion() string {
	var settings []debug.BuildSetting
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
	}
	return formatVersion(Version, settings)
}

func formatVersion(version string, settings []debug.BuildSetting) string {
	revision := ""
	modified := false
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return version
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		revision += "-dirty"
	}
	return fmt.Sprintf("%s (%s)", version, revision)
}

type compilerArguments []string

func (a *compilerArguments) Set(value string) error {
	*a = append(*a, value)
	return nil
}

func (a *compilerArguments) String() string {
	return strings.Join(*a, ", ")
}

func addArguments(ctx *cli.Context) []string {
	arguments, ok := ctx.Generic("add-arg").(*compilerArguments)
	if !ok {
		return nil
	}
	return append([]string(nil), (*arguments)...)
}

type excludePatterns []string

func (p *excludePatterns) Set(value string) error {
	*p = append(*p, value)
	return nil
}

func (p *excludePatterns) String() string {
	return strings.Join(*p, ", ")
}

func exclusions(ctx *cli.Context) []string {
	patterns, ok := ctx.Generic("exclude").(*excludePatterns)
	if !ok {
		return nil
	}
	return append([]string(nil), (*patterns)...)
}

type compiledbApp struct {
	*cli.App
	addArgs  *compilerArguments
	excludes *excludePatterns
}

func (a *compiledbApp) Run(arguments []string) error {
	ctx, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	signalDone := make(chan struct{})
	go func() {
		defer close(signalDone)
		select {
		case received := <-signals:
			cancel(internal.SignalError{ProcessSignal: received})
			select {
			case received = <-signals:
				os.Exit(internal.SignalExitCode(received))
			case <-done:
			}
		case <-done:
		}
	}()
	err := a.RunContext(ctx, arguments)
	close(done)
	signal.Stop(signals)
	<-signalDone
	cancel(context.Canceled)
	return err
}

func (a *compiledbApp) RunContext(ctx context.Context, arguments []string) error {
	*a.addArgs = nil
	*a.excludes = nil
	return a.App.RunContext(ctx, arguments)
}

func resolveEncoding(ctx *cli.Context) (string, error) {
	value := ctx.String("encoding")
	if !ctx.IsSet("encoding") {
		if envValue, ok := os.LookupEnv(encodingEnvVar); ok {
			value = strings.TrimSpace(envValue)
		}
	}

	return internal.NormalizeEncoding(value)
}

func createConfig(ctx *cli.Context, validateEncoding bool) (internal.Config, error) {
	addArgs := addArguments(ctx)
	outputFile := ctx.String("output")
	if outputFile != "-" && !internal.HasPathRootOrVolumePrefix(outputFile) {
		cwd, _ := os.Getwd()
		outputFile = filepath.Join(cwd, outputFile)
	}

	encoding := internal.EncodingRaw
	var err error
	if validateEncoding {
		encoding, err = resolveEncoding(ctx)
		if err != nil {
			return internal.Config{}, err
		}
	}
	buildDir := ctx.String("build-dir")
	if buildDir != "" {
		buildDir, err = filepath.Abs(buildDir)
		if err != nil {
			return internal.Config{}, fmt.Errorf("resolve build-dir %q: %w", ctx.String("build-dir"), err)
		}
	}
	inputFile := ctx.String("parse")
	if inputFile != "-" && !internal.HasPathRootOrVolumePrefix(inputFile) {
		if buildDir != "" {
			inputFile = filepath.Join(buildDir, inputFile)
		} else {
			inputFile, err = filepath.Abs(inputFile)
			if err != nil {
				return internal.Config{}, fmt.Errorf("resolve input file %q: %w", ctx.String("parse"), err)
			}
		}
	}

	cfg := internal.Config{
		InputFile:    inputFile,
		OutputFile:   outputFile,
		BuildDir:     buildDir,
		Exclude:      exclusions(ctx),
		AddArgs:      addArgs,
		RegexCompile: ctx.String("regex-compile"),
		RegexFile:    ctx.String("regex-file"),
		Encoding:     encoding,
		Macros:       ctx.Bool("macros"),
		NoBuild:      ctx.Bool("no-build"),
		CommandStyle: ctx.Bool("command-style"),
		NoStrict:     ctx.Bool("no-strict"),
		FullPath:     ctx.Bool("full-path"),
		Overwrite:    ctx.Bool("overwrite"),
	}

	if cfg.BuildDir != "" {
		info, err := os.Stat(cfg.BuildDir)
		if err != nil {
			return cfg, fmt.Errorf("access build-dir %q: %w", cfg.BuildDir, err)
		}
		if !info.IsDir() {
			return cfg, fmt.Errorf("build-dir %q is not a directory", cfg.BuildDir)
		}
	}

	return cfg, nil
}

type ActionFunc func(t *internal.Tool, ctx *cli.Context) error

func execute(ctx *cli.Context, validateEncoding bool, fn ActionFunc) error {
	logger := log.New()
	logger.SetOutput(os.Stderr)

	if ctx.Bool("verbose") {
		logger.SetLevel(log.DebugLevel)
		logger.Info("compiledb-go start, version:", displayVersion())
	} else {
		logger.SetLevel(log.ErrorLevel)
	}

	cfg, err := createConfig(ctx, validateEncoding)
	if err != nil {
		return err
	}
	logger.Debugf("Options: %+v", cfg)

	tool := internal.NewTool(cfg, logger)
	tool.Context = ctx.Context

	err = fn(tool, ctx)

	if tool.StatusCode != 0 {
		os.Exit(tool.StatusCode)
	}

	return err
}

func showUsageError(ctx *cli.Context, err error, isSubcommand bool) error {
	output := ctx.App.Writer
	ctx.App.Writer = ctx.App.ErrWriter
	defer func() { ctx.App.Writer = output }()

	_, _ = fmt.Fprintf(ctx.App.Writer, "Incorrect Usage: %s\n\n", err)
	if isSubcommand {
		_ = cli.ShowSubcommandHelp(ctx)
	} else {
		_ = cli.ShowAppHelp(ctx)
	}
	return cli.Exit("", 2)
}

func parseMakeArguments(arguments []string) (string, []string, bool, error) {
	makeCommand := ""
	makeArguments := make([]string, 0, len(arguments))
	showHelp := false
	options := true
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if options && argument == "--" {
			options = false
			continue
		}
		if !options {
			makeArguments = append(makeArguments, argument)
			continue
		}

		switch {
		case argument == "--help":
			showHelp = true
		case strings.HasPrefix(argument, "--help="):
			return "", nil, false, fmt.Errorf("Option '--help' does not take a value.")
		case argument == "--cmd":
			if index+1 >= len(arguments) {
				return "", nil, false, fmt.Errorf("Option '--cmd' requires an argument.")
			}
			index++
			makeCommand = arguments[index]
		case strings.HasPrefix(argument, "--cmd="):
			makeCommand = strings.TrimPrefix(argument, "--cmd=")
		case len(argument) > 1 && argument[0] == '-' && argument[1] != '-':
			unknown := make([]byte, 0, len(argument)-1)
			for position := 1; position < len(argument); position++ {
				switch argument[position] {
				case 'h':
					showHelp = true
				case 'c':
					if len(unknown) != 0 {
						makeArguments = append(makeArguments, "-"+string(unknown))
						unknown = nil
					}
					if position+1 < len(argument) {
						makeCommand = argument[position+1:]
					} else {
						if index+1 >= len(arguments) {
							return "", nil, false, fmt.Errorf("Option '-c' requires an argument.")
						}
						index++
						makeCommand = arguments[index]
					}
					position = len(argument)
				default:
					unknown = append(unknown, argument[position])
				}
			}
			if len(unknown) != 0 {
				makeArguments = append(makeArguments, "-"+string(unknown))
			}
		default:
			makeArguments = append(makeArguments, argument)
		}
	}
	return makeCommand, makeArguments, showHelp, nil
}

func newApp() *compiledbApp {
	addArgs := compilerArguments{}
	excludes := excludePatterns{}
	version := displayVersion()

	cli.AppHelpTemplate = `{{.HelpName}} {{.Version}}

USAGE: {{.Name}} {{if .VisibleFlags}}[options]{{end}}{{if .Commands}} command [command options]{{end}} {{if .ArgsUsage}}{{.ArgsUsage}}{{else}}[args]...
{{end}}
{{.Description}}
{{if .VisibleFlags}}
OPTIONS:
   {{range .VisibleFlags}}{{.}}
   {{end}}{{end}}{{if .Commands}}
COMMANDS:
{{range .Commands}}{{if not .HideHelp}}   {{join .Names ", "}}{{ "\t"}}{{.Usage}}{{ "\n" }}{{range .VisibleFlags}}      {{.}}{{ "\n" }}{{end}}{{end}}{{end}}{{end}}
`
	app := &cli.App{
		// Compiled:             time.Now()
		EnableBashCompletion:   true,
		Writer:                 os.Stdout,
		ErrWriter:              os.Stderr,
		OnUsageError:           showUsageError,
		Version:                version,
		UseShortOptionHandling: true,
		HideHelpCommand:        true,
		HideVersion:            true,
		Name:                   "compiledb",
		HelpName:               "compiledb-go",
		Description: "\tClang's Compilation Database generator for make-based build systems." +
			"\n\tWhen no subcommand is used it will parse build log/commands and generates" +
			"\n\tits corresponding Compilation datAbase.",
		Action: func(ctx *cli.Context) error {
			return execute(ctx, false, func(t *internal.Tool, c *cli.Context) error {
				t.Generate()
				t.Logger.Debugf("Done")
				return nil
			})
		},
		Commands: []*cli.Command{{
			Name:            "make",
			Usage:           "Generates compilation database file for an arbitrary GNU Make...",
			ArgsUsage:       "[MAKE_ARGS]...",
			SkipFlagParsing: true,
			HideHelpCommand: true,
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "cmd", Aliases: []string{"c"}, Usage: "Command to be used as make executable."},
			},
			Action: func(ctx *cli.Context) error {
				makeCommand, makeArguments, showHelp, err := parseMakeArguments(ctx.Args().Slice())
				if err != nil {
					_, _ = fmt.Fprintf(ctx.App.ErrWriter, "Error: %s\n", err)
					return cli.Exit("", 2)
				}
				if showHelp {
					cli.HelpPrinter(ctx.App.Writer, cli.CommandHelpTemplate, ctx.Command)
					return nil
				}
				return execute(ctx, true, func(t *internal.Tool, c *cli.Context) error {
					t.Config.MakeCommand = makeCommand
					t.MakeWrap(makeArguments)
					return nil
				})
			},
		}},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "parse", Aliases: []string{"p"}, Usage: "Build log `file` to parse compilation commands, or '-' for stdin.", Value: "-"},
			&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "Output `file`, Use '-' to output to stdout", Value: "compile_commands.json"},
			&cli.BoolFlag{Name: "overwrite", Aliases: []string{"f"}, Usage: "Overwrite compile_commands.json instead of just updating it.", DisableDefaultText: true},
			&cli.StringFlag{Name: "build-dir", Aliases: []string{"d"}, Usage: "`Path` to be used as initial build dir."},
			&cli.GenericFlag{Name: "exclude", Usage: "Regular expression matched from the start of the source path (repeat for multiple expressions).", Destination: &excludes},
			&cli.GenericFlag{Name: "e", Usage: "Alias for --exclude.", Destination: &excludes},
			&cli.StringFlag{Name: "encoding", Usage: "Encoding used when printing wrapped make output: raw or gb18030 (or set COMPILEDB_ENCODING)", Value: internal.EncodingRaw},
			&cli.BoolFlag{Name: "no-build", Aliases: []string{"n"}, Usage: "Only generates compilation db file", DisableDefaultText: true},
			&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "Print verbose messages.", DisableDefaultText: true},
			&cli.BoolFlag{Name: "no-strict", Aliases: []string{"S"}, Usage: "Do not check if source files exist in the file system.", DisableDefaultText: true},
			&cli.BoolFlag{Name: "macros", Aliases: []string{"m"}, Usage: "Add predefined compiler macros to the compilation database. Compilers must be available from their working directory or PATH.", DisableDefaultText: true},
			&cli.GenericFlag{Name: "add-arg", Usage: "Add an argument to each compiler command (repeat flag for multiple arguments).", Destination: &addArgs},
			&cli.GenericFlag{Name: "a", Usage: "Alias for --add-arg.", Destination: &addArgs},
			&cli.BoolFlag{Name: "command-style", Aliases: []string{"c"}, Usage: `Output compilation database with single "command" string rather than the default "arguments" list of strings.`, DisableDefaultText: true},
			&cli.BoolFlag{Name: "full-path", Usage: "Write full path to the compiler executable.", DisableDefaultText: true},
			&cli.StringFlag{Name: "regex-compile", Usage: "Regular expressions to find compile", Value: internal.RegexCompile, DefaultText: internal.RegexCompile},
			&cli.StringFlag{Name: "regex-file", Usage: "Regular expressions to find file", Value: internal.RegexFile, DefaultText: internal.RegexFile},
		},
	}
	return &compiledbApp{App: app, addArgs: &addArgs, excludes: &excludes}
}

func main() {
	log.SetOutput(os.Stderr)
	app := newApp()
	if err := app.Run(os.Args); err != nil {
		var exitCoder cli.ExitCoder
		if errors.As(err, &exitCoder) {
			if err.Error() != "" {
				log.Error(err)
			}
			os.Exit(exitCoder.ExitCode())
		}
		log.Fatal(err)
	}
}
