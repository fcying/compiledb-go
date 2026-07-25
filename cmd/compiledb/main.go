package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fcying/compiledb-go/internal"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

var Version string = "v1.6.2"

const encodingEnvVar = "COMPILEDB_ENCODING"

func resolveEncoding(ctx *cli.Context) (string, error) {
	value := ctx.String("encoding")
	if !ctx.IsSet("encoding") {
		if envValue, ok := os.LookupEnv(encodingEnvVar); ok {
			value = strings.TrimSpace(envValue)
		}
	}

	return internal.NormalizeEncoding(value)
}

func createConfig(ctx *cli.Context) (internal.Config, error) {
	outputFile := ctx.String("output")
	if outputFile != "-" && !internal.IsAbsPath(outputFile) {
		cwd, _ := os.Getwd()
		outputFile = filepath.Join(cwd, outputFile)
	}

	encoding, err := resolveEncoding(ctx)
	if err != nil {
		return internal.Config{}, err
	}

	cfg := internal.Config{
		InputFile:    ctx.String("parse"),
		OutputFile:   outputFile,
		BuildDir:     ctx.String("build-dir"),
		Exclude:      ctx.String("exclude"),
		Macros:       ctx.StringSlice("macros"),
		RegexCompile: ctx.String("regex-compile"),
		RegexFile:    ctx.String("regex-file"),
		Encoding:     encoding,
		NoBuild:      ctx.Bool("no-build"),
		CommandStyle: ctx.Bool("command-style"),
		NoStrict:     ctx.Bool("no-strict"),
		FullPath:     ctx.Bool("full-path"),
	}

	if cfg.BuildDir != "" {
		if err := os.Chdir(cfg.BuildDir); err != nil {
			return cfg, fmt.Errorf("change build-dir to %q: %w", cfg.BuildDir, err)
		}
	}

	return cfg, nil
}

type ActionFunc func(t *internal.Tool, ctx *cli.Context) error

func execute(ctx *cli.Context, fn ActionFunc) error {
	logger := log.New()
	logger.SetOutput(os.Stdout)

	if ctx.Bool("verbose") {
		logger.SetLevel(log.DebugLevel)
		logger.Info("compiledb-go start, version:", Version)
	} else {
		logger.SetLevel(log.WarnLevel)
	}

	cfg, err := createConfig(ctx)
	if err != nil {
		return err
	}
	logger.Debugf("Options: %+v", cfg)

	tool := internal.NewTool(cfg, logger)

	err = fn(tool, ctx)

	if tool.StatusCode != 0 {
		os.Exit(tool.StatusCode)
	}

	return err
}

func newApp() *cli.App {
	cli.AppHelpTemplate = `{{.HelpName}} {{.Version}}

USAGE: {{.Name}} {{if .VisibleFlags}}[options]{{end}}{{if .Commands}} command [command options]{{end}} {{if .ArgsUsage}}{{.ArgsUsage}}{{else}}[args]...
{{end}}
{{.Description}}
{{if .VisibleFlags}}
OPTIONS:
   {{range .VisibleFlags}}{{.}}
   {{end}}{{end}}{{if .Commands}}
COMMANDS:
{{range .Commands}}{{if not .HideHelp}}   {{join .Names ", "}}{{ "\t"}}{{.Usage}}{{ "\n" }}{{end}}{{end}}{{end}}
`
	app := &cli.App{
		// Compiled:             time.Now()
		EnableBashCompletion:   true,
		Version:                Version,
		UseShortOptionHandling: true,
		HideHelpCommand:        true,
		HideVersion:            true,
		Name:                   "compiledb",
		HelpName:               "compiledb-go",
		Description: "\tClang's Compilation Database generator for make-based build systems." +
			"\n\tWhen no subcommand is used it will parse build log/commands and generates" +
			"\n\tits corresponding Compilation datAbase.",
		Action: func(ctx *cli.Context) error {
			return execute(ctx, func(t *internal.Tool, c *cli.Context) error {
				t.Generate()
				t.Logger.Debugf("Done")
				return nil
			})
		},
		Commands: []*cli.Command{{
			Name:            "make",
			Usage:           "Generates compilation database file for an arbitrary GNU Make...",
			SkipFlagParsing: true,
			Action: func(ctx *cli.Context) error {
				return execute(ctx, func(t *internal.Tool, c *cli.Context) error {
					t.MakeWrap(ctx.Args().Slice())
					return nil
				})
			},
		}},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "parse", Aliases: []string{"p"}, Usage: "Build log `file` to parse compilation commands.", Value: "stdin"},
			&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "Output `file`, Use '-' to output to stdout", Value: "compile_commands.json"},
			&cli.StringFlag{Name: "build-dir", Aliases: []string{"d"}, Usage: "`Path` to be used as initial build dir."},
			&cli.StringFlag{Name: "exclude", Aliases: []string{"e"}, Usage: "Regular expressions to exclude files"},
			&cli.StringFlag{Name: "encoding", Usage: "Encoding used when printing wrapped make output: raw or gb18030 (or set COMPILEDB_ENCODING)", Value: internal.EncodingRaw},
			&cli.BoolFlag{Name: "no-build", Aliases: []string{"n"}, Usage: "Only generates compilation db file", DisableDefaultText: true},
			&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "Print verbose messages.", DisableDefaultText: true},
			&cli.BoolFlag{Name: "no-strict", Aliases: []string{"S"}, Usage: "Do not check if source files exist in the file system.", DisableDefaultText: true},
			&cli.StringSliceFlag{Name: "macros", Aliases: []string{"m"}, Usage: "Add predefined compiler macros to the compilation database (repeat flag for multiple entries)."},
			&cli.BoolFlag{Name: "command-style", Aliases: []string{"c"}, Usage: `Output compilation database with single "command" string rather than the default "arguments" list of strings.`, DisableDefaultText: true},
			&cli.BoolFlag{Name: "full-path", Usage: "Write full path to the compiler executable.", DisableDefaultText: true},
			&cli.StringFlag{Name: "regex-compile", Usage: "Regular expressions to find compile", Value: internal.RegexCompile, DefaultText: internal.RegexCompile},
			&cli.StringFlag{Name: "regex-file", Usage: "Regular expressions to find file", Value: internal.RegexFile, DefaultText: internal.RegexFile},
		},
	}
	return app
}

func main() {
	app := newApp()
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
