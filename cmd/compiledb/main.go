package main

import (
	"os"
	"path/filepath"

	"github.com/fcying/compiledb-go/internal"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

var Version string = "v1.5.0"

func init() {
	log.SetOutput(os.Stdout)
	// log.SetLevel(log.InfoLevel)
	log.SetLevel(log.WarnLevel)
}

func updateConfig(ctx *cli.Context) {
	outputFile := ctx.String("output")
	internal.ParseConfig.InputFile = ctx.String("parse")
	internal.ParseConfig.BuildDir = ctx.String("build-dir")
	internal.ParseConfig.Exclude = ctx.String("exclude")
	internal.ParseConfig.Macros = ctx.String("macros")
	internal.ParseConfig.RegexCompile = ctx.String("regex-compile")
	internal.ParseConfig.RegexFile = ctx.String("regex-file")
	internal.ParseConfig.NoBuild = ctx.Bool("no-build")
	internal.ParseConfig.CommandStyle = ctx.Bool("command-style")
	internal.ParseConfig.NoStrict = ctx.Bool("no-strict")
	internal.ParseConfig.FullPath = ctx.Bool("full-path")

	if internal.IsAbsPath(outputFile) == false {
		cwd, _ := os.Getwd()
		outputFile = filepath.Join(cwd, outputFile)
	}
	internal.ParseConfig.OutputFile = outputFile

	if internal.ParseConfig.BuildDir != "" {
		err := os.Chdir(internal.ParseConfig.BuildDir)
		if err != nil {
			log.Error(err)
		}
	}

	log.Debugf("Options: %+v", internal.ParseConfig)
}

func main() {
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
			updateConfig(ctx)
			internal.Generate()
			log.Debugf("Done")
			return nil
		},
		Commands: []*cli.Command{
			{
				Name:            "make",
				Usage:           "Generates compilation database file for an arbitrary GNU Make...",
				SkipFlagParsing: true,
				Action: func(ctx *cli.Context) error {
					updateConfig(ctx)
					internal.MakeWrap(ctx.Args().Slice())
					return nil
				},
			},
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "parse",
				Aliases: []string{"p"},
				Usage:   "Build log `file` to parse compilation commands.",
				Value:   "stdin",
			},
			&cli.StringFlag{
				Name:    "output",
				Aliases: []string{"o"},
				Usage:   "Output `file`, Use '-' to output to stdout",
				Value:   "compile_commands.json",
			},
			&cli.StringFlag{
				Name:    "build-dir",
				Aliases: []string{"d"},
				Usage:   "`Path` to be used as initial build dir.",
			},
			&cli.StringFlag{
				Name:    "exclude",
				Aliases: []string{"e"},
				Usage:   "Regular expressions to exclude files",
			},
			&cli.BoolFlag{
				Name:               "no-build",
				Aliases:            []string{"n"},
				Usage:              "Only generates compilation db file",
				DisableDefaultText: true,
			},
			&cli.BoolFlag{
				Name:               "verbose",
				Aliases:            []string{"v"},
				Usage:              "Print verbose messages.",
				DisableDefaultText: true,
				Action: func(*cli.Context, bool) error {
					log.SetLevel(log.DebugLevel)
					log.Info("compiledb-go start, version:", Version)
					return nil
				},
			},
			// &cli.BoolFlag{
			// 	Name:               "overwrite",
			// 	Aliases:            []string{"f"},
			// 	Usage:              "Overwrite compile_commands.json instead of just updating it.",
			// 	DisableDefaultText: true,
			// },
			&cli.BoolFlag{
				Name:               "no-strict",
				Aliases:            []string{"S"},
				Usage:              "Do not check if source files exist in the file system.",
				DisableDefaultText: true,
			},
			&cli.StringFlag{
				Name:    "macros",
				Aliases: []string{"m"},
				Usage:   "Add predefined compiler macros to the compilation database.",
			},
			&cli.BoolFlag{
				Name:               "command-style",
				Aliases:            []string{"c"},
				Usage:              `Output compilation database with single "command" string rather than the default "arguments" list of strings.`,
				DisableDefaultText: true,
			},
			&cli.BoolFlag{
				Name:               "full-path",
				Usage:              "Write full path to the compiler executable.",
				DisableDefaultText: true,
			},
			&cli.StringFlag{
				Name:        "regex-compile",
				Usage:       "Regular expressions to find compile",
				Value:       internal.RegexCompile,
				DefaultText: internal.RegexCompile,
			},
			&cli.StringFlag{
				Name:        "regex-file",
				Usage:       "Regular expressions to find file",
				Value:       internal.RegexFile,
				DefaultText: internal.RegexFile,
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}

	if internal.StatusCode != 0 {
		os.Exit(internal.StatusCode)
	}
}
